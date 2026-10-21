package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// TestCron_JobLogsCarryJobAndTraceID_OverlapSkipped_SIGTERMWaitsForInFlightRun
//
// The documentation (xcron/README.md):
//  1. every run gets a root span (cron <name>), and the logs on its ctx carry job and trace_id;
//  2. a run is skipped while the previous one has not finished, recording a WARN cron job skipped;
//  3. on exit the ctx of a run in flight is cancelled and the clients are closed only once it has
//     returned (xcron is in StageServer and stops first).
//
// e2e-tick runs once a second and each run waits an hour while watching ctx: every run after the
// first meets "the previous one is still running".
// On SIGTERM it is cancelled, winds down for 300ms without watching ctx and then returns
func TestCron_JobLogsCarryJobAndTraceID_OverlapSkipped_SIGTERMWaitsForInFlightRun(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true, Overlay: "Service:\n  CronEvery: 1s\n  CronHold: 1h\n"})

	tick := p.WaitLog(t, 10*time.Second, func(l harness.Log) bool { return l.Msg() == "e2e cron tick" })
	if tick.Str("job") != "e2e-tick" || len(tick.Str("trace_id")) != 32 {
		t.Errorf("a log line inside the job should carry job and trace_id: %s", tick.Line)
	}
	skip := p.WaitLog(t, 10*time.Second, func(l harness.Log) bool { return l.Msg() == "cron job skipped, previous run still running" })
	if skip.Level() != "WARN" || skip.Str("job") != "e2e-tick" {
		t.Errorf("a skip records a WARN carrying job: %s", skip.Line)
	}
	if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "e2e cron tick" })); n != 1 {
		t.Errorf("the next run should not start while the previous has not finished: it ticked %d times", n)
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("it should exit with 0 after the SIGTERM, got %v\n%s", exit, lastLines(p.Output(), 30))
	}

	// The wind-down line comes before any client is closed: the stop hook waited for it to return
	drained, firstClientStop := -1, -1
	for i, l := range p.Logs() {
		switch {
		case l.Msg() == "e2e cron drained" && drained < 0:
			drained = i
		// A stop hook that is in StageServer alongside xcron (xkafka's consumer) is not a client
		case l.Msg() == "stopping" && firstClientStop < 0 && pkgOf(l.Str("hook")) != "xcron" && l.Str("hook") != "xkafka.stopConsumers":
			firstClientStop = i
		}
	}
	if drained < 0 || firstClientStop < 0 || drained > firstClientStop {
		t.Errorf("a run in flight should finish winding down before the clients are closed: drained=%d first client stop=%d\n%s", drained, firstClientStop, lastLines(p.Output(), 30))
	}
	failed := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cron job failed" && l.Str("job") == "e2e-tick" })
	if len(failed) != 1 || !strings.Contains(failed[0].Str("error"), "context canceled") || failed[0].Str("trace_id") != tick.Str("trace_id") {
		t.Errorf("the cancelled run records one cron job failed carrying the same trace_id: %v", failed)
	}

	s := p.WaitSpan(t, 5*time.Second, func(s harness.Span) bool { return s.Name == "cron e2e-tick" })
	if s.TraceID != tick.Str("trace_id") || s.ParentSpanID != "" {
		t.Errorf("it should be the root span of the trace in the log: %+v", s)
	}
}
