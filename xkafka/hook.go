package xkafka

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/jupiter-eng/xone/xkafka"

// franz-go's own kotel plugin is not used: it writes the message key into a Span attribute
// (messaging.kafka.message.key), and a key often holds a user id, a phone number or an order number, which would
// then reach the tracing backend. Only the attributes below are written, on the same principle that keeps xredis and xgorm from writing parameters onto a Span.

// produceHook is the producing hook attached to every client:
//   - when a message enters the buffer (the moment Produce / ProduceSync is called) it writes the trace context into the message headers and opens a Producer Span;
//   - when the message has a result it ends the Span, recording a WARN on failure.
//
// The Span opens at the moment of buffering and ends when the broker answered (or it gave up): it measures the whole
// stretch from sending to settling, batching (Linger), queueing and retries included, which matches the duration a ProduceSync caller sees.
// What goes into the message headers is this Span's context, which is why the consuming side's process Span hangs below it.
type produceHook struct {
	name  string // the cluster name; empty for one built by calling New directly
	trace bool
	log   bool
}

var (
	_ kgo.HookProduceRecordBuffered   = produceHook{}
	_ kgo.HookProduceRecordUnbuffered = produceHook{}
)

func (h produceHook) OnProduceRecordBuffered(r *kgo.Record) {
	ctx := r.Context // franz-go has already put the ctx Produce received in here before calling this hook, so it is never nil
	if h.trace {
		ctx, _ = otel.Tracer(tracerName).Start(ctx, r.Topic+" publish",
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.operation.name", "publish"),
				attribute.String("messaging.destination.name", r.Topic),
			))
		r.Context = ctx // the Span is taken back from here once the result arrives
	}
	// Injected even with Trace off: no Span is opened while the trace context (and the trusted forwarded headers and baggage) still travels downstream,
	// the same as what xhttp does with Trace off.
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier{r: r})
}

func (h produceHook) OnProduceRecordUnbuffered(r *kgo.Record, err error) {
	if h.trace {
		span := trace.SpanFromContext(r.Context)
		if err != nil {
			// Only the status, not the error text, as in xcron: the text is in the log.
			span.SetStatus(codes.Error, "kafka produce failed")
		} else {
			span.SetAttributes(
				attribute.Int("messaging.destination.partition.id", int(r.Partition)),
				attribute.Int64("messaging.kafka.offset", r.Offset),
			)
		}
		span.End()
	}
	if err != nil && h.log {
		// The value is not recorded. For one that failed before being assigned a partition (the topic does not exist) partition is 0 (measured on kfake), which says nothing.
		attrs := append(nameAttr(h.name), "topic", r.Topic, "partition", r.Partition)
		attrs = append(attrs, keyAttrs(r.Key)...)
		slog.WarnContext(r.Context, "kafka produce failed", append(attrs, "error", err)...)
	}
}

// nameAttr is the cluster name; one built by calling New directly has no name and the field is not written.
func nameAttr(name string) []any {
	if name == "" {
		return nil
	}
	return []any{"name", name}
}

// headerCarrier treats the message headers as an OTel TextMapCarrier.
//
// Set replaces a header of the same name rather than appending: a dead-letter message is written again carrying the
// original message's headers, and appending would give two traceparents, leaving the downstream guessing which one it gets.
type headerCarrier struct{ r *kgo.Record }

func (c headerCarrier) Get(key string) string {
	for _, h := range c.r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c headerCarrier) Set(key, value string) {
	for i, h := range c.r.Headers {
		if h.Key == key {
			c.r.Headers[i].Value = []byte(value)
			return
		}
	}
	c.r.Headers = append(c.r.Headers, kgo.RecordHeader{Key: key, Value: []byte(value)})
}

func (c headerCarrier) Keys() []string {
	keys := make([]string, len(c.r.Headers))
	for i, h := range c.r.Headers {
		keys[i] = h.Key
	}
	return keys
}

// TrustedPeer: the forwarded headers and baggage in the message headers are accepted, see xtrace.HeaderPropagator.Extract.
//
// xtrace accepts those two only from a trusted peer, and on the HTTP side a trusted peer is a directly connected address in TrustedProxies.
// A message has no "directly connected peer": whoever can write into the topic holds this cluster's credentials -- like a row in a database,
// whoever wrote it is one of ours; and what this module injects when producing is only what this process already accepted (given by a trusted upstream, or written by itself).
// So it counts as trusted. For a topic open to outside writes (a third party writing into it directly), do not rely on forwarded headers or baggage in its consumer.
// traceparent is not governed by this: it is only a trace identifier and xtrace accepts it from anybody.
func (headerCarrier) TrustedPeer() bool { return true }

// slogLogger routes franz-go's own logs into slog, accepting WARN and above only.
//
// franz-go v1.21.7 logs nothing by default (nopLogger): a failed connection, a failed authentication and a rebalance error all pass in silence,
// and the caller only learns of it when ProduceSync returns an error, or when nothing arrives at all.
// INFO / DEBUG produce several lines per connection, per rebalance and per commit, which is far too much, so they are not accepted.
// Each client wires up its own (kgo.WithLogger) and no global is touched.
type slogLogger struct{ name string }

func (l slogLogger) Level() kgo.LogLevel { return kgo.LogLevelWarn }

func (l slogLogger) Log(level kgo.LogLevel, msg string, keyvals ...any) {
	lv := slog.LevelWarn
	if level == kgo.LogLevelError {
		lv = slog.LevelError
	}
	// franz-go's keyvals alternate key and value, with keys in English snake_case (broker, err, group, ...),
	// used as fields unchanged. franz-go's message goes into detail and the key-values into a fields group, so they do not collide with fields the logging platform already has (msg, error, name).
	attrs := make([]any, 0, len(keyvals)/2)
	for i := 0; i+1 < len(keyvals); i += 2 {
		attrs = append(attrs, slog.Any(fmt.Sprint(keyvals[i]), keyvals[i+1]))
	}
	slog.Log(context.Background(), lv, "xkafka franz-go log", append(nameAttr(l.name),
		"detail", msg, slog.Group("fields", attrs...))...)
}

// partitionStr: the metric labels have no use for the partition, so this only builds "topic/partition" for logs and errors.
func partitionStr(topic string, partition int32) string {
	return topic + "/" + strconv.Itoa(int(partition))
}
