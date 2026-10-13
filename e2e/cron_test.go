package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// TestCron_JobLogsCarryJobAndTraceID_OverlapSkipped_SIGTERMWaitsForInFlightRun
//
// 文档（xcron/README.md）：
//  1. 每次执行一个根 Span（cron <name>），ctx 里的日志带着 job 和 trace_id；
//  2. 上一次没跑完时这一次跳过，记 WARN cron job skipped；
//  3. 退出时取消在途执行的 ctx，等它返回之后才轮到关客户端（xcron 在 StageServer，最先停）。
//
// e2e-tick 每秒一次，每次看着 ctx 等一小时：第一次之后的每一次都撞上「上一次还在跑」。
// 收到 SIGTERM 时它被取消，不看 ctx 地收尾 300ms 再返回
func TestCron_JobLogsCarryJobAndTraceID_OverlapSkipped_SIGTERMWaitsForInFlightRun(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true, Overlay: "Service:\n  CronEvery: 1s\n  CronHold: 1h\n"})

	tick := p.WaitLog(t, 10*time.Second, func(l harness.Log) bool { return l.Msg() == "e2e cron tick" })
	if tick.Str("job") != "e2e-tick" || len(tick.Str("trace_id")) != 32 {
		t.Errorf("任务里的日志该带 job 和 trace_id：%s", tick.Line)
	}
	skip := p.WaitLog(t, 10*time.Second, func(l harness.Log) bool { return l.Msg() == "cron job skipped, previous run still running" })
	if skip.Level() != "WARN" || skip.Str("job") != "e2e-tick" {
		t.Errorf("跳过记 WARN，带 job：%s", skip.Line)
	}
	if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "e2e cron tick" })); n != 1 {
		t.Errorf("上一次没跑完，不该开始下一次：tick 了 %d 次", n)
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("SIGTERM 之后应以 0 退出，实际 %v\n%s", exit, lastLines(p.Output(), 30))
	}

	// 收尾那一行在任何客户端关闭之前：停止钩子等它返回了才往下走
	drained, firstClientStop := -1, -1
	for i, l := range p.Logs() {
		switch {
		case l.Msg() == "e2e cron drained" && drained < 0:
			drained = i
		// 和 xcron 同在 StageServer 的停止钩子（xkafka 的消费者）不是客户端
		case l.Msg() == "stopping" && firstClientStop < 0 && pkgOf(l.Str("hook")) != "xcron" && l.Str("hook") != "xkafka.stopConsumers":
			firstClientStop = i
		}
	}
	if drained < 0 || firstClientStop < 0 || drained > firstClientStop {
		t.Errorf("在途的执行该在客户端关闭之前收完尾：drained=%d first client stop=%d\n%s", drained, firstClientStop, lastLines(p.Output(), 30))
	}
	failed := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cron job failed" && l.Str("job") == "e2e-tick" })
	if len(failed) != 1 || !strings.Contains(failed[0].Str("error"), "context canceled") || failed[0].Str("trace_id") != tick.Str("trace_id") {
		t.Errorf("被取消的那次执行记一行 cron job failed，带着同一个 trace_id：%v", failed)
	}

	s := p.WaitSpan(t, 5*time.Second, func(s harness.Span) bool { return s.Name == "cron e2e-tick" })
	if s.TraceID != tick.Str("trace_id") || s.ParentSpanID != "" {
		t.Errorf("该是日志里那个 trace 的根 Span：%+v", s)
	}
}
