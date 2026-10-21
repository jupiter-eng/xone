package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xflow"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xredis"
)

// order is the request to place an order, and the data shared across the three steps. The three
// injection fields are all a step number (1..3), with 0 injecting nothing:
//
//	fail_at           this step's Process returns an error
//	panic_at          this step's Process panics
//	rollback_fail_at  this step's Rollback returns an error
type order struct {
	ID             string `json:"id"` // generated when left empty
	UserID         int64  `json:"user_id"`
	Amount         int64  `json:"amount"`
	FailAt         int    `json:"fail_at"`
	PanicAt        int    `json:"panic_at"`
	RollbackFailAt int    `json:"rollback_fail_at"`
}

// orderFlow has three steps, and each one's side effect lands where a test can find it:
//
//	1 create   inserts a row into the PG order table (status=created)   rollback: deletes that row
//	2 charge   Redis SET {prefix}order:{id}:charged                     rollback: DEL
//	3 confirm  sets status to confirmed in PG                           rollback: back to created
//
// Every injected fault happens before the side effect: the failing step did nothing itself, and by
// xflow's rule it is not rolled back either -- what is rolled back are the steps before it that
// succeeded
var orderFlow = xflow.New[*order]("create_order", createStep{}, chargeStep{}, confirmStep{})

// inject injects the fault at step step as the request asks
func (o *order) inject(step int) error {
	if o.PanicAt == step {
		panic(fmt.Sprintf("injected panic at step %d", step))
	}
	if o.FailAt == step {
		return fmt.Errorf("injected failure at step %d", step)
	}
	return nil
}

func (o *order) injectRollback(step int) error {
	if o.RollbackFailAt == step {
		return fmt.Errorf("injected rollback failure at step %d", step)
	}
	return nil
}

func chargedKey(id string) string { return conf.C().KeyPrefix + "order:" + id + ":charged" }

type createStep struct{}

func (createStep) Name() string { return "create" }

func (createStep) Process(ctx context.Context, o *order) error {
	if err := o.inject(1); err != nil {
		return err
	}
	return store.InsertOrder(ctx, o.ID, o.UserID, o.Amount)
}

func (createStep) Rollback(ctx context.Context, o *order) error {
	if err := o.injectRollback(1); err != nil {
		return err
	}
	return store.DeleteOrder(ctx, o.ID)
}

type chargeStep struct{}

func (chargeStep) Name() string { return "charge" }

func (chargeStep) Process(ctx context.Context, o *order) error {
	if err := o.inject(2); err != nil {
		return err
	}
	return xredis.C().Set(ctx, chargedKey(o.ID), o.Amount, 0).Err()
}

func (chargeStep) Rollback(ctx context.Context, o *order) error {
	if err := o.injectRollback(2); err != nil {
		return err
	}
	return xredis.C().Del(ctx, chargedKey(o.ID)).Err()
}

type confirmStep struct{}

func (confirmStep) Name() string { return "confirm" }

func (confirmStep) Process(ctx context.Context, o *order) error {
	if err := o.inject(3); err != nil {
		return err
	}
	return store.SetOrderStatus(ctx, o.ID, "confirmed")
}

func (confirmStep) Rollback(ctx context.Context, o *order) error {
	if err := o.injectRollback(3); err != nil {
		return err
	}
	return store.SetOrderStatus(ctx, o.ID, "created")
}

// createOrder runs orderFlow once. Success answers 201 and failure 500, both carrying id, and a
// failure carries error, rolled and rollback_errors besides, which a test checks against PG /
// Redis to see whether the compensation ran
func createOrder(c *gin.Context) {
	ctx := c.Request.Context()
	var o order
	if err := c.ShouldBindJSON(&o); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if o.ID == "" {
		o.ID = "o-" + randomHex(8)
	}

	done := xmetric.Timer("order_flow_seconds")
	res := orderFlow.Execute(ctx, &o)
	done()

	body := gin.H{"id": o.ID, "success": res.Success(), "rolled": res.Rolled}
	result := "success"
	if !res.Success() {
		result = "failed"
		body["error"] = res.Err.Error()
	}
	if len(res.RollbackErrors) > 0 {
		errs := make([]string, len(res.RollbackErrors))
		for i, e := range res.RollbackErrors {
			errs[i] = e.Error()
		}
		body["rollback_errors"] = errs
	}
	xmetric.CounterInc("orders_total", xmetric.T("result", result))
	slog.InfoContext(ctx, "order processed", "order_id", o.ID, "result", result, "rolled", res.Rolled)

	status := http.StatusCreated
	if !res.Success() {
		status = http.StatusInternalServerError
	}
	c.JSON(status, body)
}
