package xflow

import (
	"context"
	"errors"
	"testing"
)

// The steps below write only what has to be written: Process and Rollback.

type DeductCoupon struct{ err error }

func (s *DeductCoupon) Process(context.Context, *struct{}) error  { return s.err }
func (s *DeductCoupon) Rollback(context.Context, *struct{}) error { return nil }

type ReserveStock struct{ rolled bool }

func (s *ReserveStock) Process(context.Context, *struct{}) error  { return nil }
func (s *ReserveStock) Rollback(context.Context, *struct{}) error { s.rolled = true; return nil }

// Notify writes both optional methods.
type Notify struct{}

func (*Notify) Name() string                              { return "notify" }
func (*Notify) Dependency() Dependency                    { return Weak }
func (*Notify) Process(context.Context, *struct{}) error  { return errors.New("sms gateway timeout") }
func (*Notify) Rollback(context.Context, *struct{}) error { return nil }

func TestNew_UsesTypeNameWhenNameUnset(t *testing.T) {
	// The name is already written on the type, and asking the caller for a method returning the same string is pure duplication.
	res := New[*struct{}]("place-order", &DeductCoupon{err: errors.New("coupon used up")}).Execute(context.Background(), &struct{}{})

	var se *StepError
	if !errors.As(res.Err, &se) {
		t.Fatalf("the failing step must appear in the error, got=%v", res.Err)
	}
	if se.Processor != "DeductCoupon" {
		t.Errorf("without a Name the type name must be used, got=%q", se.Processor)
	}
}

func TestNew_StrongDependencyWhenDependencyUnset(t *testing.T) {
	// The default has to err on the safe side: a failure interrupts and rolls back rather than quietly skipping on and charging the card.
	stock := &ReserveStock{}
	res := New[*struct{}]("place-order", stock, &DeductCoupon{err: errors.New("coupon used up")}).
		Execute(context.Background(), &struct{}{})

	if res.Success() {
		t.Fatal("a step without a Dependency failing must interrupt the flow")
	}
	if !stock.rolled {
		t.Error("a failing strong dependency must roll back the steps that already succeeded")
	}
}

func TestNew_UsesNameAndDependencyWhenSet(t *testing.T) {
	res := New[*struct{}]("place-order", &Notify{}).Execute(context.Background(), &struct{}{})

	if !res.Success() {
		t.Fatalf("a failing weak dependency must not fail the flow, got=%v", res.Err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Processor != "notify" {
		t.Errorf("it must be recorded in Skipped under the name written, got=%+v", res.Skipped)
	}
}
