package xcron

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xlog"
)

// ---- validation ----

func wantXErr(t *testing.T, err error, op string, contains ...string) {
	t.Helper()
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xcron" || xe.Op != op {
		t.Fatalf("want *xerror.Error{xcron %s}, got %T %v", op, err, err)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("the error must contain %q: %v", c, err)
		}
	}
}

func TestAdd_InvalidInputIsConfigError(t *testing.T) {
	fresh(t)
	wantXErr(t, Add("61 * * * *", ok, WithName("report")), "config", `"61 * * * *"`, `"report"`)
	wantXErr(t, Add("@every 1m", nil), "config", "nil function")
	wantXErr(t, Add("@every 1m", ok, WithName(" ")), "config", "name is empty")
	wantXErr(t, Add("@every 1m", ok, WithName("a"), WithTimeout(-time.Second)), "config", "timeout")
	wantXErr(t, Add("@every 1m", ok, WithName("b"), WithLocation(nil)), "config", "location is nil")
	wantXErr(t, Add("@every 1m", ok, WithName("c"), RunOnStart(), RunOnStartAndWait()), "config", "mutually exclusive")
	wantXErr(t, Add("CRON_TZ=UTC 0 * * * *", ok, WithName("d")), "config", "WithLocation")
}

func TestAdd_DuplicateNameIsConfigError(t *testing.T) {
	fresh(t)
	if err := Add("@every 1m", ok); err != nil {
		t.Fatal(err)
	}
	wantXErr(t, Add("@daily", ok), "config", `duplicate job name "xcron.ok"`)
	if err := Add("@daily", ok, WithName("ok-daily")); err != nil {
		t.Errorf("a different name must register: %v", err)
	}
}

type service struct{}

func (service) Sync(context.Context) error     { return nil }
func (*service) Refresh(context.Context) error { return nil }
func cleanup(context.Context) error            { return nil }

func TestFuncName_DefaultNames(t *testing.T) {
	s := &service{}
	closure := func(context.Context) error { return nil }
	cases := map[string]any{
		"xcron.cleanup":                         cleanup,
		"xcron.service.Sync":                    service{}.Sync,
		"xcron.(*service).Refresh":              s.Refresh,
		"xcron.TestFuncName_DefaultNames.func1": closure,
	}
	for want, fn := range cases {
		if got := funcName(fn); got != want {
			t.Errorf("got=%q want=%q", got, want)
		}
	}
}

// ---- what each execution injects ----

func TestExecute_InjectsRootSpanAndJobField(t *testing.T) {
	logs := captureLogs(t)
	spans := captureSpans(t)

	var inner trace.SpanContext
	err := execute(context.Background(), "report", 0, func(ctx context.Context) error {
		inner = trace.SpanContextFromContext(ctx)
		xlog.AddKV(ctx, "rows", 3)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inner.IsValid() {
		t.Fatal("fn's ctx must carry a Span")
	}
	got := spans.GetSpans()
	if len(got) != 1 || got[0].Name != "cron report" || got[0].SpanKind != trace.SpanKindInternal || got[0].Parent.IsValid() {
		t.Fatalf("there must be one internal root Span named cron report: %+v", got)
	}
	if got[0].SpanContext.TraceID() != inner.TraceID() {
		t.Error("the Span in fn's ctx must be that root Span")
	}

	lines := logs()
	for _, msg := range []string{"cron job started", "cron job finished"} {
		l := withMsg(lines, msg)
		if len(l) != 1 || l[0]["job"] != "report" || l[0]["trace_id"] != inner.TraceID().String() {
			t.Errorf("%s must carry job and trace_id: %v", msg, l)
		}
	}
	fin := withMsg(lines, "cron job finished")[0]
	if fin["level"] != "INFO" || fin["rows"] != float64(3) {
		t.Errorf("the end line must be INFO and carry the fields AddKV added inside fn: %v", fin)
	}
	if _, ok := fin["elapsed_ms"].(float64); !ok {
		t.Errorf("the end line must carry elapsed_ms: %v", fin)
	}
	if l := withMsg(lines, "cron job started"); l[0]["level"] != "DEBUG" {
		t.Errorf("the start line is recorded at DEBUG: %v", l)
	}
}

func TestExecute_FailureLogsWarnAndMarksSpan(t *testing.T) {
	logs := captureLogs(t)
	spans := captureSpans(t)
	boom := errors.New("boom")
	if err := execute(context.Background(), "report", 0, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("fn's error must be returned as it is: %v", err)
	}
	l := withMsg(logs(), "cron job failed")
	if len(l) != 1 || l[0]["level"] != "WARN" || l[0]["error"] != "boom" || l[0]["job"] != "report" {
		t.Errorf("a failure is recorded at WARN with error and job: %v", l)
	}
	s := spans.GetSpans()[0]
	if s.Status.Code != codes.Error || s.Status.Description != "cron job failed" || len(s.Events) != 0 {
		t.Errorf("a failed Span carries only the status, not the error text (which may carry SQL parameters): %+v %v", s.Status, s.Events)
	}
}

func TestExecute_RecoversPanic(t *testing.T) {
	logs := captureLogs(t)
	spans := captureSpans(t)
	err := execute(context.Background(), "report", 0, func(context.Context) error { panic("kaboom") })
	if err == nil || !strings.Contains(err.Error(), "panicked: kaboom") {
		t.Fatalf("a panic must become an error: %v", err)
	}
	l := withMsg(logs(), "cron job panicked")
	if len(l) != 1 || l[0]["level"] != "ERROR" || l[0]["job"] != "report" || !strings.Contains(fmt.Sprint(l[0]["stack"]), "xcron") {
		t.Errorf("a panic is recorded at ERROR with the stack: %v", l)
	}
	if s := spans.GetSpans()[0].Status; s.Code != codes.Error || s.Description != "cron job panicked" {
		t.Errorf("a panicking Span must be marked as an error: %+v", s)
	}
}

func TestExecute_TimeoutCancelsCtx(t *testing.T) {
	start := time.Now()
	err := execute(context.Background(), "slow", 20*time.Millisecond, waitCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the ctx must be cancelled when the limit is reached: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a 20ms timeout waited %v", d)
	}
}

// ---- scheduling ----

func TestScheduler_RunsOnSchedule(t *testing.T) {
	logs := captureLogs(t)
	fresh(t)
	var n atomic.Int32
	if err := Add("@every 10ms", func(context.Context) error { n.Add(1); return nil }, WithName("tick")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatal("nothing may run before the scheduler is up")
	}
	started(t, std)
	eventually(t, "ran 3 times", func() bool { return n.Load() >= 3 })
	eventually(t, "the end line carries job", func() bool {
		l := withMsg(logs(), "cron job finished")
		return len(l) > 0 && l[0]["job"] == "tick"
	})
}

func TestScheduler_PassesLocationToSchedule(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	fresh(t)
	got := make(chan *time.Location, 1)
	rec := recordingSchedule(func(t time.Time) {
		select {
		case got <- t.Location():
		default:
		}
	})
	std.add(&job{name: "tz", sched: rec, fn: ok, o: buildOptions(ok, []Option{WithLocation(berlin)})})
	started(t, std)
	if loc := <-got; loc != berlin {
		t.Errorf("the next time point must be computed in the WithLocation time zone: got=%v", loc)
	}
}

// recordingSchedule records the moments Next receives, with the next time point always an hour later.
type recordingSchedule func(time.Time)

func (r recordingSchedule) Next(t time.Time) time.Time { r(t); return t.Add(time.Hour) }

// A schedule with no time point left returns the zero time (see the schedule interface), and time.Until of it is
// a large negative duration: without the guard the timer fires at once, prev stays zero, the next round computes
// the zero time again, and the loop spins as fast as the CPU allows -- starting an execution every round, or
// writing one "cron job skipped" per round. A spec that never fires is refused at registration, so what is left
// is the far end of one that does: cronexpr v1.1.3 searches the years 1970..2099, which "0 0 29 2 *" runs past
// in 2096, its next leap day being 2104.
func TestScheduler_NoNextTimePointStopsTheLoop(t *testing.T) {
	logs := captureLogs(t)
	fresh(t)
	var asked, ran atomic.Int32
	sched := exhaustedSchedule(func() { asked.Add(1) })
	std.add(&job{name: "sweep", sched: sched, fn: func(context.Context) error { ran.Add(1); return nil },
		o: buildOptions(ok, []Option{WithName("sweep")})})
	started(t, std)

	eventually(t, "the WARN saying there is no next time point", func() bool {
		return len(withMsg(logs(), "cron job has no next time point and will not run again")) == 1
	})
	time.Sleep(50 * time.Millisecond)
	if got := asked.Load(); got != 1 {
		t.Errorf("the loop must ask for the next time point once and then give up, asked %d times", got)
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("a job with no time point left must not run, ran %d times", got)
	}
	l := withMsg(logs(), "cron job has no next time point and will not run again")[0]
	if l["level"] != "WARN" || l["job"] != "sweep" {
		t.Errorf("it is recorded at WARN with job: %v", l)
	}
}

// exhaustedSchedule has run out of time points: Next counts the question and answers with the zero time.
type exhaustedSchedule func()

func (e exhaustedSchedule) Next(time.Time) time.Time { e(); return time.Time{} }

func TestOptions_DefaultLocationIsUTC(t *testing.T) {
	if o := buildOptions(ok, nil); o.loc != time.UTC {
		t.Errorf("the default time zone must be UTC: got=%v", o.loc)
	}
}

func TestTimeout_AppliesToFirstRuns(t *testing.T) {
	fresh(t)
	Add("@every 1h", waitCtx, WithName("boot"), WithTimeout(20*time.Millisecond), RunOnStartAndWait())
	if err := std.start(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the first run at startup is also subject to WithTimeout: %v", err)
	}

	fresh(t)
	started(t, std)
	err := Add("@every 1h", waitCtx, WithName("late"), WithTimeout(20*time.Millisecond), RunOnStartAndWait())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the first run of an Add after startup is also subject to WithTimeout: %v", err)
	}
}

func TestScheduler_SkipsOverlappingRunAndWarns(t *testing.T) {
	logs := captureLogs(t)
	fresh(t)
	gate := release(t)
	var n atomic.Int32
	if err := Add("@every 5ms", func(context.Context) error { n.Add(1); <-gate; return nil }, WithName("slow")); err != nil {
		t.Fatal(err)
	}
	started(t, std)
	eventually(t, "the skipped WARN", func() bool { return len(withMsg(logs(), "cron job skipped, previous run still running")) >= 3 })
	if got := n.Load(); got != 1 {
		t.Errorf("the next run must not start while the previous one is unfinished: ran %d times", got)
	}
	l := withMsg(logs(), "cron job skipped, previous run still running")[0]
	if l["level"] != "WARN" || l["job"] != "slow" {
		t.Errorf("a skip is recorded at WARN with job: %v", l)
	}
}

func TestScheduler_AllowOverlapRunsConcurrently(t *testing.T) {
	fresh(t)
	gate := release(t)
	var inFlight atomic.Int32
	if err := Add("@every 5ms", func(context.Context) error { inFlight.Add(1); <-gate; return nil }, WithName("slow"), AllowOverlap()); err != nil {
		t.Fatal(err)
	}
	started(t, std)
	eventually(t, "3 running at once", func() bool { return inFlight.Load() >= 3 })
}

func TestScheduler_PanicDoesNotStopTheJobOrOthers(t *testing.T) {
	captureLogs(t)
	fresh(t)
	var panics, others atomic.Int32
	Add("@every 5ms", func(context.Context) error { panics.Add(1); panic("kaboom") }, WithName("bad"))
	Add("@every 5ms", func(context.Context) error { others.Add(1); return nil }, WithName("good"))
	started(t, std)
	eventually(t, "both jobs keep being scheduled after a panic", func() bool { return panics.Load() >= 3 && others.Load() >= 3 })
}

func TestStart_RunOnStartRunsNowWithoutWaiting(t *testing.T) {
	fresh(t)
	gate := release(t)
	ran := make(chan struct{})
	Add("@every 1h", func(context.Context) error { close(ran); <-gate; return errors.New("ignored") }, WithName("warm"), RunOnStart())
	started(t, std) // not waited for: fn is blocked on gate and start returns anyway
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnStart must run once as it comes up")
	}
}

func TestStart_RunOnStartAndWaitWaitsForFirstRuns(t *testing.T) {
	fresh(t)
	// Both have to arrive before it continues: run serially, the first would never see the second, and the error after 2s would fail startup.
	var arrived atomic.Int32
	var done atomic.Int32
	both := func(context.Context) error {
		arrived.Add(1)
		for deadline := time.Now().Add(2 * time.Second); arrived.Load() < 2; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				return errors.New("ran alone, first runs are not parallel")
			}
		}
		time.Sleep(20 * time.Millisecond)
		done.Add(1)
		return nil
	}
	Add("@every 1h", both, WithName("a"), RunOnStartAndWait())
	Add("@every 1h", both, WithName("b"), RunOnStartAndWait())
	started(t, std)
	if done.Load() != 2 {
		t.Fatal("the first run of both jobs must have finished by the time start returns")
	}
}

func TestStart_RunOnStartAndWaitFailureFailsStartup(t *testing.T) {
	fresh(t)
	boom := errors.New("boom")
	Add("@every 1h", func(context.Context) error { return boom }, WithName("load-config"), RunOnStartAndWait())
	Add("@every 1h", ok, WithName("fine"), RunOnStartAndWait())

	// This goes through the start hook the framework would call.
	err := startXCron(context.Background())
	wantXErr(t, err, "start", `job "load-config"`)
	if !errors.Is(err, boom) {
		t.Errorf("the job's error must be carried with %%w: %v", err)
	}
	if strings.Contains(err.Error(), `"fine"`) {
		t.Errorf("a job that succeeded must not be named: %v", err)
	}
	wantXErr(t, Add("@every 1h", ok, WithName("late")), "register")
}

func TestStart_RunOnStartAndWaitHonorsStartCtx(t *testing.T) {
	fresh(t)
	Add("@every 1h", waitCtx, WithName("hang"), RunOnStartAndWait())
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := std.start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the stop signal must cancel the first execution: %v", err)
	}
}

func TestStart_DynamicallyAddedFirstRunHonorsStartCtx(t *testing.T) {
	s := fresh(t)
	entered := make(chan struct{})
	if err := Add("@every 1h", func(context.Context) error {
		return Add("@every 1h", func(ctx context.Context) error {
			close(entered)
			return waitCtx(ctx)
		}, WithName("inner"), RunOnStartAndWait())
	}, WithName("outer"), RunOnStartAndWait()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.start(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("nested first run did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup cancellation was lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup cancellation did not reach the dynamically added first run")
	}
}

func TestStart_CompletedStartupLeavesCancellationToStop(t *testing.T) {
	s := fresh(t)
	entered := make(chan context.Context, 1)
	if err := Add("@every 1h", func(ctx context.Context) error {
		entered <- ctx
		<-ctx.Done()
		return ctx.Err()
	}, WithName("background"), RunOnStart()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	var jobCtx context.Context
	select {
	case jobCtx = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the background job did not start")
	}
	cancel()
	select {
	case <-jobCtx.Done():
		t.Fatal("after startup, the stop hook must own cancellation")
	case <-time.After(20 * time.Millisecond):
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := s.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(jobCtx.Err(), context.Canceled) {
		t.Fatalf("stop did not cancel the background job: %v", jobCtx.Err())
	}
}

func TestStart_FailureWaitsForDynamicallyAddedJobs(t *testing.T) {
	s := fresh(t)
	entered, canceled := make(chan struct{}), make(chan struct{})
	finish := release(t)
	boom := errors.New("warmup failed")
	if err := Add("@every 1h", func(context.Context) error {
		if err := Add("@every 1h", func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-finish
			return ctx.Err()
		}, WithName("background"), RunOnStart()); err != nil {
			return err
		}
		<-entered
		return boom
	}, WithName("warmup"), RunOnStartAndWait()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.start(context.Background()) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("startup failure did not cancel the dynamically added job")
	}
	select {
	case err := <-done:
		t.Fatalf("startup returned before the canceled job finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// release's cleanup normally opens this gate; here release it explicitly without closing twice.
	finish <- struct{}{}
	if err := <-done; !errors.Is(err, boom) {
		t.Fatalf("startup error = %v, want original warmup failure", err)
	}
}

func TestAdd_AfterStartSchedulesImmediately(t *testing.T) {
	fresh(t)
	started(t, std)
	var n atomic.Int32
	if err := Add("@every 5ms", func(context.Context) error { n.Add(1); return nil }, WithName("late")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a job added after startup runs too", func() bool { return n.Load() >= 2 })
}

func TestAdd_AfterStartWithRunOnStartAndWait(t *testing.T) {
	fresh(t)
	started(t, std)
	var done atomic.Bool
	if err := Add("@every 1h", func(context.Context) error { done.Store(true); return nil }, WithName("w"), RunOnStartAndWait()); err != nil || !done.Load() {
		t.Fatalf("Add must wait for the first run: err=%v done=%v", err, done.Load())
	}

	boom := errors.New("boom")
	err := Add("@every 1h", func(context.Context) error { return boom }, WithName("bad"), RunOnStartAndWait())
	wantXErr(t, err, "execute", `"bad"`)
	if !errors.Is(err, boom) {
		t.Errorf("the first run's error must be carried with %%w: %v", err)
	}
	if err := Add("@every 1h", ok, WithName("bad")); err != nil {
		t.Errorf("a job whose first run failed is not registered, so the same name can be added again: %v", err)
	}
}

func TestAdd_AfterStopIsRegisterError(t *testing.T) {
	fresh(t)
	started(t, std)
	if err := stopXCron(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantXErr(t, Add("@every 1h", ok, WithName("late")), "register", "shutting down")
}

func TestStop_NoNewRunsAfterStop(t *testing.T) {
	fresh(t)
	var n atomic.Int32
	Add("@every 5ms", func(context.Context) error { n.Add(1); return nil }, WithName("tick"))
	started(t, std)
	eventually(t, "it is running", func() bool { return n.Load() >= 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := std.stop(ctx); err != nil {
		t.Fatal(err)
	}
	before := n.Load()
	time.Sleep(50 * time.Millisecond)
	if after := n.Load(); after != before {
		t.Errorf("nothing may run after stopping: %d -> %d", before, after)
	}
}

func TestStop_CancelsInFlightAndWaitsForThem(t *testing.T) {
	fresh(t)
	running := make(chan struct{})
	var cancelled, finished atomic.Bool
	Add("@every 1h", func(ctx context.Context) error {
		close(running)
		err := waitCtx(ctx)
		cancelled.Store(errors.Is(err, context.Canceled))
		time.Sleep(30 * time.Millisecond) // cleanup
		finished.Store(true)
		return err
	}, WithName("flush"), RunOnStart())
	started(t, std)
	<-running

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stopXCron(ctx); err != nil {
		t.Fatal(err)
	}
	if !cancelled.Load() {
		t.Error("stop must cancel the ctx of the executions in flight")
	}
	if !finished.Load() {
		t.Error("stop must wait for the executions in flight to return")
	}
}

func TestStop_DeadlineNamesStuckJobs(t *testing.T) {
	logs := captureLogs(t)
	fresh(t)
	gate := release(t)
	running := make(chan struct{})
	Add("@every 1h", func(context.Context) error { close(running); <-gate; return nil }, WithName("stuck"), RunOnStart())
	Add("@every 1h", ok, WithName("idle"))
	started(t, std)
	<-running

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := stopXCron(ctx)
	wantXErr(t, err, "stop", "stuck")
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "idle") {
		t.Errorf("only the ones still running are named, in an error carrying the deadline: %v", err)
	}
	l := withMsg(logs(), "xcron jobs still running when the stop budget ran out")
	if len(l) != 1 || fmt.Sprint(l[0]["jobs"]) != "[stuck]" {
		t.Errorf("the log naming them must go out no matter what: %v", l)
	}
}

func TestHooks_StartAtServerStage(t *testing.T) {
	for _, e := range hook.Start() {
		if e.Name == "xcron.startXCron" {
			if e.Stage != hook.StageServer {
				t.Errorf("the scheduler must come up on StageServer: got=%v", e.Stage)
			}
			return
		}
	}
	t.Fatal("xcron registered no start hook")
}
