package xkafka

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xlog"
)

// TestMain 调短各种退避，并把 slog 默认 logger 接到丢弃里（要看日志的用例自己 capture）
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	retryInterval = 10 * time.Millisecond
	dlqInterval = 10 * time.Millisecond
	dlqMaxInterval = 50 * time.Millisecond
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

// wait 给异步的事一个宽裕的上限：kfake 上一次入组、分配要几百毫秒，CI 的机器还更慢
const wait = 20 * time.Second

// cluster 起一个内存里的 Kafka（kfake），每个 topic 建好 partitions 个分区。测试结束时关掉
func cluster(t *testing.T, partitions int32, topics ...string) (*kfake.Cluster, ClientConfig) {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(partitions, topics...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	cfg := DefaultClientConfig()
	cfg.Brokers = c.ListenAddrs()
	cfg.Consumer.ResetOffset = "earliest" // 先写后消费的用例不用等入组；latest 另有用例
	return c, cfg
}

// rawClient 测试自己用来写、读的客户端，不带本模块的任何钩子
func rawClient(t *testing.T, cfg ClientConfig, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	cl, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(cfg.Brokers...)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return cl
}

// produce 往 topic 的 partition 里按顺序写 values
func produce(t *testing.T, cl *kgo.Client, topic string, partition int32, values ...string) {
	t.Helper()
	for _, v := range values {
		r := &kgo.Record{Topic: topic, Partition: partition, Key: []byte("k-" + v), Value: []byte(v)}
		if err := cl.ProduceSync(context.Background(), r).FirstErr(); err != nil {
			t.Fatalf("produce %s: %v", v, err)
		}
	}
}

// manualPartitioner 让 produce 写进 Record.Partition 指定的分区
func manualPartitioner() kgo.Opt { return kgo.RecordPartitioner(kgo.ManualPartitioner()) }

// readAll 从头读 topic 里的全部消息，直到 n 条或者超时
func readAll(t *testing.T, cfg ClientConfig, topic string, n int) []*kgo.Record {
	t.Helper()
	cl := rawClient(t, cfg, kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n {
		fs := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("read %s: got %d of %d records before timeout", topic, len(out), n)
		}
		out = append(out, fs.Records()...)
	}
	return out
}

// committed 消费组在 topic/partition 上提交的 offset，没提交过是 -1
func committed(t *testing.T, cfg ClientConfig, group, topic string, partition int32) int64 {
	t.Helper()
	cl := rawClient(t, cfg)
	req := kmsg.NewPtrOffsetFetchRequest()
	req.Group = group
	rt := kmsg.NewOffsetFetchRequestTopic()
	rt.Topic, rt.Partitions = topic, []int32{partition}
	req.Topics = append(req.Topics, rt)
	resp, err := req.RequestWith(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range resp.Topics {
		for _, p := range tp.Partitions {
			if tp.Topic == topic && p.Partition == partition {
				return p.Offset
			}
		}
	}
	for _, g := range resp.Groups {
		for _, tp := range g.Topics {
			for _, p := range tp.Partitions {
				if tp.Topic == topic && p.Partition == partition {
					return p.Offset
				}
			}
		}
	}
	return -1
}

// testManager 一个独立的消费者管理器，集群用 cfg（名字 default，多给的 extra 按名字），
// 测试结束时停掉。不碰全局的 std
func testManager(t *testing.T, cfg ClientConfig, extra ...map[string]ClientConfig) *manager {
	t.Helper()
	clusters := map[string]ClientConfig{DefaultName: cfg}
	for _, e := range extra {
		for k, v := range e {
			clusters[k] = v
		}
	}
	m := &manager{lookup: func(name string) (instance, bool) {
		c, ok := clusters[name]
		return instance{name: name, cfg: c}, ok
	}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		_ = m.stop(ctx)
	})
	return m
}

// register 同 Consume，但登记进 m
func register(t *testing.T, m *manager, topic, group string, fn func(context.Context, *kgo.Record) error, opts ...Option) *consumer {
	t.Helper()
	c := &consumer{topic: topic, group: group, fn: fn, o: buildOptions(topic, opts)}
	if err := c.o.validate(topic); err != nil {
		t.Fatal(err)
	}
	if err := m.add(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// start 起 m，失败就 Fatal
func start(t *testing.T, m *manager) {
	t.Helper()
	if err := m.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// waitAssigned 等 c 分到 n 个分区
func waitAssigned(t *testing.T, c *consumer, n int) {
	t.Helper()
	eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.assigned) == n
	}, "consumer %s assigned %d partitions", c, n)
}

// eventually 每 10ms 看一次 cond，wait 之内不成立就 Fatal
func eventually(t *testing.T, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: "+format, args...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recorder 收集处理函数看到的消息，线程安全
type recorder struct {
	mu   sync.Mutex
	seen []string // partition:value
}

func (r *recorder) add(rec *kgo.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, string(rec.Value))
}

func (r *recorder) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

// logLine 一行 JSON 日志
type logLine map[string]any

// captureLogs 把默认 logger 换成真的 xlog（作用域字段、trace_id 是它的 handler 从 ctx 里取的），
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

// withMsg 只留 msg 是这个的日志
func withMsg(lines []logLine, msg string) []logLine {
	var out []logLine
	for _, l := range lines {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// tracing 装一个记在内存里的 TracerProvider 和 W3C 的 Propagator，测试结束时换回去
func tracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTracerProvider(oldTP); otel.SetTextMapPropagator(oldProp) })
	return rec
}

// spansNamed 已结束的、名字是 name 的 Span
func spansNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// header 消息头里 key 的值
func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// useConfig 用 yml 当配置文件，测试结束时清掉
func useConfig(t *testing.T, yml string) {
	t.Helper()
	testkit.UseConfigEnv(t, yml)
}

// runHook 按名字跑一个已登记的启动钩子
func runHook(t *testing.T, name string) {
	t.Helper()
	for _, e := range hook.Start() {
		if e.Name == name {
			if err := e.Run(context.Background()); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("hook %s is not registered", name)
}
