package xflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recorder records the events it receives.
type recorder struct {
	mu    sync.Mutex
	steps []*StepEvent
	flows []*FlowEvent
	panic bool
}

func (r *recorder) OnStep(_ context.Context, e *StepEvent) {
	if r.panic {
		panic("the monitor blew up")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, e)
}

func (r *recorder) OnFlow(_ context.Context, e *FlowEvent) {
	if r.panic {
		panic("the monitor blew up")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flows = append(r.flows, e)
}

func (r *recorder) stepEvents() []*StepEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*StepEvent(nil), r.steps...)
}

// withMonitor installs a monitoring implementation and restores the previous one when the test ends.
func withMonitor(t *testing.T, m Monitor) {
	t.Helper()
	old := monitor.Load()
	t.Cleanup(func() { monitor.Store(old) })
	SetMonitor(m)
}

func TestMonitor_ReceivesEveryStepAndFlowEvent(t *testing.T) {
	withConfig(t, nil)
	r := &recorder{}
	withMonitor(t, r)

	New("place-order", ok("deduct-coupon"), failing("charge", Strong)).Execute(context.Background(), &data{})

	steps := r.stepEvents()
	// Two Processes (one success, one failure) plus one Rollback.
	if len(steps) != 3 {
		t.Fatalf("3 step events must arrive, got=%d", len(steps))
	}
	if steps[0].Rollback || steps[0].Err != nil || steps[0].Processor != "deduct-coupon" {
		t.Errorf("the first event is wrong, got=%+v", steps[0])
	}
	if steps[1].Err == nil || steps[1].Processor != "charge" {
		t.Errorf("a failing step must carry the error, got=%+v", steps[1])
	}
	if !steps[2].Rollback || steps[2].Processor != "deduct-coupon" {
		t.Errorf("the third event must be the rollback, got=%+v", steps[2])
	}

	if len(r.flows) != 1 {
		t.Fatalf("one flow event must arrive, got=%d", len(r.flows))
	}
	if r.flows[0].Flow != "place-order" || r.flows[0].Result.Success() {
		t.Errorf("the flow event is wrong, got=%+v", r.flows[0])
	}
	// The flow duration has to include the rollback, otherwise "how long did this flow take" is a lie.
	if r.flows[0].Duration <= 0 {
		t.Error("the flow event must carry the duration")
	}
}

func TestMonitor_PanicDoesNotInterruptFlow(t *testing.T) {
	// A failing monitoring implementation must only lose one observation.
	withConfig(t, nil)
	withMonitor(t, &recorder{panic: true})

	d := &data{}
	res := New("place-order", ok("deduct-coupon"), ok("charge")).Execute(context.Background(), d)

	if !res.Success() {
		t.Errorf("a panicking monitor must not fail the flow: %v", res)
	}
	if len(d.doneList()) != 2 {
		t.Errorf("the flow must run to the end as usual, got=%v", d.doneList())
	}
}

func TestMonitor_ZeroCallbacksWhenDisabled(t *testing.T) {
	withConfig(t, func(c *Config) { c.Monitor = false })
	r := &recorder{}
	withMonitor(t, r)

	New("place-order", ok("deduct-coupon")).Execute(context.Background(), &data{})

	if len(r.stepEvents()) != 0 || len(r.flows) != 0 {
		t.Error("with monitoring off there must be no callback at all")
	}
}

func TestSetMonitor_NilDisablesMonitor(t *testing.T) {
	withConfig(t, nil)
	withMonitor(t, nil)

	if activeMonitor() != nil {
		t.Error("passing nil must turn monitoring off")
	}
	// It must not panic.
	New("place-order", ok("deduct-coupon")).Execute(context.Background(), &data{})
}

func TestMonitor_StepDuration(t *testing.T) {
	withConfig(t, nil)
	r := &recorder{}
	withMonitor(t, r)

	New("place-order", &step{name: "slow", dep: Strong, delay: 20 * time.Millisecond}).
		Execute(context.Background(), &data{})

	steps := r.stepEvents()
	if len(steps) != 1 || steps[0].Duration < 15*time.Millisecond {
		t.Errorf("the step duration is wrong, got=%v", steps)
	}
}

func TestSlogMonitor_DoesNotPanic(t *testing.T) {
	// The default implementation, run once to cover every branch.
	withConfig(t, nil)
	withMonitor(t, slogMonitor{})

	ctx := context.Background()
	New("success", ok("a")).Execute(ctx, &data{})
	New("failure", failing("a", Strong)).Execute(ctx, &data{})
	New("rollback with a failure", &step{name: "compensation-failed", dep: Strong, rbErr: errors.New("x")}, failing("b", Strong)).
		Execute(ctx, &data{})
}
