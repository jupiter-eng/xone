package xkafka

import (
	"context"
	"errors"
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

// 死信消息在原消息的头之外多带的几个头
const (
	HeaderDLQTopic     = "xkafka-dlq-topic"     // 原 topic
	HeaderDLQPartition = "xkafka-dlq-partition" // 原分区，十进制
	HeaderDLQOffset    = "xkafka-dlq-offset"    // 原 offset，十进制
	HeaderDLQGroup     = "xkafka-dlq-group"     // 处理失败的消费组
	HeaderDLQError     = "xkafka-dlq-error"     // 最后一次的错误文本，截到 maxDLQError 字节
)

// maxDLQError 死信头里的错误文本最多这么多字节。
//
// 错误文本是处理函数返回的，里面可能带着 SQL、下游的响应、消息里的字段——会和消息一起落进死信 topic，
// 谁能读死信谁就能看到。截短只是让它不至于撑大消息头，挡不住内容：死信 topic 本来就装着原消息的 value，
// 它的读权限要和原 topic 一样收紧。别把凭证拼进错误里
const maxDLQError = 256

var (
	// retryInterval 处理函数两次尝试之间第一次退避的上界，之后翻倍（xutil.Retry）。变量只为让测试调短
	retryInterval = time.Second

	// dlqInterval 死信写不进去时第一次退避的上界，之后翻倍、带抖动，最多 dlqMaxInterval。变量理由同上
	dlqInterval    = time.Second
	dlqMaxInterval = 30 * time.Second
)

// worker 一个分区的协程：按顺序一条一条处理派给它的消息
type worker struct {
	c         *consumer
	partition int32
	recs      chan []*kgo.Record

	// stopping 分区被收回、或者消费者在停：不再处理下一条、不再发起下一次重试、不再等死信重试。
	// 在途的那一次处理函数调用不受它影响
	stopping context.Context
	quit     context.CancelFunc
	done     chan struct{}

	// inflight 正在处理的消息的 offset + 1，空闲时为 0。停止预算用完时点名用
	inflight atomic.Int64
}

func newWorker(c *consumer, partition int32) *worker {
	w := &worker{c: c, partition: partition, recs: make(chan []*kgo.Record, workerQueue), done: make(chan struct{})}
	// 停止预算用完（hard）时也算：那时 revokedFn 可能还没来得及收回分区
	w.stopping, w.quit = context.WithCancel(c.hard)
	return w
}

func (w *worker) run() {
	defer func() {
		w.c.mu.Lock()
		delete(w.c.live, w)
		w.c.mu.Unlock()
		close(w.done)
	}()
	for {
		select {
		case <-w.stopping.Done():
			return // 队列里还没处理的不管了：没标记，下一个拿到这个分区的从这里接着处理
		case batch := <-w.recs:
			for _, r := range batch {
				if w.stopping.Err() != nil || !w.process(r) {
					// 没处理完的这一条没标记；它后面的也不能再处理——标记了后面的，提交的 offset 就越过了它
					return
				}
			}
		}
	}
}

// outcome 一条消息最后怎样了，也是指标的 status 标签
type outcome string

const (
	outcomeOK         outcome = "ok"
	outcomeDeadLetter outcome = "dead_letter"
	outcomeSkipped    outcome = "skipped"
	outcomeAborted    outcome = "aborted" // 退出、分区被收回时还没处理完，不标记，之后重新投递
)

// process 处理一条消息：调处理函数（带重试），失败的写死信或跳过，然后标记。
// 返回 false 表示没处理完就被叫停了（没有标记）
func (w *worker) process(r *kgo.Record) bool {
	c := w.c
	w.inflight.Store(r.Offset + 1)
	defer w.inflight.Store(0)
	start := time.Now()

	// 链路接着生产方：父 Span 从消息头里取。base 不随退出取消，在途的消息要处理完
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

	// 本模块自己的日志把字段写在每一行上（没装 xlog 也带着）；处理函数拿到的 ctx 里放的是同样几个字段的作用域，
	// 装了 xlog 时它里面写的每一行日志都带上。两处用不同的 ctx：同一行日志里写两遍就是两个同名的 key
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

// handle 调处理函数，失败了重试，重试用完写死信或者跳过
func (w *worker) handle(ctx, hctx context.Context, r *kgo.Record, fields []any) outcome {
	c := w.c
	attempts := 0
	var last error // 最后一次处理函数返回的错误，原样（可能包着 xutil.Permanent）
	err := xutil.Retry(w.stopping, c.o.retries+1, c.o.timeout, retryInterval, func(attempt context.Context) error {
		attempts++
		start := time.Now()
		last = c.call(hctx, attempt, r, fields)
		if last != nil && c.log {
			slog.WarnContext(ctx, "kafka message failed", append(slices.Clone(fields),
				"attempt", attempts, "elapsed_ms", ms(time.Since(start)), "error", last)...)
		}
		return last
	})
	switch {
	case err == nil:
		return outcomeOK
	case attempts == 0 || interrupted(err, last):
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

// interrupted xutil.Retry 是不是被 stopping 叫停的：那时它返回的既不是最后一次的错误，
// 也不是那个错误 Permanent 里包着的（xutil.Retry 去掉了 Permanent 那一层）
func interrupted(err, last error) bool {
	return err != last && !errors.Is(last, err)
}

// call 调一次处理函数。
//
// ctx 的截止时间是这一次尝试的（WithTimeout），但不跟着 attempt 取消：attempt 的父亲是 stopping，
// 退出、分区被收回时它被取消，而在途的处理要做完。只有停止预算用完（hard）时才取消。
// panic 被接住，当成一次失败；栈记一条 ERROR
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

// deadLetter 把原消息（key、value、头）写进死信 topic，多带几个头说明它从哪来、为什么失败。
//
// 写不进去就一直重试（退避翻倍、带抖动，最多 30s 一次），不标记、不往下处理：丢掉它是静默丢消息，
// 跳过它去处理下一条的话，提交的 offset 就越过了它。退出、分区被收回时放弃（在写的那一次也当场取消），
// 返回 false——这条消息没标记，之后重新投递
func (w *worker) deadLetter(ctx context.Context, r *kgo.Record, cause error, fields []any) bool {
	c := w.c
	for wait := dlqInterval; ; wait = min(wait*2, dlqMaxInterval) {
		if w.stopping.Err() != nil {
			return false // 最后一次重试赶上了退出：不再开始写，重启之后这条从头来
		}
		// 这一次写也随 stopping 取消：它是本模块自己的 I/O，不是处理函数在做的事。
		// 写到一半被叫停的，这条没标记，之后重新投递（死信 topic 里可能因此多一份）。
		// franz-go v1.21.7 往不存在的 topic 写，第一次约 1s 报错，之后每次要等约 10s（kfake 实测），不能干等它
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

// dlqRecord 死信消息：原消息的 key、value、头原样带着，另加 HeaderDLQ* 那几个头。
// 每次重试新建一个：franz-go 会改写交给它的 Record（分区、offset、ctx）
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

// panicked 把 recover 到的值变成 error，本身是 error 的用 %w 接住，同 xcron
func panicked(p any) error {
	if err, ok := p.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", p)
}
