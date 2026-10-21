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

// fresh swaps in a clean global scheduler (the one Add uses) and stops it, restoring the previous one, when the
// test ends. A test using it cannot t.Parallel.
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

// started brings the scheduler up and stops it when the test ends (in fresh's cleanup).
func started(t *testing.T, s *scheduler) {
	t.Helper()
	if err := s.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// logLine is one JSON log line.
type logLine map[string]any

// captureLogs swaps the default logger for a real xlog (whose handler takes the job field and the trace_id out of
// the ctx) with the level at debug; the function returned reads out every log so far at any time.
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

// withMsg picks out the logs whose msg is this one.
func withMsg(lines []logLine, msg string) []logLine {
	var out []logLine
	for _, l := range lines {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// captureSpans swaps in a TracerProvider exporting synchronously into memory.
func captureSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	return exp
}

// eventually waits for cond to hold, for at most 5s: scheduling happens in another goroutine, so the result takes a moment to become visible.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not reached within 5s: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// release closes ch when the test ends, freeing the jobs blocked on it (before fresh stops the scheduler).
func release(t *testing.T) chan struct{} {
	ch := make(chan struct{})
	t.Cleanup(func() { close(ch) })
	return ch
}

func ok(context.Context) error { return nil }

// waitCtx waits for ctx to end and returns its error. After 2s it returns a different error instead, so that code
// broken in a way that leaves the ctx uncancelled makes the test go red rather than hang.
func waitCtx(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("ctx was never done")
	}
}
