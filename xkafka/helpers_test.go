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

// TestMain shortens the various backoffs and routes the slog default logger into a discard (a case that wants logs captures them itself).
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	retryInterval = 10 * time.Millisecond
	dlqInterval = 10 * time.Millisecond
	dlqMaxInterval = 50 * time.Millisecond
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

// wait is a generous upper bound for something asynchronous: joining a group and being assigned takes a few hundred milliseconds on kfake, and a CI machine is slower still.
const wait = 20 * time.Second

// cluster starts an in-memory Kafka (kfake) with partitions partitions per topic. It is closed when the test ends.
func cluster(t *testing.T, partitions int32, topics ...string) (*kfake.Cluster, ClientConfig) {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(partitions, topics...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	cfg := DefaultClientConfig()
	cfg.Brokers = c.ListenAddrs()
	cfg.Consumer.ResetOffset = "earliest" // so cases that produce before consuming need not wait to join the group; latest has cases of its own
	return c, cfg
}

// rawClient is the client the tests use to write and read themselves, without any of this module's hooks.
func rawClient(t *testing.T, cfg ClientConfig, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	cl, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(cfg.Brokers...)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return cl
}

// produce writes values into topic's partition, in order.
func produce(t *testing.T, cl *kgo.Client, topic string, partition int32, values ...string) {
	t.Helper()
	for _, v := range values {
		r := &kgo.Record{Topic: topic, Partition: partition, Key: []byte("k-" + v), Value: []byte(v)}
		if err := cl.ProduceSync(context.Background(), r).FirstErr(); err != nil {
			t.Fatalf("produce %s: %v", v, err)
		}
	}
}

// manualPartitioner makes produce write into the partition named by Record.Partition.
func manualPartitioner() kgo.Opt { return kgo.RecordPartitioner(kgo.ManualPartitioner()) }

// readAll reads every message in topic from the beginning, until n of them or a timeout.
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

// committed is the offset the consumer group committed on topic/partition, or -1 when it never committed.
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

// testManager is a consumer manager of its own, with cfg as the cluster (named default, plus any extra by name),
// stopped when the test ends. It does not touch the global std.
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

// register is Consume, but registering into m.
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

// start brings m up and Fatals on failure.
func start(t *testing.T, m *manager) {
	t.Helper()
	if err := m.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// waitAssigned waits for c to be assigned n partitions.
func waitAssigned(t *testing.T, c *consumer, n int) {
	t.Helper()
	eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.assigned) == n
	}, "consumer %s assigned %d partitions", c, n)
}

// eventually checks cond every 10ms and Fatals when it does not hold within wait.
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

// recorder collects the messages the handler saw, safely across goroutines.
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

// logLine is one JSON log line.
type logLine map[string]any

// captureLogs swaps the default logger for a real xlog (whose handler takes the scope fields and the trace_id out of the ctx)
// with the level at debug; the function returned reads out every log so far at any time.
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

// withMsg keeps only the logs whose msg is this one.
func withMsg(lines []logLine, msg string) []logLine {
	var out []logLine
	for _, l := range lines {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// tracing installs an in-memory TracerProvider and the W3C Propagator, restoring the previous ones when the test ends.
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

// spansNamed are the finished Spans named name.
func spansNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// header is the value of key in the message headers.
func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// useConfig uses yml as the config file and clears it when the test ends.
func useConfig(t *testing.T, yml string) {
	t.Helper()
	testkit.UseConfigEnv(t, yml)
}

// runHook runs one registered start hook by name.
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
