package xlog

import (
	"context"
	"log/slog"

	"github.com/jupiter-eng/xone/internal/logext"
)

// TraceExtractor takes the trace identifiers out of a context.
//
// It is this package's only extension point, injected by xtrace during initialization. The contract:
//   - passing nil cancels the injection
//   - a later injection replaces an earlier one (there should only ever be one tracing system)
//   - without an injection the logs carry no trace field and nothing is reported as an error
//
// That keeps xlog free of any dependency on OpenTelemetry. The injection point itself lives in the core's
// internal/logext, which xtrace writes to without importing xlog: a program using only an integration such as xgorm does not get xlog pulled in.
type TraceExtractor = logext.TraceExtractor

// SetTraceExtractor injects the trace identifier extractor.
func SetTraceExtractor(f TraceExtractor) { logext.SetTraceExtractor(f) }

// Observer observes every log line actually written. It is injected by packages such as xmetric.
//
// It is called only when the log passes the level filter and is really going to be written, and the record it
// receives is the one that goes into the output. A panic inside an observer is isolated: a problem in observation must not interrupt the logging itself.
//
// The list is append-only -- an observer lives as long as the process and there is no unregistering.
type Observer = logext.Observer

// AddObserver injects a log observer.
//
// xmetric uses it to count Error-level logs without xlog having to know about Prometheus the other way round.
//
// Why not wrap a slog.Handler from outside: slog.SetDefault also routes the standard library log package's output
// into the new handler, so "wrap it and set it back" can form a cycle -- the record flows back through log.Output
// into the same handler and deadlocks on the log package's non-reentrant lock.
// Having xlog hold the extension point itself avoids that.
func AddObserver(o Observer) { logext.AddObserver(o) }

// notify hands the record to every observer, isolating panics one by one.
func notify(ctx context.Context, r slog.Record) {
	for _, o := range logext.Observers() {
		callObserver(o, ctx, r)
	}
}

func callObserver(o Observer, ctx context.Context, r slog.Record) {
	defer func() {
		if rec := recover(); rec != nil {
			warnf("observer panic: %v", rec)
		}
	}()
	o(ctx, r)
}

// TraceIDs returns the trace identifiers for the current ctx, that is the two values this package writes into the logs.
//
// It is for packages that need the trace identifiers without depending on OpenTelemetry.
// It returns two empty strings when no extractor is injected or the ctx holds no valid Span.
func TraceIDs(ctx context.Context) (traceID, spanID string) { return logext.TraceIDs(ctx) }

// groupOrAttrs records one WithGroup or WithAttrs call, for replaying the call chain once a group has been opened.
type groupOrAttrs struct {
	group string      // non-empty means this was a WithGroup
	attrs []slog.Attr // valid when group is empty
}

// ctxHandler adds the fields carried in the context on top of a standard library handler.
//
// It does one thing, adding fields, and leaves formatting, level filtering and grouping entirely to the wrapped
// slog.JSONHandler / slog.TextHandler -- they already do it well and there is no reason to rewrite them.
//
// Grouping is the one complication: the standard library puts a record's attributes into the group currently open,
// so after logger.WithGroup("g") a trace_id would become g.trace_id.
// But trace_id is the identity of the whole record and belongs to none of the user's groups, so it has to stay at the top level.
// Once a group has been opened it therefore takes the slow path of "attach the ctx fields to base, then replay the
// call chain"; without a group the attributes are at the top level anyway and the fast path costs nothing extra.
type ctxHandler struct {
	next    slog.Handler   // the handler with With/WithGroup already applied
	base    slog.Handler   // the original handler with no With/WithGroup applied
	chain   []groupOrAttrs // the call chain from base to next, meaningful only while grouped
	grouped bool           // whether WithGroup has ever been called
}

func newCtxHandler(h slog.Handler) *ctxHandler {
	return &ctxHandler{next: h, base: h}
}

func (h *ctxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	notify(ctx, r)

	attrs := ctxAttrs(ctx)
	if len(attrs) == 0 {
		return h.next.Handle(ctx, r) // the common case: no ctx fields at all, nothing extra
	}
	if !h.grouped {
		r.AddAttrs(attrs...) // without a group, a record's attributes are already at the top level
		return h.next.Handle(ctx, r)
	}
	return h.replay(attrs).Handle(ctx, r)
}

// replay attaches the ctx fields at the outermost level and then replays the With/WithGroup call chain in order.
// One handler is rebuilt per record, a cost only a logger that has used WithGroup ever pays.
func (h *ctxHandler) replay(attrs []slog.Attr) slog.Handler {
	out := h.base.WithAttrs(attrs)
	for _, g := range h.chain {
		if g.group != "" {
			out = out.WithGroup(g.group)
		} else {
			out = out.WithAttrs(g.attrs)
		}
	}
	return out
}

func (h *ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.fork(groupOrAttrs{attrs: attrs}, h.next.WithAttrs(attrs), h.grouped)
}

func (h *ctxHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.fork(groupOrAttrs{group: name}, h.next.WithGroup(name), true)
}

// fork derives a new handler. The call chain has to be recorded unconditionally: a group may appear several Withs
// later, and the attributes attached before it have to take part in the replay too -- missing them would lose fields out of nowhere on the slow path.
// A new slice is built every time rather than reusing append, so two loggers derived from the same parent do not overwrite each other.
func (h *ctxHandler) fork(g groupOrAttrs, next slog.Handler, grouped bool) *ctxHandler {
	chain := make([]groupOrAttrs, len(h.chain), len(h.chain)+1)
	copy(chain, h.chain)
	return &ctxHandler{next: next, base: h.base, chain: append(chain, g), grouped: grouped}
}

// ctxAttrs gathers every field carried in the context.
// Gathering them before AddAttrs: slog.Record inlines only the first 5 attributes, and adding them one by one would grow the underlying slice repeatedly.
func ctxAttrs(ctx context.Context) []slog.Attr {
	s := scopeFrom(ctx)
	n := 0
	if s != nil {
		n = s.Len()
	}

	// Take the trace identifiers first, then decide whether to allocate this slice.
	// "an extractor is installed but this ctx holds no Span" is very common -- startup logs, background jobs and
	// scheduled jobs all are. Allocating first would cost those logs one wasted allocation per line,
	// for an empty slice.
	traceID, spanID := TraceIDs(ctx)
	if n == 0 && traceID == "" {
		return nil
	}

	attrs := make([]slog.Attr, 0, n+2)
	if traceID != "" {
		attrs = append(attrs, slog.String("trace_id", traceID))
		if spanID != "" {
			attrs = append(attrs, slog.String("span_id", spanID))
		}
	}
	if s != nil {
		s.Each(func(k string, v any) { attrs = append(attrs, slog.Any(k, v)) })
	}
	return attrs
}
