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

// order 下单请求，也是贯穿三步的共享数据。三个注入字段都是步骤序号（1..3），0 表示不注入：
//
//	fail_at           这一步的 Process 返回错误
//	panic_at          这一步的 Process panic
//	rollback_fail_at  这一步的 Rollback 返回错误
type order struct {
	ID             string `json:"id"` // 留空时生成一个
	UserID         int64  `json:"user_id"`
	Amount         int64  `json:"amount"`
	FailAt         int    `json:"fail_at"`
	PanicAt        int    `json:"panic_at"`
	RollbackFailAt int    `json:"rollback_fail_at"`
}

// orderFlow 三步，每一步的副作用都落在测试查得到的地方：
//
//	1 create   PG 订单表插一行（status=created）   回滚：删掉这一行
//	2 charge   Redis SET {prefix}order:{id}:charged  回滚：DEL
//	3 confirm  PG 里 status 改成 confirmed          回滚：改回 created
//
// 注入的故障都发生在副作用之前：失败的那一步自己什么都没做，
// 按 xflow 的规矩它也不会被回滚，被回滚的只有它前面成功了的那几步
var orderFlow = xflow.New[*order]("create_order", createStep{}, chargeStep{}, confirmStep{})

// inject 按请求在第 step 步注入故障
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

// createOrder 跑一遍 orderFlow。成功 201，失败 500；两种情况都带上 id，
// 失败时另有 error、rolled、rollback_errors，测试据此去 PG / Redis 核对补偿做没做
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
