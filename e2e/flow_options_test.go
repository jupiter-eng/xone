package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covFlowResult is what POST /probe/flow answers, see hangFlow in service/probe.go
type covFlowResult struct {
	Success        bool     `json:"success"`
	Rolled         bool     `json:"rolled"`
	RollbackErrors []string `json:"rollback_errors"`
	ElapsedMS      int64    `json:"elapsed_ms"`
	Error          string   `json:"error"`
}

func covFlow(t *testing.T, p *harness.Process, hangMS int) covFlowResult {
	t.Helper()
	r := p.PostJSON(t, "/probe/flow", map[string]int{"hang_ms": hangMS})
	if r.Status != http.StatusOK {
		t.Fatalf("POST /probe/flow: %v", r)
	}
	var res covFlowResult
	r.JSON(t, &res)
	return res
}

// xflow/README.md, XFlow.RollbackTimeout: "the budget for rolling every step back", and "this
// budget holds for a Rollback that does not watch ctx as well: once it is up that one is no longer
// waited for, that step goes into RollbackErrors (with context.DeadlineExceeded in the error), the
// steps that never had their turn are each recorded as well, and Execute returns right after";
// writing 0 fails the start.
// The flow has three steps, first -> hang -> fail, and after fail fails they roll back in reverse:
// hang's Rollback sleeps 4s without watching ctx
func TestCoverage_ExecuteReturnsAtRollbackTimeoutWhenRollbackIgnoresCtx(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: "XFlow:\n  RollbackTimeout: 1s\n"})

	t.Run("it hangs 4s with a budget of 1s", func(t *testing.T) {
		start := time.Now()
		res := covFlow(t, p, 4000)
		took := time.Since(start)
		if res.Success || !res.Rolled {
			t.Fatalf("the fail step failed, so the flow should fail and have rolled back, got %+v", res)
		}
		if took < time.Second-50*time.Millisecond || took > time.Second+faultSlack {
			t.Errorf("with RollbackTimeout: 1s, Execute should return at about 1s and not wait out the 4s it hangs, got %v", took)
		}
		if len(res.RollbackErrors) != 2 {
			t.Fatalf("the hang that hung and the first that never had its turn should both go into RollbackErrors, got %q", res.RollbackErrors)
		}
		if e := res.RollbackErrors[0]; !strings.Contains(e, `"hang"`) || !strings.Contains(e, "context deadline exceeded") {
			t.Errorf("the step that hung goes into RollbackErrors with context.DeadlineExceeded, got %q", e)
		}
		if e := res.RollbackErrors[1]; !strings.Contains(e, `"first"`) || !strings.Contains(e, "never ran") {
			t.Errorf("the first that never had its turn is recorded as well, got %q", e)
		}
		t.Logf("numbers: the Rollback hangs 4s with RollbackTimeout 1s: Execute returned after %v (the server measured %dms)", took.Round(time.Millisecond), res.ElapsedMS)
	})

	t.Run("the rollback finishes within the budget", func(t *testing.T) {
		res := covFlow(t, p, 200)
		if !res.Rolled || len(res.RollbackErrors) != 0 || res.ElapsedMS < 200 || res.ElapsedMS > 200+faultSlack.Milliseconds() {
			t.Errorf("a 200ms rollback should finish within the 1s budget with no RollbackErrors, got %+v", res)
		}
	})

	t.Run("RollbackTimeout: 0 fails the start", func(t *testing.T) {
		stderr := covStartupError(t, harness.Options{Overlay: "XFlow:\n  RollbackTimeout: 0s\n"})
		faultMustContain(t, "the start error for RollbackTimeout: 0s", stderr, "RollbackTimeout must be > 0")
	})
}

// xflow/README.md, XFlow.Monitor: "turned off, Execute makes not one monitor call". The default
// monitor writes to slog: a failing step records a WARN and an incomplete rollback an ERROR
// (slogMonitor in xflow/monitor.go)
func TestCoverage_XflowMonitorOnLogsStepsAndFlow_OffLogsNothing(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	isFlowLog := func(l harness.Log) bool {
		return strings.HasPrefix(l.Msg(), "xflow ") && l.Str("flow") == "cov_hang_rollback"
	}
	for _, on := range []bool{true, false} {
		name := map[bool]string{true: "Monitor: true (the default)", false: "Monitor: false"}[on]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			overlay := "XFlow:\n  RollbackTimeout: 1s\n"
			if !on {
				overlay += "  Monitor: false\n"
			}
			p := harness.Start(t, harness.Options{Overlay: overlay})
			r := p.PostJSON(t, "/probe/flow", map[string]int{"hang_ms": 1500})
			accessLog(t, p, traceIDOf(t, r)) // a landmark: the access log comes after Execute returns, so a monitor line would be out by then
			logs := p.FindLogs(isFlowLog)
			if !on {
				if len(logs) != 0 {
					t.Errorf("with Monitor: false not one monitor call is made, and there are %d xflow log lines: %s", len(logs), logs[0].Line)
				}
				return
			}
			want := map[string]string{
				"xflow step process failed":                                       "WARN",
				"xflow rollback did not complete, resources may be left dangling": "ERROR",
			}
			for msg, level := range want {
				found := false
				for _, l := range logs {
					if l.Msg() == msg && l.Level() == level {
						found = true
					}
				}
				if !found {
					t.Errorf("with Monitor on there should be one %s %q, and there are %d xflow log lines", level, msg, len(logs))
				}
			}
		})
	}
}
