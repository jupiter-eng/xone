// Package xflow orchestrates a series of steps into one flow, rolling them back in reverse when a strong dependency fails.
//
//	flow := xflow.New("place-order",
//		&DeductCoupon{}, &ReserveStock{}, &Charge{}, &Notify{},
//	)
//	result := flow.Execute(ctx, data)
//	if !result.Success() { ... }
//
// A step only has to write Process and Rollback. The default is a strong dependency: a failure interrupts the flow and
// rolls it back; for a weak dependency (a failure is only recorded and the flow continues) add a Dependency() returning Weak.
//
// This package has no third-party dependencies, which is why it stays in the core module.
package xflow

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"runtime/debug"
	"time"

	"github.com/jupiter-eng/xone/xerror"
)

// Dependency is how strongly a step is depended on.
type Dependency int

const (
	// Strong is a strong dependency: a failure interrupts the flow and rolls it back.
	Strong Dependency = iota
	// Weak is a weak dependency: a failure is only recorded and the flow continues.
	Weak
)

func (d Dependency) String() string {
	switch d {
	case Strong:
		return "strong"
	case Weak:
		return "weak"
	default:
		return "unknown"
	}
}

// Processor is one step in a flow.
//
// The type parameter T is the data shared across the whole flow, and a pointer (such as *OrderData) is the natural
// choice: the inputs, the outputs and the intermediate results between steps all go into that one struct, so each
// step's method signatures mention only the business's own types and never repeat the framework's generics.
//
// Process and Rollback belong on the same type: whoever did something is responsible for undoing it.
// Splitting them apart ends sooner or later in "a forward step was added and its compensation forgotten".
//
// There are two more optional methods, used when written and read once while the flow is built:
//
//	Name() string            the step name, for logs and error messages. Without it the type name is used: &DeductCoupon{} is called "DeductCoupon"
//	Dependency() Dependency  a strong or a weak dependency. Without it, Strong
type Processor[T any] interface {
	// Process is the forward logic.
	Process(ctx context.Context, data T) error

	// Rollback is the compensation logic, called in reverse on the steps that succeeded when a strong dependency fails.
	//
	// Two things to keep in mind:
	//   - It cannot assume Process fully succeeded. A failed weak dependency is still rolled back,
	//     so this has to be idempotent.
	//   - The ctx it receives has had the original cancellation and timeout stripped off and is limited by
	//     RollbackTimeout instead (see rollback's documentation). One that has not returned when the budget runs out is
	//     no longer waited for and is recorded as a timeout, but its goroutine keeps running and may still read and write data -- watch the ctx and do not leave it dangling.
	Rollback(ctx context.Context, data T) error
}

// StepError is an error from one step.
type StepError struct {
	Processor  string
	Dependency Dependency
	Err        error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step %q (%s dependency) failed: %v", e.Processor, e.Dependency, e.Err)
}

func (e *StepError) Unwrap() error { return e.Err }

// PanicError means a step's Process or Rollback panicked; it appears in StepError.Err.
//
// The call stack goes into Stack rather than into the error message: the message reaches alert titles and log search,
// and a stack of a few dozen lines would split one error into a few dozen lines. The default monitor records it as a
// separate stack field; an own Monitor implementation takes it with errors.As.
type PanicError struct {
	Value any    // the value recover() returned
	Stack []byte // the call stack at the moment of the panic
}

func (e *PanicError) Error() string { return fmt.Sprintf("panicked: %v", e.Value) }

// Unwrap returns the panicked value when it is itself an error, so errors.Is / errors.As can reach the root cause.
func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// Result is the outcome of one execution.
//
// The business data is not here -- it is in the T the caller passed to Execute.
// When the flow fails, what the earlier steps wrote into it is kept, for investigation and manual compensation.
type Result struct {
	// Err is the fatal error: a strong dependency failed, or the ctx was cancelled. nil means the flow ran to the end.
	Err error

	// Skipped records the failed weak dependencies, which do not stop the flow.
	Skipped []*StepError

	// RollbackErrors are the errors that occurred during the rollback.
	//
	// A non-empty list means some resource was not compensated and needs a human.
	RollbackErrors []*StepError

	// Rolled says whether at least one step was really rolled back.
	//
	// A flow cancelled before the first step began has nothing to roll back, so this is false.
	Rolled bool
}

// Success reports whether the flow ran to the end (with no strong dependency failing).
func (r *Result) Success() bool { return r.Err == nil }

func (r *Result) String() string {
	if r.Err == nil {
		if len(r.Skipped) > 0 {
			return fmt.Sprintf("flow succeeded, %d weak step(s) skipped after failing", len(r.Skipped))
		}
		return "flow succeeded"
	}
	msg := fmt.Sprintf("flow failed: %v", r.Err)
	if r.Rolled {
		msg += ", rolled back"
	}
	if n := len(r.RollbackErrors); n > 0 {
		msg += fmt.Sprintf(", %d step(s) failed to roll back", n)
	}
	return msg
}

// Flow is one orchestrated flow.
//
// Its fields do not change after it is built, so Execute may be called concurrently.
type Flow[T any] struct {
	name  string
	steps []entry[T]

	// rollbackTimeout is this flow's own rollback budget; 0 means it follows XFlow.RollbackTimeout
	rollbackTimeout time.Duration
}

// entry is one step together with the name and dependency strength fixed when it was built.
type entry[T any] struct {
	Processor[T]
	name string
	dep  Dependency
}

// entryOf reads a step's optional Name / Dependency, using the defaults for whatever is not written.
func entryOf[T any](p Processor[T]) entry[T] {
	s := entry[T]{Processor: p, name: typeName(p), dep: Strong}
	if n, ok := p.(interface{ Name() string }); ok {
		s.name = n.Name()
	}
	if d, ok := p.(interface{ Dependency() Dependency }); ok {
		s.dep = d.Dependency()
	}
	return s
}

// fail records an error from this step as a StepError.
func (s entry[T]) fail(err error) *StepError {
	return &StepError{Processor: s.name, Dependency: s.dep, Err: err}
}

// typeName gives a step without a Name its type name: &DeductCoupon{} -> DeductCoupon
func typeName(p any) string {
	t := reflect.TypeOf(p)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n := t.Name(); n != "" {
		return n
	}
	return t.String()
}

// New builds a flow in the order the steps are passed.
//
// A nil step panics outright: it would otherwise only blow up as a nil pointer when that step runs, by which time the
// error is long detached from where it was built, and the earlier it surfaces the better.
func New[T any](name string, steps ...Processor[T]) *Flow[T] {
	out := make([]entry[T], len(steps))
	for i, p := range steps {
		if p == nil {
			panic(fmt.Sprintf("xflow: step %d of flow %q is nil", i, name))
		}
		out[i] = entryOf(p)
	}
	return &Flow[T]{name: name, steps: out}
}

// Name is the flow name.
func (f *Flow[T]) Name() string { return f.name }

// WithRollbackTimeout sets this flow's own total rollback budget, overriding XFlow.RollbackTimeout from the configuration.
//
// When using xflow on its own without xone.Run, this is how the rollback budget is changed: the config file is only
// read when the framework starts. It returns a new Flow and leaves the original untouched, so a flow already being
// Executed concurrently elsewhere is unaffected. d must be positive: 0 would make a rollback time out the moment it starts and skip every compensation, so passing it panics.
func (f *Flow[T]) WithRollbackTimeout(d time.Duration) *Flow[T] {
	if d <= 0 {
		panic(fmt.Sprintf("xflow: rollback timeout of flow %q must be > 0, got=%v", f.name, d))
	}
	g := *f
	g.rollbackTimeout = d
	return &g
}

// Execute runs the steps in order, with data carried through for every step to read and write.
//
// A failing strong dependency or a cancelled ctx interrupts the flow and rolls the executed steps back in reverse;
// a failing weak dependency is recorded in Skipped and the flow continues, but it is included in the rollback range
// all the same -- it may already have had side effects and merely did not carry on.
func (f *Flow[T]) Execute(ctx context.Context, data T) *Result {
	if ctx == nil {
		ctx = context.Background()
	}

	m := activeMonitor()
	res := &Result{}
	start := now(m)

	// A defer rather than writing it at each exit: there are three exits (cancelled, failed, ran to the end), and
	// missing one when another branch is added later would show up as this execution vanishing from monitoring while
	// the flow itself returns as usual -- nobody would notice.
	defer func() { f.notifyFlow(ctx, m, res, start) }()

	// What is rolled back is always the executed prefix: the successful steps and the failed weak dependencies are all
	// in it, and only a failed strong dependency is not -- and that is necessarily the last one executed. So a count is enough.
	n := 0

	for _, s := range f.steps {
		// The caller is no longer waiting, so no new step is started; what is already done is still rolled back.
		if err := ctx.Err(); err != nil {
			res.Err = xerror.Newf("xflow", "execute", "flow %q canceled before step %q: %w", f.name, s.name, err)
			f.rollback(ctx, data, f.steps[:n], res, m)
			return res
		}

		stepStart := now(m)
		err := safeProcess(ctx, s.Processor, data)
		f.notifyStep(ctx, m, false, s, err, stepStart)

		if err == nil {
			n++
			continue
		}

		se := s.fail(err)

		if s.dep == Weak {
			res.Skipped = append(res.Skipped, se)
			n++ // a failed weak dependency may have left side effects too and is rolled back as well

			// A failing weak dependency is only recorded and the flow continues -- unless it was dragged down by a cancellation.
			//
			// Checking for cancellation only at the start of each step would miss a cancellation hitting the last step:
			// its context.Canceled would walk into this "skipped" branch, the loop would end right after, and a cancelled
			// flow would be reported as Success with not a single step rolled back.
			if ctx.Err() == nil {
				continue
			}
			res.Err = xerror.Newf("xflow", "execute", "flow %q canceled while running step %q: %w",
				f.name, s.name, ctx.Err())
		} else {
			// A failing strong dependency: this step did not succeed and is not in the rollback range.
			//
			// It is wrapped in an xerror: without that, res.Err would be a bare *StepError that
			// xerror.Is(err, "xflow") does not recognize, while every other error is recognized.
			// With it, errors.As(err, &stepErr) still reaches the one inside.
			res.Err = xerror.New("xflow", "execute", se)
		}

		f.rollback(ctx, data, f.steps[:n], res, m)
		return res
	}
	return res
}

// rollback rolls the executed steps back in reverse.
//
// The ctx used for the rollback has the original cancellation and timeout stripped off: compensation logic (a refund,
// returning stock, unfreezing a limit) is needed most precisely after a request has timed out, and inheriting an
// already cancelled ctx would have every compensation call rejected the moment it starts, really leaking the resources.
// With them stripped off it is limited by RollbackTimeout instead, so compensation cannot hang the shutdown sequence indefinitely.
func (f *Flow[T]) rollback(ctx context.Context, data T, done []entry[T], res *Result, m Monitor) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(f.rollbackTimeout, cfg.RollbackTimeout))
	defer cancel()

	for i := len(done) - 1; i >= 0; i-- {
		s := done[i]

		// Even once the budget is exhausted the rest are recorded one by one: the caller has to know which resources are still dangling.
		if err := rbCtx.Err(); err != nil {
			res.RollbackErrors = append(res.RollbackErrors,
				s.fail(fmt.Errorf("rollback budget exhausted, this step never ran: %w", err)))
			continue
		}

		res.Rolled = true
		stepStart := now(m)
		err := rollbackWithin(rbCtx, s.Processor, data)
		f.notifyStep(rbCtx, m, true, s, err, stepStart)

		if err != nil {
			res.RollbackErrors = append(res.RollbackErrors, s.fail(err))
		}
	}
}

// rollbackWithin runs one step's Rollback within the rollback budget and stops waiting for it once the budget is reached.
//
// Rollback receives a ctx, but it may not really watch it -- what is inside may be a third-party call that ignores it.
// Checking the budget only between steps would let such a step hang Execute indefinitely (measured: a 50ms budget
// waited 2s) without appearing in RollbackErrors itself. So another goroutine does the waiting, the same way as xone's
// runWithin. After the timeout that goroutine is still sitting there and still reads and writes data, which is
// deliberate: abandoning it outright beats having Execute, and the caller with it, hang alongside it.
// The budget is exhausted by then, so rollback records every step that never got a turn as "not executed".
func rollbackWithin[T any](ctx context.Context, p Processor[T], data T) error {
	done := make(chan error, 1)
	go func() { done <- safeRollback(ctx, p, data) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("rollback did not finish within the rollback budget, abandoned while still running: %w", ctx.Err())
	}
}

// safeProcess runs Process and isolates panics.
//
// One step blowing up must not take the whole process down: it should become an ordinary step failure, so the earlier steps get a chance to be rolled back.
//
// It returns a *PanicError rather than an xerror: that goes into a StepError, which Execute wraps in an xerror at the
// module boundary. Wrapping here as well would be two layers of xflow.
func safeProcess[T any](ctx context.Context, p Processor[T], data T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return p.Process(ctx, data)
}

// safeRollback runs Rollback and isolates panics.
//
// This matters especially: one compensation blowing up must not stop the others from running.
func safeRollback[T any](ctx context.Context, p Processor[T], data T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return p.Rollback(ctx, data)
}
