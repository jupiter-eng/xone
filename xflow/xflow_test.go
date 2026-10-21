package xflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
)

// TestMain discards the flow logs: these cases fill a screen by design.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// data is the data shared across a whole flow.
type data struct {
	mu   sync.Mutex
	done []string // the steps executed forward
	back []string // the steps rolled back
}

func (d *data) record(list *[]string, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	*list = append(*list, name)
}

func (d *data) doneList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.done...)
}

func (d *data) backList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.back...)
}

// step is one orchestratable test step.
type step struct {
	name     string
	dep      Dependency
	err      error // returned by Process
	rbErr    error // returned by Rollback
	panics   bool
	rbPanics bool
	delay    time.Duration
	rbDelay  time.Duration
}

func (s *step) Name() string           { return s.name }
func (s *step) Dependency() Dependency { return s.dep }

func (s *step) Process(ctx context.Context, d *data) error {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.panics {
		panic("the step blew up: " + s.name)
	}
	if s.err != nil {
		return s.err // done records only the successes, which keeps the assertions readable
	}
	d.record(&d.done, s.name)
	return nil
}

func (s *step) Rollback(ctx context.Context, d *data) error {
	if s.rbDelay > 0 {
		time.Sleep(s.rbDelay)
	}
	if s.rbPanics {
		panic("the rollback blew up: " + s.name)
	}
	d.record(&d.back, s.name)
	return s.rbErr
}

func ok(name string) *step   { return &step{name: name, dep: Strong} }
func weak(name string) *step { return &step{name: name, dep: Weak} }

func failing(name string, dep Dependency) *step {
	return &step{name: name, dep: dep, err: errors.New(name + " failed")}
}

// withConfig swaps in another configuration and restores the previous one when the test ends.
// The handle is read-only, so swapping the configuration means replacing the package-level handle with a Fixed one.
func withConfig(t *testing.T, mutate func(*Config)) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
	c := DefaultConfig()
	if mutate != nil {
		mutate(&c)
	}
	cfg = c
}

func TestExecute_AllSucceed(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("place-order", ok("deduct-coupon"), ok("reserve-stock"), ok("charge")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("it must succeed: %v", res)
	}
	if res.Rolled {
		t.Error("a successful flow must not roll back")
	}
	if got := d.doneList(); len(got) != 3 || got[0] != "deduct-coupon" || got[2] != "charge" {
		t.Errorf("they must run in order, got=%v", got)
	}
	if len(d.backList()) != 0 {
		t.Errorf("nothing may be rolled back, got=%v", d.backList())
	}
}

func TestExecute_StrongDependencyFailureRollsBackInReverse(t *testing.T) {
	// "whoever did something is responsible for undoing it", and the undo order has to be the reverse of the run order.
	withConfig(t, nil)
	d := &data{}
	res := New("place-order",
		ok("deduct-coupon"), ok("reserve-stock"), failing("charge", Strong), ok("notify"),
	).Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("the flow must fail when a strong dependency fails")
	}
	if !res.Rolled {
		t.Error("a rollback must be triggered")
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("the steps after the failure must not run, got=%v", got)
	}
	if got := d.backList(); len(got) != 2 || got[0] != "reserve-stock" || got[1] != "deduct-coupon" {
		t.Errorf("the rollback must run in reverse, got=%v", got)
	}

	var se *StepError
	if !errors.As(res.Err, &se) || se.Processor != "charge" {
		t.Errorf("the error must name which step it was, got=%v", res.Err)
	}
}

func TestExecute_WeakDependencyFailureContinues(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("place-order", ok("deduct-coupon"), failing("notify", Weak), ok("write-log")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("a failing weak dependency must not interrupt the flow: %v", res)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Processor != "notify" {
		t.Errorf("the skipped error must be recorded, got=%v", res.Skipped)
	}
	if got := d.doneList(); len(got) != 2 || got[1] != "write-log" {
		t.Errorf("the later steps must keep running, got=%v", got)
	}
}

func TestExecute_FailedWeakDependencyIsAlsoRolledBack(t *testing.T) {
	// It may already have had side effects and merely did not carry on.
	withConfig(t, nil)
	d := &data{}
	New("place-order", ok("deduct-coupon"), failing("notify", Weak), failing("charge", Strong)).
		Execute(context.Background(), d)

	got := d.backList()
	if len(got) != 2 || got[0] != "notify" || got[1] != "deduct-coupon" {
		t.Errorf("a failed weak dependency must be in the rollback range too, got=%v", got)
	}
}

func TestExecute_CtxCanceledStartsNoNewStepsButStillRollsBack(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	ctx, cancel := context.WithCancel(context.Background())

	cancelStep := &step{name: "cancel", dep: Strong}
	res := New("place-order", ok("deduct-coupon"), &cancelHook{step: cancelStep, cancel: cancel}, ok("charge")).
		Execute(ctx, d)

	if res.Success() {
		t.Fatal("the flow must fail once the ctx is cancelled")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("the error must make the cancellation visible, got=%v", res.Err)
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("no new step may start after cancellation, got=%v", got)
	}
	if got := d.backList(); len(got) != 2 {
		t.Errorf("what is already done is rolled back as usual, got=%v", got)
	}
}

// cancelHook cancels the ctx while it runs.
type cancelHook struct {
	*step
	cancel context.CancelFunc
}

func (c *cancelHook) Process(ctx context.Context, d *data) error {
	err := c.step.Process(ctx, d)
	c.cancel()
	return err
}

func TestRollback_UsesCtxWithoutCancellation(t *testing.T) {
	// Compensation logic is needed most precisely after a request has timed out.
	// Inheriting an already cancelled ctx would have every compensation call rejected the moment it starts, really leaking the resources.
	withConfig(t, nil)
	d := &data{}

	var rbCtxErr error
	checker := &ctxChecker{step: &step{name: "check", dep: Strong}, got: &rbCtxErr}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled from the start

	New("place-order", checker, ok("this-step-never-runs")).Execute(ctx, d)

	if rbCtxErr != nil {
		t.Errorf("the ctx used for the rollback must not be a cancelled one, got=%v", rbCtxErr)
	}
}

// ctxChecker records the ctx state during the rollback.
type ctxChecker struct {
	*step
	got *error
}

func (c *ctxChecker) Rollback(ctx context.Context, d *data) error {
	*c.got = ctx.Err()
	return c.step.Rollback(ctx, d)
}

func TestRollback_CtxValuesPreserved(t *testing.T) {
	// What is stripped off is cancellation and timeouts, not values: compensation logic often needs the tenant or the trace identifiers from the ctx.
	withConfig(t, nil)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "tenant-a")

	var seen any
	checker := &valueChecker{step: &step{name: "check", dep: Strong}, key: key{}, got: &seen}
	New("place-order", checker, failing("charge", Strong)).Execute(ctx, &data{})

	if seen != "tenant-a" {
		t.Errorf("a value in the ctx must still be readable during the rollback, got=%v", seen)
	}
}

type valueChecker struct {
	*step
	key any
	got *any
}

func (c *valueChecker) Rollback(ctx context.Context, d *data) error {
	*c.got = ctx.Value(c.key)
	return c.step.Rollback(ctx, d)
}

func TestRollback_RecordsRemainingStepsWhenBudgetExhausted(t *testing.T) {
	// The caller has to know which resources are still dangling, otherwise the only option is guessing from the logs.
	withConfig(t, func(c *Config) { c.RollbackTimeout = 30 * time.Millisecond })
	d := &data{}

	slow := &step{name: "slow-compensation", dep: Strong, rbDelay: 60 * time.Millisecond}
	res := New("place-order", ok("step-one"), ok("step-two"), slow, failing("charge", Strong)).
		Execute(context.Background(), d)

	if len(res.RollbackErrors) == 0 {
		t.Fatal("when the budget runs out, what was not compensated must be recorded")
	}
	var names []string
	for _, e := range res.RollbackErrors {
		names = append(names, e.Processor)
	}
	if !strings.Contains(strings.Join(names, ","), "step-one") {
		t.Errorf("a step that never got a turn must be recorded too, got=%v", names)
	}
	if !strings.Contains(res.String(), "failed to roll back") {
		t.Errorf("the result summary must show that the rollback did not finish, got=%s", res.String())
	}
}

func TestWithRollbackTimeout_OverridesConfigForThisFlow_OriginalUnchanged(t *testing.T) {
	// When using xflow on its own without xone.Run, nobody reads the config file, so this is the only way to change the rollback budget.
	withConfig(t, nil) // the configuration holds the default 30s
	slow := &step{name: "slow-compensation", dep: Strong, rbDelay: 60 * time.Millisecond}
	base := New("place-order", ok("step-one"), slow, failing("charge", Strong))
	short := base.WithRollbackTimeout(20 * time.Millisecond)

	if res := short.Execute(context.Background(), &data{}); len(res.RollbackErrors) == 0 {
		t.Error("with a 20ms rollback budget and a 60ms compensation, the unfinished compensation must be recorded")
	}
	// What comes back is a new flow: the original still follows the configured 30s and compensates everything.
	if res := base.Execute(context.Background(), &data{}); len(res.RollbackErrors) != 0 {
		t.Errorf("the original flow must not be changed, got=%v", res.RollbackErrors)
	}
}

func TestWithRollbackTimeout_PanicsOnNonPositive(t *testing.T) {
	// 0 would make a rollback time out the moment it starts and skip every compensation, while the flow looks entirely normal.
	for _, d := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithRollbackTimeout(%v) must panic", d)
				}
			}()
			New("place-order", ok("step-one")).WithRollbackTimeout(d)
		}()
	}
}

func TestRollback_CompensationIgnoringCtxCannotHangExecute(t *testing.T) {
	// Checking the budget only between steps would let a Rollback that ignores the ctx hang Execute:
	// measured, a 50ms budget waited 2s, and the hanging step was not in RollbackErrors.
	withConfig(t, func(c *Config) { c.RollbackTimeout = 50 * time.Millisecond })
	d := &data{}

	// It hangs for 3s: if it really hung, Execute would wait at least 3s, and the 1.5s bound is far from both the 50ms budget and the 3s.
	hung := &step{name: "compensation-ignoring-ctx", dep: Strong, rbDelay: 3 * time.Second}
	start := time.Now()
	res := New("place-order", ok("step-one"), hung, failing("charge", Strong)).Execute(context.Background(), d)

	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("the rollback budget was 50ms but Execute waited %v", took)
	}
	if len(res.RollbackErrors) != 2 {
		t.Fatalf("both the hanging step and the one that never got a turn must be recorded, got=%v", res.RollbackErrors)
	}
	if e := res.RollbackErrors[0]; e.Processor != "compensation-ignoring-ctx" || !errors.Is(e, context.DeadlineExceeded) {
		t.Errorf("the hanging step must be recorded as a timeout, got=%v", e)
	}
	if e := res.RollbackErrors[1]; e.Processor != "step-one" {
		t.Errorf("a step that never got a turn must be recorded too, got=%v", e)
	}
}

func TestExecute_CanceledBeforeFirstStepIsNotRolledBack(t *testing.T) {
	// With no step executed there is nothing to roll back: reporting rolled back would send someone looking for a compensation that never happened.
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := New("place-order", ok("deduct-coupon")).Execute(ctx, &data{})

	if res.Success() {
		t.Fatal("the flow must fail once the ctx is cancelled")
	}
	if res.Rolled {
		t.Error("with not a single step executed, Rolled must not be true")
	}
	if strings.Contains(res.String(), "rolled back") {
		t.Errorf("the summary must not say it rolled back, got=%s", res.String())
	}
}

func TestRollback_OneFailureDoesNotBlockOtherSteps(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	bad := &step{name: "compensation-fails", dep: Strong, rbErr: errors.New("compensation failed")}

	res := New("place-order", ok("step-one"), bad, failing("charge", Strong)).Execute(context.Background(), d)

	if len(res.RollbackErrors) != 1 {
		t.Errorf("that step's rollback error must be recorded, got=%v", res.RollbackErrors)
	}
	// Both compensations ran: one failing must not stop the other.
	if got := d.backList(); len(got) != 2 {
		t.Errorf("the other steps must be rolled back as usual, got=%v", got)
	}
}

func TestExecute_StepPanicBecomesOrdinaryFailure(t *testing.T) {
	// One step blowing up must not take the whole process down, and the earlier steps must still get a chance to be rolled back.
	withConfig(t, nil)
	d := &data{}
	res := New("place-order", ok("deduct-coupon"), &step{name: "boom", dep: Strong, panics: true}).
		Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("a panic must become a flow failure")
	}
	if !strings.Contains(res.Err.Error(), "panic") {
		t.Errorf("the error must make the panic visible, got=%v", res.Err)
	}
	if got := d.backList(); len(got) != 1 || got[0] != "deduct-coupon" {
		t.Errorf("the earlier steps must still be rolled back, got=%v", got)
	}
}

// errPanicStep is a step that panics with an error value.
type errPanicStep struct{ err error }

func (errPanicStep) Name() string           { return "panics-with-error" }
func (errPanicStep) Dependency() Dependency { return Strong }
func (s errPanicStep) Process(context.Context, *data) error {
	panic(s.err)
}
func (errPanicStep) Rollback(context.Context, *data) error { return nil }

func TestExecute_StepPanicWrappedOnceAndKeepsErrorChain(t *testing.T) {
	// One module boundary, one xerror: wrapping the panic once in safeProcess and once again in Execute would nest the
	// text into xone xflow execute failed, err=[... xone xflow execute failed ...].
	// A panicked error has to be caught with %w so errors.Is can reach the root cause;
	// the call stack is dozens of lines and would wreck alert titles and log search if it went into the message.
	withConfig(t, nil)
	cause := errors.New("the connection pool blew up")
	res := New[*data]("place-order", errPanicStep{cause}).Execute(context.Background(), &data{})

	if res.Success() {
		t.Fatal("a panic must become a flow failure")
	}
	msg := res.Err.Error()
	if n := strings.Count(msg, "xone xflow"); n != 1 {
		t.Errorf("there must be only one layer of xflow error, got %d layers: %s", n, msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("the error message must not hold a multi-line call stack: %s", msg)
	}
	if !errors.Is(res.Err, cause) {
		t.Errorf("the panicked error must still be on the chain, got=%v", res.Err)
	}
	var pe *PanicError
	if !errors.As(res.Err, &pe) || !strings.Contains(string(pe.Stack), "goroutine") {
		t.Errorf("the call stack must hang on PanicError.Stack, got=%+v", pe)
	}
}

func TestMonitor_DefaultRecordsPanicStackAsSeparateField(t *testing.T) {
	// With the stack out of the error message, it has to be visible somewhere: the default monitor puts it in a stack field.
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("place-order", &step{name: "boom", dep: Strong, panics: true}).Execute(context.Background(), &data{})

	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "xflow step process failed") {
			line = l
		}
	}
	if !strings.Contains(line, `"stack":"goroutine`) {
		t.Errorf("the log of the failing step must carry a stack field, got=%s", line)
	}
}

func TestRollback_PanicDoesNotBlockOtherCompensations(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("place-order",
		ok("step-one"),
		&step{name: "compensation-blows-up", dep: Strong, rbPanics: true},
		failing("charge", Strong),
	).Execute(context.Background(), d)

	if len(res.RollbackErrors) != 1 {
		t.Errorf("that step's panic must be recorded, got=%v", res.RollbackErrors)
	}
	if got := d.backList(); len(got) != 1 || got[0] != "step-one" {
		t.Errorf("the other compensations must run as usual, got=%v", got)
	}
}

func TestNew_NilStepPanics(t *testing.T) {
	// It would otherwise only blow up as a nil pointer when that step runs, by which time the error is long detached from where it was built.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("passing a nil step must panic")
		}
		if !strings.Contains(fmt.Sprint(r), "step 1") {
			t.Errorf("it must say which step it was, got=%v", r)
		}
	}()
	New[*data]("place-order", ok("step-one"), nil)
}

func TestFlow_CanExecuteConcurrently(t *testing.T) {
	// The fields do not change after it is built.
	withConfig(t, nil)
	flow := New("place-order", ok("deduct-coupon"), weak("notify"), ok("charge"))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := flow.Execute(context.Background(), &data{}); !res.Success() {
				t.Errorf("concurrent executions must all succeed: %v", res)
			}
		}()
	}
	wg.Wait()
}

func TestExecute_nil_CtxDoesNotPanic(t *testing.T) {
	withConfig(t, nil)
	//nolint:staticcheck // nil is passed on purpose
	if res := New("place-order", ok("step-one")).Execute(nil, &data{}); !res.Success() {
		t.Errorf("a nil ctx must be handled, got=%v", res)
	}
}

func TestDependencyString(t *testing.T) {
	if Strong.String() != "strong" || Weak.String() != "weak" || Dependency(9).String() != "unknown" {
		t.Error("the dependency type text is wrong")
	}
}

func TestResultString(t *testing.T) {
	for _, c := range []struct {
		res  *Result
		want string
	}{
		{&Result{}, "flow succeeded"},
		{&Result{Skipped: []*StepError{{}}}, "1 weak step(s) skipped"},
		{&Result{Err: errors.New("boom")}, "flow failed"},
		{&Result{Err: errors.New("boom"), Rolled: true}, "rolled back"},
		{&Result{Err: errors.New("boom"), Rolled: true, RollbackErrors: []*StepError{{}}}, "1 step(s) failed to roll back"},
	} {
		if got := c.res.String(); !strings.Contains(got, c.want) {
			t.Errorf("the summary must contain %q, got=%q", c.want, got)
		}
	}
}

func TestStepError(t *testing.T) {
	cause := errors.New("the root cause")
	e := &StepError{Processor: "charge", Dependency: Strong, Err: cause}
	if !strings.Contains(e.Error(), "charge") || !strings.Contains(e.Error(), "strong") {
		t.Errorf("the error message must carry the step name and the dependency type, got=%s", e.Error())
	}
	if !errors.Is(e, cause) {
		t.Error("errors.Is must reach the root cause")
	}
}

func TestFlowName(t *testing.T) {
	if got := New[*data]("place-order").Name(); got != "place-order" {
		t.Errorf("Name is wrong, got=%q", got)
	}
}

// ---- configuration and registration ----

func TestConfig_LoadsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte("XFlow:\n  Monitor: false\n  RollbackTimeout: 5s\n"), 0o644)

	c := DefaultConfig()
	if err := config.LoadInto(path, ConfigKey, &c); err != nil {
		t.Fatal(err)
	}
	if c.Monitor || c.RollbackTimeout != 5*time.Second {
		t.Errorf("the configuration had no effect, got=%+v", c)
	}
}

func TestValidate_ZeroRollbackBudgetFails(t *testing.T) {
	// A 0 would make every rollback time out the moment it starts and skip every compensation,
	// while the flow itself looks entirely normal -- a misconfiguration like that has to be caught at startup.
	c := DefaultConfig()
	c.RollbackTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatal("a rollback budget of 0 must be an error")
	}
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("the default configuration must be valid: %v", err)
	}
}

func TestRegister_OnlyReadsConfigBuildsNothing(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xflow" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("no start hook is registered, so nobody reads the XFlow block and nobody calls Validate")
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xflow" {
			t.Error("this package has nothing to close, and registering a stop hook would put it in the shutdown sequence")
		}
	}
	// Config has to implement Validate() for the framework to catch a misconfiguration at startup.
	var c any = &Config{}
	if _, ok := c.(interface{ Validate() error }); !ok {
		t.Error("Config must implement Validate(), otherwise nothing catches a RollbackTimeout of 0")
	}
}

func TestLoad_InvalidConfigFailsStartup(t *testing.T) {
	// A RollbackTimeout of 0 would make every rollback time out the moment it starts and skip every compensation,
	// while the flow itself looks entirely normal -- only the Validate at load time can catch that.
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte("XFlow:\n  RollbackTimeout: 0s\n"), 0o644)

	c := DefaultConfig()
	err := config.LoadInto(path, ConfigKey, &c)
	if err == nil {
		t.Fatal("an invalid configuration must fail loading")
	}
	if !strings.Contains(err.Error(), "RollbackTimeout") {
		t.Errorf("the error must name the field, got=%v", err)
	}
}

// panicMonitor panics in every callback, to confirm that a failing monitoring implementation never interrupts the business flow.
type panicMonitor struct{}

func (panicMonitor) OnStep(context.Context, *StepEvent) { panic("the monitor's OnStep blew up") }
func (panicMonitor) OnFlow(context.Context, *FlowEvent) { panic("the monitor's OnFlow blew up") }

func TestMonitor_CallbackPanicIsolated(t *testing.T) {
	// A problem in observation must only lose one observation and must not interrupt the business flow.
	// The isolation is done with defer recoverNotify() -- recover only works when called directly by the deferred
	// function, and wrapping it in another layer breaks it, which is why this is pinned down.
	withConfig(t, nil)
	SetMonitor(panicMonitor{})
	t.Cleanup(func() { SetMonitor(slogMonitor{}) })

	d := &data{}
	res := New("place-order", ok("deduct-coupon"), ok("charge")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("a panicking monitor must not fail the flow: %v", res)
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("every step must run as usual, got=%v", got)
	}
}

func TestMonitor_CallbackPanicIsolatedDuringRollback(t *testing.T) {
	withConfig(t, nil)
	SetMonitor(panicMonitor{})
	t.Cleanup(func() { SetMonitor(slogMonitor{}) })

	d := &data{}
	res := New("place-order", ok("deduct-coupon"), failing("charge", Strong)).Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("the flow must fail when a strong dependency fails")
	}
	if got := d.backList(); len(got) != 1 || got[0] != "deduct-coupon" {
		t.Errorf("a panicking monitor must not stop the rollback, got=%v", got)
	}
}

func TestSlogMonitor_StepLogsAtInfoLevel(t *testing.T) {
	// Monitor on means wanting to see every step: a successful step is recorded at INFO, visible at the default level, and so is the flow result.
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("place-order", ok("deduct-coupon"), ok("charge")).Execute(context.Background(), &data{})

	got := buf.String()
	for _, want := range []string{`"level":"INFO","msg":"xflow step process done"`, "deduct-coupon", "charge", `"msg":"xflow flow done"`} {
		if !strings.Contains(got, want) {
			t.Errorf("at the default info level every step and the flow result must be visible, missing %q\ngot=\n%s", want, got)
		}
	}
}

func TestSlogMonitor_NoStepLogsAtWarnLevel(t *testing.T) {
	// With the log level at warn a successful step writes nothing; a failing step still writes.
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("place-order", ok("deduct-coupon")).Execute(context.Background(), &data{})
	if strings.Contains(buf.String(), "xflow step") {
		t.Errorf("at warn level there must be no log for a successful step\ngot=\n%s", buf.String())
	}
}

// cancelingStep cancels the parent ctx while it runs and returns the cancellation faithfully.
type cancelingStep struct {
	name   string
	dep    Dependency
	cancel context.CancelFunc
}

func (s *cancelingStep) Name() string           { return s.name }
func (s *cancelingStep) Dependency() Dependency { return s.dep }
func (s *cancelingStep) Process(ctx context.Context, d *data) error {
	s.cancel()
	return ctx.Err()
}
func (s *cancelingStep) Rollback(_ context.Context, d *data) error {
	d.record(&d.back, s.name)
	return nil
}

func TestExecute_CanceledLastStepMustNotReportSuccess(t *testing.T) {
	// Checking for cancellation only at the start of each step would miss a cancellation hitting the last step:
	// its context.Canceled would walk into the "weak dependency skipped" branch, the loop would end right after,
	// and a cancelled flow would be reported as Success with not a single step rolled back.
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d := &data{}

	res := New("place-order", ok("deduct-coupon"), &cancelingStep{name: "notify", dep: Weak, cancel: cancel}).
		Execute(ctx, d)

	if res.Success() {
		t.Fatalf("the flow was cancelled and must not be reported as a success: %v", res)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("it must say plainly that it was cancelled, got=%v", res.Err)
	}
	if !res.Rolled {
		t.Error("the executed steps must be rolled back after a cancellation")
	}
	if got := d.backList(); len(got) != 2 {
		t.Errorf("a failed weak dependency is in the rollback range too, got=%v", got)
	}
}

func TestExecute_CanceledMiddleWeakStepAlsoAborts(t *testing.T) {
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d := &data{}

	res := New("place-order",
		ok("deduct-coupon"),
		&cancelingStep{name: "notify", dep: Weak, cancel: cancel},
		ok("must-not-run"),
	).Execute(ctx, d)

	if res.Success() {
		t.Fatal("the flow was cancelled and must not be reported as a success")
	}
	if got := d.doneList(); strings.Contains(strings.Join(got, ","), "must-not-run") {
		t.Errorf("nothing may carry on after a cancellation, got=%v", got)
	}
}

func TestLoadConfig_ReadsConfigFlowActuallyUses(t *testing.T) {
	// Decoding into a local variable would also make the configuration "load successfully", but Execute reads the
	// package-level cfg -- RollbackTimeout and Monitor would then be configured for nothing, with no sign of it.
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = DefaultConfig()

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte("XFlow:\n  Monitor: false\n  RollbackTimeout: 3s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	if err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor || cfg.RollbackTimeout != 3*time.Second {
		t.Errorf("the configuration did not reach the package-level variable, got=%+v", cfg)
	}
}

func TestLoadConfig_InvalidConfigFailsStartup(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte("XFlow:\n  RollbackTimeout: 0s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	if err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)

	cfg = DefaultConfig()
	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("a rollback budget of 0 must fail startup")
	}
	if cfg.RollbackTimeout != DefaultConfig().RollbackTimeout {
		t.Errorf("a value that failed validation must not stay on the package-level configuration, got=%v", cfg.RollbackTimeout)
	}
}

func TestLoadConfig_UnsetFieldsResetToDefaultsNotPreviousValues(t *testing.T) {
	// Two Runs in the same process: the second config file does not write RollbackTimeout,
	// so it must be the default 30s rather than the 3s configured the first time.
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = Config{Monitor: false, RollbackTimeout: 3 * time.Second} // left over from the previous Run

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte("XFlow:\n  Monitor: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	if err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.RollbackTimeout != DefaultConfig().RollbackTimeout {
		t.Errorf("a field that is not set must return to its default, got=%v", cfg.RollbackTimeout)
	}
}
