package xredis

import (
	"context"
	"slices"
	"strings"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// redisotel v9.22.0 records an error as span.RecordError(err) plus span.SetStatus(codes.Error, err.Error())
// (tracing.go recordError), which puts the server's text into exception.message and the status description --
// and that text carries parameters (see redactedError). It has no option for rewriting the error, so the Span it
// receives is wrapped here: both places are replaced by the text from the command log. The rest of the Span (attributes, events, End) is passed through to the inner one.

// redactingTracerProvider is the TracerProvider handed to redisotel; every Span it emits is a redactingSpan.
type redactingTracerProvider struct{ trace.TracerProvider }

func (p redactingTracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return redactingTracer{p.TracerProvider.Tracer(name, opts...)}
}

type redactingTracer struct{ trace.Tracer }

// Start renames the Span to what spanName gives; what goes into the ctx is still the inner Span, so later hooks (the command log's trace_id / span_id) still take it from the ctx.
func (t redactingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := t.Tracer.Start(ctx, spanName(ctx, name), opts...)
	return ctx, &redactingSpan{Span: span}
}

// redisotel v9.22.0 names a Span cmd.FullName(), and a pipeline "redis.pipeline " plus each command's FullName
// (deduplicated, at most 10, rediscmd.CmdsString). FullName is the first argument lowercased as it is, unvalidated: measured,
// Do(ctx, "SET k1 <value>") produced the Span name "set k1 <value>", and inside a pipeline "redis.pipeline set k2 <value>".
// The name alone does not say which parts of a pipeline are one command, so spanCmdsHook hangs outside redisotel and puts the commands into the ctx,
// and the renaming here goes by command, following the same rule as the command log's cmd field (cmdName).

// spanCmdsKey holds either a redis.Cmder (one command) or a []redis.Cmder (a pipeline) in the ctx.
type spanCmdsKey struct{}

// spanCmdsHook hangs before redisotel's hook (go-redis puts the one added first on the outside) and leaves this round's commands for redactingTracer.
type spanCmdsHook struct{}

func (spanCmdsHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (spanCmdsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		return next(context.WithValue(ctx, spanCmdsKey{}, cmd), cmd)
	}
}

func (spanCmdsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		return next(context.WithValue(ctx, spanCmdsKey{}, cmds), cmds)
	}
}

// spanName only renames what redisotel named after a command; one that does not come from a command, such as redis.dial, is left alone
// (a connection is made inside a command's hook, so the ctx holds a command there too, which is why the name is checked first).
func spanName(ctx context.Context, name string) string {
	switch v := ctx.Value(spanCmdsKey{}).(type) {
	case redis.Cmder:
		if name == v.FullName() {
			return spanCmdName(v)
		}
	case []redis.Cmder:
		if strings.HasPrefix(name, "redis.pipeline ") {
			names := make([]string, 0, min(len(v), 10))
			for _, c := range v {
				if n := spanCmdName(c); !slices.Contains(names, n) && len(names) < 10 {
					names = append(names, n)
				}
			}
			return "redis.pipeline " + strings.Join(names, " ")
		}
	}
	return name
}

// spanCmdName is one command's Span name: the command name follows cmdName, and a subcommand carried in FullName (cluster info, command count)
// is kept only when it looks like a command name too.
func spanCmdName(cmd redis.Cmder) string {
	name := cmdName(cmd)
	if _, sub, ok := strings.Cut(cmd.FullName(), " "); ok && name != invalidCmdName && validCmdName.MatchString(sub) {
		return name + " " + sub
	}
	return name
}

// redactingSpan records only the text redactedError gives when recording an error.
// One Span is used inside one command's hook and is never used concurrently.
type redactingSpan struct {
	trace.Span
	text string // the redacted text of the last RecordError, used by SetStatus
}

// RecordError records an event as usual for the ones whose text may be recorded verbatim (network errors, timeouts) and records nothing otherwise: the event's exception.message is exactly that text.
func (s *redactingSpan) RecordError(err error, opts ...trace.EventOption) {
	s.text, _ = redactedError(err)
	if clientSafe(err) {
		s.Span.RecordError(err, opts...)
	}
}

// SetStatus replaces the description on an error with the text RecordError redacted; without a preceding RecordError there is no telling what the description holds, so no text is recorded at all.
func (s *redactingSpan) SetStatus(code codes.Code, desc string) {
	if code == codes.Error {
		desc = s.text
		if desc == "" {
			desc = "redis error (message omitted, it may contain argument values)"
		}
	}
	s.Span.SetStatus(code, desc)
}
