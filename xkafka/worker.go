package xkafka

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/logext"
	"github.com/jupiter-eng/xone/xutil"
)

// The headers a dead-letter message carries on top of the original message's.
const (
	HeaderDLQTopic     = "xkafka-dlq-topic"     // the original topic
	HeaderDLQPartition = "xkafka-dlq-partition" // the original partition, in decimal
	HeaderDLQOffset    = "xkafka-dlq-offset"    // the original offset, in decimal
	HeaderDLQGroup     = "xkafka-dlq-group"     // the consumer group that failed to process it
	HeaderDLQError     = "xkafka-dlq-error"     // the last error text, cut to maxDLQError bytes
)

// maxDLQError is how many bytes of error text a dead-letter header carries at most.
//
// The error text comes from the handler and may hold SQL, a downstream response or fields from the message -- all of which land in the dead-letter topic with it, visible to anybody who can read it.
// Cutting it short only keeps the headers from growing and does not keep anything out: a dead-letter topic holds the
// original message's value anyway, so its read permissions have to be as tight as the original topic's. Do not put credentials into an error.
const maxDLQError = 256

var (
	// retryInterval is the upper bound on the first backoff between two handler attempts, doubling after that (xutil.Retry). It is a variable only so tests can shorten it
	retryInterval = time.Second

	// dlqInterval is the upper bound on the first backoff when a dead-letter write fails, doubling with jitter after that, capped at dlqMaxInterval. A variable for the same reason
	dlqInterval    = time.Second
	dlqMaxInterval = 30 * time.Second
)

// worker is one partition's goroutine: it processes the messages dispatched to it one at a time, in order.
type worker struct {
	c         *consumer
	partition int32
	recs      chan []*kgo.Record

	// stopping means the partition was revoked or the consumer is stopping: no next message is processed, no next retry
	// is started and no dead-letter retry is waited for. The handler call in flight is unaffected by it.
	stopping context.Context
	quit     context.CancelFunc
	done     chan struct{}

	// inflight is the offset + 1 of the message being processed, 0 when idle. It is used to name it when the stop budget runs out.
	inflight atomic.Int64
}

func newWorker(c *consumer, partition int32) *worker {
	w := &worker{c: c, partition: partition, recs: make(chan []*kgo.Record, workerQueue), done: make(chan struct{})}
	// The stop budget running out (hard) counts too: revokedFn may not have had time to revoke the partition by then.
	w.stopping, w.quit = context.WithCancel(c.hard)
	return w
}

// run is this partition's goroutine. It exits when the partition is revoked, when the consumer is stopping,
// or -- the case the recover below is for -- when this module's own code blows up.
func (w *worker) run() {
	defer func() {
		// A panic here is this module's code, not the handler's: that one is caught by call and counted as one
		// failed attempt. Uncontained, a panic in a goroutine takes the whole process down, which is exactly
		// what the framework's other lifecycle entry points (xone's safeHook, xclient's safeNew, xflow's
		// safeProcess) exist to prevent -- one partition must not be able to kill the service.
		// Nothing was marked, so this partition's records come back after the next rebalance.
		if p := recover(); p != nil {
			slog.ErrorContext(w.c.base, "kafka partition worker panicked, this partition stops until the next rebalance",
				"topic", w.c.topic, "partition", w.partition, "group", w.c.group,
				"error", panicked(p), "stack", string(debug.Stack()))
		}
		w.c.retire(w)
		close(w.done)
	}()
	for {
		select {
		case <-w.stopping.Done():
			return // whatever is still queued is dropped: nothing was marked, so whoever gets this partition next continues from here
		case batch := <-w.recs:
			for _, r := range batch {
				if w.stopping.Err() != nil || !w.process(r) {
					// This unfinished message was not marked, and the ones after it must not be processed either -- marking them would commit an offset past it
					return
				}
			}
		}
	}
}

// outcome is how a message ended up, and is also the metric's status label.
type outcome string

const (
	outcomeOK         outcome = "ok"
	outcomeDeadLetter outcome = "dead_letter"
	outcomeSkipped    outcome = "skipped"
	outcomeAborted    outcome = "aborted" // unfinished when shutting down or when the partition was revoked; not marked, redelivered later
)

// process handles one message: it calls the handler (with retries), writes a dead letter or skips on failure, and then marks it.
// It returns false when it was stopped before finishing (and nothing was marked).
func (w *worker) process(r *kgo.Record) bool {
	c := w.c
	w.inflight.Store(r.Offset + 1)
	defer w.inflight.Store(0)
	start := time.Now()

	// The trace continues from the producer: the parent Span comes from the message headers. base is not cancelled on shutdown, so a message in flight is finished.
	ctx := otel.GetTextMapPropagator().Extract(c.base, headerCarrier{r: r})
	if c.trace {
		var span trace.Span
		ctx, span = otel.Tracer(tracerName).Start(ctx, r.Topic+" process",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.operation.name", "process"),
				attribute.String("messaging.destination.name", r.Topic),
				attribute.Int("messaging.destination.partition.id", int(r.Partition)),
				attribute.Int64("messaging.kafka.offset", r.Offset),
				attribute.String("messaging.consumer.group.name", c.group),
			))
		defer span.End()
	}

	// This module's own logs write the fields onto every line (carried even without xlog installed); the ctx the handler receives holds a scope with the same fields,
	// so with xlog installed every line it writes carries them. The two use different ctxs: writing them twice in one line would be two keys of the same name.
	fields := []any{"topic", r.Topic, "partition", r.Partition, "offset", r.Offset, "group", c.group}
	fields = append(fields, keyAttrs(r.Key)...)
	hctx := logext.WithKV(ctx, map[string]any{"topic": r.Topic, "partition": r.Partition, "offset": r.Offset, "group": c.group})

	res := w.handle(ctx, hctx, r, fields)
	if res == outcomeAborted {
		c.observe(r.Topic, res, time.Since(start))
		return false
	}
	if res != outcomeOK {
		trace.SpanFromContext(ctx).SetStatus(codes.Error, "kafka message failed")
	}
	c.cl.MarkCommitRecords(r)
	c.observe(r.Topic, res, time.Since(start))
	if res == outcomeOK && c.log {
		slog.DebugContext(ctx, "kafka message processed", append(fields, "elapsed_ms", ms(time.Since(start)))...)
	}
	return true
}

// handle calls the handler, retries on failure, and writes a dead letter or skips once the retries are used up.
func (w *worker) handle(ctx, hctx context.Context, r *kgo.Record, fields []any) outcome {
	c := w.c
	attempts := 0
	err := xutil.Retry(w.stopping, c.o.retries+1, c.o.timeout, retryInterval, func(attempt context.Context) error {
		attempts++
		start := time.Now()
		last := c.call(hctx, attempt, r, fields) // as the handler returned it, possibly wrapped in xutil.Permanent
		if last != nil && c.log {
			slog.WarnContext(ctx, "kafka message failed", append(slices.Clone(fields),
				"attempt", attempts, "elapsed_ms", ms(time.Since(start)), "error", last)...)
		}
		return last
	})
	switch {
	case err == nil:
		return outcomeOK
	// Shutting down, or this partition revoked: the record is left unmarked and comes back later.
	// attempts == 0 is the same thing one step earlier -- xutil.Retry makes no attempt at all on a ctx that
	// was already cancelled when it was entered, so there is no failure of the handler's to report.
	//
	// It is asked of stopping rather than worked out from the error xutil.Retry handed back. Working it out
	// meant comparing that error against the one the handler returned last, and comparing two interface
	// values panics outright when their dynamic type is not comparable -- an error type such as
	// validator.ValidationErrors ([]FieldError) or mongo's WriteException took the whole process down from
	// inside this goroutine as soon as a message used up its retries. Whether this message was interrupted
	// is what stopping says, and nothing has to be inferred from an error value.
	case attempts == 0 || w.stopping.Err() != nil:
		return outcomeAborted
	}

	if c.o.dlq == "" {
		slog.ErrorContext(ctx, "kafka message skipped, retries exhausted and dead lettering is off",
			append(fields, "attempts", attempts, "error", err)...)
		return outcomeSkipped
	}
	if !w.deadLetter(ctx, r, err, fields) {
		return outcomeAborted
	}
	slog.WarnContext(ctx, "kafka message dead-lettered",
		append(fields, "dlq_topic", c.o.dlq, "attempts", attempts, "error", err)...)
	return outcomeDeadLetter
}

// call makes one call to the handler.
//
// The ctx deadline is this attempt's (WithTimeout) but it is not cancelled with attempt: attempt's parent is stopping,
// which is cancelled on shutdown and on revocation, while the work in flight has to finish. Only the stop budget running out (hard) cancels it.
// A panic is caught and counted as one failure, with the stack recorded at ERROR.
func (c *consumer) call(hctx, attempt context.Context, r *kgo.Record, fields []any) (err error) {
	ctx, cancel := context.WithCancel(hctx)
	if dl, ok := attempt.Deadline(); ok {
		ctx, cancel = context.WithDeadline(hctx, dl)
	}
	defer cancel()
	defer context.AfterFunc(c.hard, cancel)()

	defer func() {
		if p := recover(); p != nil {
			err = panicked(p)
			slog.ErrorContext(ctx, "kafka handler panicked", append(slices.Clone(fields),
				"error", err, "stack", string(debug.Stack()))...)
		}
	}()
	return c.fn(ctx, r)
}

// deadLetter writes the original message (key, value, headers) into the dead-letter topic with a few extra headers saying where it came from and why it failed.
//
// A failed write is retried forever (backing off with doubling and jitter, capped at 30s), with nothing marked and nothing processed after it: dropping it would be losing a message silently,
// and skipping it to process the next one would commit an offset past it. On shutdown or revocation it gives up (cancelling the write in flight on the spot) and
// returns false -- this message was not marked and is redelivered later.
func (w *worker) deadLetter(ctx context.Context, r *kgo.Record, cause error, fields []any) bool {
	c := w.c
	for wait := dlqInterval; ; wait = min(wait*2, dlqMaxInterval) {
		if w.stopping.Err() != nil {
			return false // the last retry met the shutdown: no write is started, and after a restart this message starts over
		}
		// This write is cancelled with stopping too: it is this module's own I/O, not something the handler is doing.
		// One stopped halfway leaves the message unmarked and redelivered later (which may leave a duplicate in the dead-letter topic).
		// Writing to a topic that does not exist, franz-go v1.21.7 reports an error after about 1s the first time and takes about 10s per attempt after that (measured on kfake), which cannot just be waited out.
		pctx, cancel := context.WithTimeout(ctx, c.o.timeout)
		stop := context.AfterFunc(w.stopping, cancel)
		err := c.cl.ProduceSync(pctx, dlqRecord(r, c.o.dlq, c.group, cause)).FirstErr()
		stop()
		cancel()
		if err == nil {
			return true
		}
		slog.WarnContext(ctx, "kafka dead letter produce failed, retrying", append(slices.Clone(fields),
			"dlq_topic", c.o.dlq, "error", err)...)
		select {
		case <-w.stopping.Done():
			return false
		case <-time.After(rand.N(wait + 1)):
		}
	}
}

// dlqRecord is the dead-letter message: the original message's key, value and headers as they are, plus the HeaderDLQ* headers.
// A new one is built for each retry: franz-go rewrites the Record handed to it (partition, offset, ctx).
func dlqRecord(r *kgo.Record, topic, group string, cause error) *kgo.Record {
	headers := slices.Clone(r.Headers)
	headers = append(headers,
		kgo.RecordHeader{Key: HeaderDLQTopic, Value: []byte(r.Topic)},
		kgo.RecordHeader{Key: HeaderDLQPartition, Value: []byte(strconv.Itoa(int(r.Partition)))},
		kgo.RecordHeader{Key: HeaderDLQOffset, Value: []byte(strconv.FormatInt(r.Offset, 10))},
		kgo.RecordHeader{Key: HeaderDLQGroup, Value: []byte(group)},
		kgo.RecordHeader{Key: HeaderDLQError, Value: []byte(truncate(strings.ToValidUTF8(cause.Error(), "?"), maxDLQError))},
	)
	return &kgo.Record{Topic: topic, Key: r.Key, Value: r.Value, Headers: headers}
}

// panicked turns a recovered value into an error, catching one that is already an error with %w, as in xcron.
func panicked(p any) error {
	if err, ok := p.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", p)
}
