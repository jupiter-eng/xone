package xcron

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	// Not a blank import: the job field goes into the ctx through xlog.CtxWithKV. The side effect is the same as in
	// xgin -- using xcron brings xlog, so a process running only scheduled jobs still logs JSON with a trace_id.
	"github.com/jupiter-eng/xone/xlog"
)

const tracerName = "github.com/jupiter-eng/xone/xcron"

// execute runs the job once; every execution of an Add goes through here.
//
// What it does on fn's behalf: opens a root Span (cron <name>), puts the log field job=<name> into the ctx, applies
// the timeout, records one line of result, and catches panics. It returns fn's error; a panic becomes a returned error.
//
// The log levels: the start is DEBUG, the end INFO, a failure WARN and a panic ERROR. The start line is not INFO
// because an @every 1s job would mean two lines every second, while the end line already carries elapsed_ms and the
// start time follows from it; a job that hangs shows no end line, and the error on shutdown names the jobs still
// running (see scheduler.stop). As with xgin's access log: one line per execution.
//
// The job field goes only into the ctx's logging scope and is not written again here: writing it in both places would put two keys of the same name in the JSON.
//
// The Span carries only the status and not the error text (RecordError is not called), as in xgorm / xredis: the
// error a job returns often wraps an underlying SQL or Redis error whose text carries parameter values, which xgorm has just gone to some trouble to keep out of traces. The text is in the log.
func execute(parent context.Context, name string, timeout time.Duration, fn func(context.Context) error) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(parent, "cron "+name, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	ctx = xlog.CtxWithKV(ctx, map[string]any{"job": name})
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	start := time.Now()
	slog.DebugContext(ctx, "cron job started")
	defer func() {
		elapsed := ms(time.Since(start))
		if r := recover(); r != nil {
			err = panicked(r)
			span.SetStatus(codes.Error, "cron job panicked")
			slog.ErrorContext(ctx, "cron job panicked", "elapsed_ms", elapsed, "error", err, "stack", string(debug.Stack()))
			return
		}
		if err != nil {
			span.SetStatus(codes.Error, "cron job failed")
			slog.WarnContext(ctx, "cron job failed", "elapsed_ms", elapsed, "error", err)
			return
		}
		slog.InfoContext(ctx, "cron job finished", "elapsed_ms", elapsed)
	}()
	return fn(ctx)
}

// panicked turns a recovered value into an error, catching one that is already an error with %w so errors.Is / errors.As still work.
func panicked(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", r)
}

// ms converts an elapsed time to milliseconds, keeping microseconds. The field name carries the unit: slog's JSON writes a Duration as an integer number of nanoseconds.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
