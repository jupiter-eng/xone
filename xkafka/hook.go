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

// 没有用 franz-go 自带的 kotel 插件：它把消息的 key 写进 Span 属性（messaging.kafka.message.key），
// key 里常有用户 ID、手机号、订单号，就此进了链路后端。这里只写下面几个属性，与 xredis / xgorm 不把参数写进
// Span 是同一条原则。

// produceHook 挂在每个客户端上的生产钩子：
//   - 消息进缓冲时（Produce / ProduceSync 一调用）把链路上下文写进消息头，开一个 Producer Span；
//   - 消息有了结果时结束 Span，失败的记一条 WARN。
//
// Span 开在缓冲那一刻、结束在 broker 回了结果（或者放弃）那一刻：它量的是这条消息从发起到落定的整段，
// 攒批（Linger）、排队、重试都算在里面，和 ProduceSync 的调用方看到的耗时一致。
// 消息头里写的是这个 Span 的上下文，消费那一侧的 process Span 因此挂在它下面。
type produceHook struct {
	name  string // 集群名，直接调 New 建的为空
	trace bool
	log   bool
}

var (
	_ kgo.HookProduceRecordBuffered   = produceHook{}
	_ kgo.HookProduceRecordUnbuffered = produceHook{}
)

func (h produceHook) OnProduceRecordBuffered(r *kgo.Record) {
	ctx := r.Context // franz-go 在调这个钩子之前已经把 Produce 收到的 ctx 放进来了，不会是 nil
	if h.trace {
		ctx, _ = otel.Tracer(tracerName).Start(ctx, r.Topic+" publish",
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.operation.name", "publish"),
				attribute.String("messaging.destination.name", r.Topic),
			))
		r.Context = ctx // 结果出来时从这里取回 Span
	}
	// Trace 关着也注入：Span 不开，链路上下文（还有可信的透传 Header、baggage）照样往下游带，
	// 与 xhttp 关掉 Trace 时的做法一致
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier{r: r})
}

func (h produceHook) OnProduceRecordUnbuffered(r *kgo.Record, err error) {
	if h.trace {
		span := trace.SpanFromContext(r.Context)
		if err != nil {
			// 只标状态，不记错误原文，同 xcron：原文在日志里
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
		// 不记 value。没分到分区就失败的（topic 不存在）partition 是 0（kfake 实测），不说明什么
		attrs := append(nameAttr(h.name), "topic", r.Topic, "partition", r.Partition)
		attrs = append(attrs, keyAttrs(r.Key)...)
		slog.WarnContext(r.Context, "kafka produce failed", append(attrs, "error", err)...)
	}
}

// nameAttr 集群名，直接调 New 建的没有名字，就不写这个字段
func nameAttr(name string) []any {
	if name == "" {
		return nil
	}
	return []any{"name", name}
}

// headerCarrier 把消息头当成 OTel 的 TextMapCarrier。
//
// Set 覆盖同名的头而不是追加：死信消息带着原消息的头再写一次，追加的话会有两个 traceparent，
// 下游取到哪个说不准。
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

// TrustedPeer 消息头里的透传 Header、baggage 照收，见 xtrace.HeaderPropagator.Extract。
//
// xtrace 只收可信对端发来的这两样，HTTP 那一侧的可信对端是 TrustedProxies 里的直连地址。
// 消息没有「直连的对端」，能往 topic 里写的就是持有这个集群凭证的生产者——和数据库里的一行一样，
// 写它的是自己人；本模块生产时注入的也只是本进程已经认下的值（可信的上游给的、或者自己写的）。
// 所以当成可信。topic 对外开放写入的（第三方直接往里写），别在它的消费者上依赖透传 Header 和 baggage。
// traceparent 不受这一条管：它只是链路标识，xtrace 谁发来的都接。
func (headerCarrier) TrustedPeer() bool { return true }

// slogLogger 把 franz-go 自己的日志接到 slog 上，只收 WARN 及以上。
//
// franz-go v1.21.7 默认不记日志（nopLogger）：连不上、认证失败、再均衡出错都悄无声息，
// 调用方只在 ProduceSync 返回错误、或者干脆什么都收不到时才知道出了事。
// INFO / DEBUG 每次建连、每次再均衡、每次提交都有几行，量太大，不收。
// 每个客户端各接各的（kgo.WithLogger），不碰任何全局。
type slogLogger struct{ name string }

func (l slogLogger) Level() kgo.LogLevel { return kgo.LogLevelWarn }

func (l slogLogger) Log(level kgo.LogLevel, msg string, keyvals ...any) {
	lv := slog.LevelWarn
	if level == kgo.LogLevelError {
		lv = slog.LevelError
	}
	// franz-go 的 keyvals 是 key、value 交替，key 是英文的 snake_case（broker、err、group……），
	// 原样作为字段。franz-go 的消息放进 detail，键值放进 fields 一个组里，免得和日志平台上已有的字段（msg、error、name）撞名
	attrs := make([]any, 0, len(keyvals)/2)
	for i := 0; i+1 < len(keyvals); i += 2 {
		attrs = append(attrs, slog.Any(fmt.Sprint(keyvals[i]), keyvals[i+1]))
	}
	slog.Log(context.Background(), lv, "xkafka franz-go log", append(nameAttr(l.name),
		"detail", msg, slog.Group("fields", attrs...))...)
}

// partitionStr 指标标签用不上分区，这里只给日志 / 报错拼「topic/partition」
func partitionStr(topic string, partition int32) string {
	return topic + "/" + strconv.Itoa(int(partition))
}
