package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covFlowResult POST /probe/flow 的响应，见 service/probe.go 的 hangFlow
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
		t.Fatalf("POST /probe/flow：%v", r)
	}
	var res covFlowResult
	r.JSON(t, &res)
	return res
}

// xflow/README.md XFlow.RollbackTimeout：「回滚全部步骤的总预算」，「这份预算对不看 ctx 的 Rollback 同样有效：
// 到点就不再等它，那一步记进 RollbackErrors（错误里带 context.DeadlineExceeded），没轮到的步骤也逐个记下，
// Execute 随即返回」；写 0 启动失败。
// 流程三步 first → hang → fail，fail 失败后逆序回滚：hang 的 Rollback 不看 ctx 地睡 4s
func TestCoverage_ExecuteReturnsAtRollbackTimeoutWhenRollbackIgnoresCtx(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: "XFlow:\n  RollbackTimeout: 1s\n"})

	t.Run("挂 4s，预算 1s", func(t *testing.T) {
		start := time.Now()
		res := covFlow(t, p, 4000)
		took := time.Since(start)
		if res.Success || !res.Rolled {
			t.Fatalf("fail 那一步失败，流程应失败并回滚过，实际 %+v", res)
		}
		if took < time.Second-50*time.Millisecond || took > time.Second+faultSlack {
			t.Errorf("RollbackTimeout: 1s 时 Execute 应在 1s 左右返回，不等挂住的 4s，实际 %v", took)
		}
		if len(res.RollbackErrors) != 2 {
			t.Fatalf("挂住的 hang 和没轮到的 first 都该记进 RollbackErrors，实际 %q", res.RollbackErrors)
		}
		if e := res.RollbackErrors[0]; !strings.Contains(e, `"hang"`) || !strings.Contains(e, "context deadline exceeded") {
			t.Errorf("挂住的那一步记进 RollbackErrors、带 context.DeadlineExceeded，实际 %q", e)
		}
		if e := res.RollbackErrors[1]; !strings.Contains(e, `"first"`) || !strings.Contains(e, "never ran") {
			t.Errorf("没轮到的 first 也要逐个记下，实际 %q", e)
		}
		t.Logf("数字：Rollback 挂 4s、RollbackTimeout 1s：Execute %v 返回（服务端量 %dms）", took.Round(time.Millisecond), res.ElapsedMS)
	})

	t.Run("回滚在预算内做完", func(t *testing.T) {
		res := covFlow(t, p, 200)
		if !res.Rolled || len(res.RollbackErrors) != 0 || res.ElapsedMS < 200 || res.ElapsedMS > 200+faultSlack.Milliseconds() {
			t.Errorf("200ms 的回滚在 1s 预算内应做完、没有 RollbackErrors，实际 %+v", res)
		}
	})

	t.Run("RollbackTimeout: 0 启动失败", func(t *testing.T) {
		stderr := covStartupError(t, harness.Options{Overlay: "XFlow:\n  RollbackTimeout: 0s\n"})
		faultMustContain(t, "RollbackTimeout: 0s 的启动错误", stderr, "RollbackTimeout must be > 0")
	})
}

// xflow/README.md XFlow.Monitor：「关掉之后 Execute 一次监控回调都不走」。默认的监控写 slog：
// 失败的步骤记 WARN，回滚没做完记 ERROR（xflow/monitor.go slogMonitor）
func TestCoverage_XflowMonitorOnLogsStepsAndFlow_OffLogsNothing(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	isFlowLog := func(l harness.Log) bool {
		return strings.HasPrefix(l.Msg(), "xflow ") && l.Str("flow") == "cov_hang_rollback"
	}
	for _, on := range []bool{true, false} {
		name := map[bool]string{true: "Monitor: true（默认）", false: "Monitor: false"}[on]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			overlay := "XFlow:\n  RollbackTimeout: 1s\n"
			if !on {
				overlay += "  Monitor: false\n"
			}
			p := harness.Start(t, harness.Options{Overlay: overlay})
			r := p.PostJSON(t, "/probe/flow", map[string]int{"hang_ms": 1500})
			accessLog(t, p, traceIDOf(t, r)) // 界碑：Execute 返回之后才有访问日志，监控日志要打早就打了
			logs := p.FindLogs(isFlowLog)
			if !on {
				if len(logs) != 0 {
					t.Errorf("Monitor: false 时一次监控回调都不走，实际有 %d 条 xflow 日志：%s", len(logs), logs[0].Line)
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
					t.Errorf("Monitor 开着时应有一条 %s 的 %q，实际 %d 条 xflow 日志", level, msg, len(logs))
				}
			}
		})
	}
}
