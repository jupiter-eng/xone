package xcron

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/xlog"
)

// fresh 换一个干净的全局调度器（Add 用的就是它），测试结束时停掉它、换回原来的。
// 用了它的测试不能 t.Parallel
func fresh(t *testing.T) *scheduler {
	t.Helper()
	old := std
	s := &scheduler{}
	std = s
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.stop(ctx)
		std = old
	})
	return s
}

// started 起调度器，测试结束时（fresh 的清理里）停掉
func started(t *testing.T, s *scheduler) {
	t.Helper()
	if err := s.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// logLine 一行 JSON 日志
type logLine map[string]any

// captureLogs 把默认 logger 换成真的 xlog（job 字段、trace_id 是它的 handler 从 ctx 里取的），
// 级别开到 debug；返回的函数随时读出到目前为止的全部日志
func captureLogs(t *testing.T) func() []logLine {
	t.Helper()
	dir := t.TempDir()
	c := xlog.DefaultConfig()
	c.Console = false
	c.Level = "debug"
	c.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}
	l, closer, err := xlog.New(c)
	if err != nil {
		t.Fatal(err)
	}
	old := slog.Default()
	slog.SetDefault(l)
	xlog.SetTraceExtractor(func(ctx context.Context) (string, string) {
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			return "", ""
		}
		return sc.TraceID().String(), sc.SpanID().String()
	})
	t.Cleanup(func() { slog.SetDefault(old); xlog.SetTraceExtractor(nil); closer.Close() })
	return func() []logLine {
		files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
		var out []logLine
		for _, f := range files {
			b, _ := os.ReadFile(f)
			for _, s := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				var l logLine
				if json.Unmarshal([]byte(s), &l) == nil {
					out = append(out, l)
				}
			}
		}
		return out
	}
}

// withMsg 挑出 msg 是这一句的日志
func withMsg(lines []logLine, msg string) []logLine {
	var out []logLine
	for _, l := range lines {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// captureSpans 换一个同步导出到内存的 TracerProvider
func captureSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	return exp
}

// eventually 等 cond 成立，最多 5s：调度在别的协程里，结果要等一会儿才看得到
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("5s 内没等到：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// release 测试结束时关掉 ch，放走卡在它上面的任务（在 fresh 停调度器之前）
func release(t *testing.T) chan struct{} {
	ch := make(chan struct{})
	t.Cleanup(func() { close(ch) })
	return ch
}

func ok(context.Context) error { return nil }

// waitCtx 等 ctx 结束，返回它的错误。2s 还没结束就返回一个别的错误：
// 该取消 ctx 的代码被改坏时，测试是红而不是挂住
func waitCtx(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("ctx was never done")
	}
}
