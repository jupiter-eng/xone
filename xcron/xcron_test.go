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

// ---- 校验 ----

func wantXErr(t *testing.T, err error, op string, contains ...string) {
	t.Helper()
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xcron" || xe.Op != op {
		t.Fatalf("want *xerror.Error{xcron %s}, got %T %v", op, err, err)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("错误里该有 %q：%v", c, err)
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
		t.Errorf("换了名字就该登记得上：%v", err)
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

// ---- 每次执行的注入 ----

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
		t.Fatal("fn 的 ctx 里该有 Span")
	}
	got := spans.GetSpans()
	if len(got) != 1 || got[0].Name != "cron report" || got[0].SpanKind != trace.SpanKindInternal || got[0].Parent.IsValid() {
		t.Fatalf("该有一个名为 cron report 的 internal 根 Span：%+v", got)
	}
	if got[0].SpanContext.TraceID() != inner.TraceID() {
		t.Error("fn 的 ctx 里的 Span 该就是那个根 Span")
	}

	lines := logs()
	for _, msg := range []string{"cron job started", "cron job finished"} {
		l := withMsg(lines, msg)
		if len(l) != 1 || l[0]["job"] != "report" || l[0]["trace_id"] != inner.TraceID().String() {
			t.Errorf("%s 该带 job 和 trace_id：%v", msg, l)
		}
	}
	fin := withMsg(lines, "cron job finished")[0]
	if fin["level"] != "INFO" || fin["rows"] != float64(3) {
		t.Errorf("结束那行该是 INFO，带着 fn 里 AddKV 的字段：%v", fin)
	}
	if _, ok := fin["elapsed_ms"].(float64); !ok {
		t.Errorf("结束那行该带 elapsed_ms：%v", fin)
	}
	if l := withMsg(lines, "cron job started"); l[0]["level"] != "DEBUG" {
		t.Errorf("开始那行记 DEBUG：%v", l)
	}
}

func TestExecute_FailureLogsWarnAndMarksSpan(t *testing.T) {
	logs := captureLogs(t)
	spans := captureSpans(t)
	boom := errors.New("boom")
	if err := execute(context.Background(), "report", 0, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("fn 的错误原样返回：%v", err)
	}
	l := withMsg(logs(), "cron job failed")
	if len(l) != 1 || l[0]["level"] != "WARN" || l[0]["error"] != "boom" || l[0]["job"] != "report" {
		t.Errorf("失败记 WARN，带 error 和 job：%v", l)
	}
	s := spans.GetSpans()[0]
	if s.Status.Code != codes.Error || s.Status.Description != "cron job failed" || len(s.Events) != 0 {
		t.Errorf("失败的 Span 只标状态，不记错误原文（原文可能带着 SQL 参数）：%+v %v", s.Status, s.Events)
	}
}

func TestExecute_RecoversPanic(t *testing.T) {
	logs := captureLogs(t)
	spans := captureSpans(t)
	err := execute(context.Background(), "report", 0, func(context.Context) error { panic("kaboom") })
	if err == nil || !strings.Contains(err.Error(), "panicked: kaboom") {
		t.Fatalf("panic 该变成错误：%v", err)
	}
	l := withMsg(logs(), "cron job panicked")
	if len(l) != 1 || l[0]["level"] != "ERROR" || l[0]["job"] != "report" || !strings.Contains(fmt.Sprint(l[0]["stack"]), "xcron") {
		t.Errorf("panic 记 ERROR，带栈：%v", l)
	}
	if s := spans.GetSpans()[0].Status; s.Code != codes.Error || s.Description != "cron job panicked" {
		t.Errorf("panic 的 Span 该标成错误：%+v", s)
	}
}

func TestExecute_TimeoutCancelsCtx(t *testing.T) {
	start := time.Now()
	err := execute(context.Background(), "slow", 20*time.Millisecond, waitCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("到点该取消 ctx：%v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("20ms 的超时等了 %v", d)
	}
}

// ---- 调度 ----

func TestScheduler_RunsOnSchedule(t *testing.T) {
	logs := captureLogs(t)
	fresh(t)
	var n atomic.Int32
	if err := Add("@every 10ms", func(context.Context) error { n.Add(1); return nil }, WithName("tick")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatal("调度器起来之前不该跑")
	}
	started(t, std)
	eventually(t, "跑了 3 次", func() bool { return n.Load() >= 3 })
	eventually(t, "结束那行带着 job", func() bool {
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
		t.Errorf("该按 WithLocation 的时区算下一个时间点：got=%v", loc)
	}
}

// recordingSchedule 记下 Next 收到的时刻，下一个时间点永远在一小时后
type recordingSchedule func(time.Time)

func (r recordingSchedule) Next(t time.Time) time.Time { r(t); return t.Add(time.Hour) }

func TestOptions_DefaultLocationIsUTC(t *testing.T) {
	if o := buildOptions(ok, nil); o.loc != time.UTC {
		t.Errorf("默认时区该是 UTC：got=%v", o.loc)
	}
}

func TestTimeout_AppliesToFirstRuns(t *testing.T) {
	fresh(t)
	Add("@every 1h", waitCtx, WithName("boot"), WithTimeout(20*time.Millisecond), RunOnStartAndWait())
	if err := std.start(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("启动时的第一次也受 WithTimeout 管：%v", err)
	}

	fresh(t)
	started(t, std)
	err := Add("@every 1h", waitCtx, WithName("late"), WithTimeout(20*time.Millisecond), RunOnStartAndWait())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("起来之后 Add 的第一次也受 WithTimeout 管：%v", err)
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
	eventually(t, "跳过的 WARN", func() bool { return len(withMsg(logs(), "cron job skipped, previous run still running")) >= 3 })
	if got := n.Load(); got != 1 {
		t.Errorf("上一次没跑完时不该开始下一次：跑了 %d 次", got)
	}
	l := withMsg(logs(), "cron job skipped, previous run still running")[0]
	if l["level"] != "WARN" || l["job"] != "slow" {
		t.Errorf("跳过记 WARN，带 job：%v", l)
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
	eventually(t, "3 次同时在跑", func() bool { return inFlight.Load() >= 3 })
}

func TestScheduler_PanicDoesNotStopTheJobOrOthers(t *testing.T) {
	captureLogs(t)
	fresh(t)
	var panics, others atomic.Int32
	Add("@every 5ms", func(context.Context) error { panics.Add(1); panic("kaboom") }, WithName("bad"))
	Add("@every 5ms", func(context.Context) error { others.Add(1); return nil }, WithName("good"))
	started(t, std)
	eventually(t, "panic 之后两个任务都照常调度", func() bool { return panics.Load() >= 3 && others.Load() >= 3 })
}

func TestStart_RunOnStartRunsNowWithoutWaiting(t *testing.T) {
	fresh(t)
	gate := release(t)
	ran := make(chan struct{})
	Add("@every 1h", func(context.Context) error { close(ran); <-gate; return errors.New("ignored") }, WithName("warm"), RunOnStart())
	started(t, std) // 不等它：fn 卡在 gate 上，start 照样返回
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnStart 该在起来时就跑一次")
	}
}

func TestStart_RunOnStartAndWaitWaitsForFirstRuns(t *testing.T) {
	fresh(t)
	// 两个都到了才往下走：串行跑的话第一个永远等不到第二个，2s 后报错让启动失败
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
		t.Fatal("start 返回时两个任务的第一次都该跑完了")
	}
}

func TestStart_RunOnStartAndWaitFailureFailsStartup(t *testing.T) {
	fresh(t)
	boom := errors.New("boom")
	Add("@every 1h", func(context.Context) error { return boom }, WithName("load-config"), RunOnStartAndWait())
	Add("@every 1h", ok, WithName("fine"), RunOnStartAndWait())

	// 走的是框架会调的那个启动钩子
	err := startXCron(context.Background())
	wantXErr(t, err, "start", `job "load-config"`)
	if !errors.Is(err, boom) {
		t.Errorf("任务的错误该用 %%w 带着：%v", err)
	}
	if strings.Contains(err.Error(), `"fine"`) {
		t.Errorf("成功的任务不该被点名：%v", err)
	}
	wantXErr(t, Add("@every 1h", ok, WithName("late")), "register")
}

func TestStart_RunOnStartAndWaitHonorsStartCtx(t *testing.T) {
	fresh(t)
	Add("@every 1h", waitCtx, WithName("hang"), RunOnStartAndWait())
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := std.start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("退出信号该取消第一次执行：%v", err)
	}
}

func TestAdd_AfterStartSchedulesImmediately(t *testing.T) {
	fresh(t)
	started(t, std)
	var n atomic.Int32
	if err := Add("@every 5ms", func(context.Context) error { n.Add(1); return nil }, WithName("late")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "起来之后 Add 的也跑", func() bool { return n.Load() >= 2 })
}

func TestAdd_AfterStartWithRunOnStartAndWait(t *testing.T) {
	fresh(t)
	started(t, std)
	var done atomic.Bool
	if err := Add("@every 1h", func(context.Context) error { done.Store(true); return nil }, WithName("w"), RunOnStartAndWait()); err != nil || !done.Load() {
		t.Fatalf("Add 该等第一次跑完：err=%v done=%v", err, done.Load())
	}

	boom := errors.New("boom")
	err := Add("@every 1h", func(context.Context) error { return boom }, WithName("bad"), RunOnStartAndWait())
	wantXErr(t, err, "execute", `"bad"`)
	if !errors.Is(err, boom) {
		t.Errorf("第一次的错误该用 %%w 带着：%v", err)
	}
	if err := Add("@every 1h", ok, WithName("bad")); err != nil {
		t.Errorf("第一次失败的不登记，同名可以再 Add：%v", err)
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
	eventually(t, "跑起来", func() bool { return n.Load() >= 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := std.stop(ctx); err != nil {
		t.Fatal(err)
	}
	before := n.Load()
	time.Sleep(50 * time.Millisecond)
	if after := n.Load(); after != before {
		t.Errorf("停了之后不该再跑：%d → %d", before, after)
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
		time.Sleep(30 * time.Millisecond) // 收尾
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
		t.Error("stop 该取消在途执行的 ctx")
	}
	if !finished.Load() {
		t.Error("stop 该等在途的执行返回")
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
		t.Errorf("只点名还在跑的，带着截止时间的错误：%v", err)
	}
	l := withMsg(logs(), "xcron jobs still running when the stop budget ran out")
	if len(l) != 1 || fmt.Sprint(l[0]["jobs"]) != "[stuck]" {
		t.Errorf("点名的日志无论如何都要写出去：%v", l)
	}
}

func TestHooks_StartAtServerStage(t *testing.T) {
	for _, e := range hook.Start() {
		if e.Name == "xcron.startXCron" {
			if e.Stage != hook.StageServer {
				t.Errorf("调度器该在 StageServer 起来：got=%v", e.Stage)
			}
			return
		}
	}
	t.Fatal("xcron 没登记启动钩子")
}
