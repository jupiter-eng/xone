package xkafka

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xutil"
)

// newProducer builds a producing client through New and closes it when the test ends.
func newProducer(t *testing.T, cfg ClientConfig) *kgo.Client {
	t.Helper()
	cl, closer, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	return cl
}

func TestNew_ProducesAndInjectsTraceContext(t *testing.T) {
	rec := tracing(t)
	_, cfg := cluster(t, 1, "orders")
	cl := newProducer(t, cfg)

	ctx, parent := otel.Tracer("test").Start(context.Background(), "http request")
	err := cl.ProduceSync(ctx, &kgo.Record{Topic: "orders", Key: []byte("user-42"), Value: []byte("v")}).FirstErr()
	parent.End()
	if err != nil {
		t.Fatal(err)
	}

	got := readAll(t, cfg, "orders", 1)[0]
	tp := header(got, "traceparent")
	if !strings.Contains(tp, parent.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent header %q does not carry the producer's trace id", tp)
	}
	spans := spansNamed(rec, "orders publish")
	if len(spans) != 1 {
		t.Fatalf("want one producer span, got %d", len(spans))
	}
	s := spans[0]
	if s.SpanKind() != trace.SpanKindProducer || s.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("producer span: kind %v parent %v", s.SpanKind(), s.Parent().SpanID())
	}
	// The headers carry the publish Span's context: the consuming side hangs below it.
	if !strings.Contains(tp, s.SpanContext().SpanID().String()) {
		t.Errorf("traceparent %q should carry the publish span %s", tp, s.SpanContext().SpanID())
	}
	for _, a := range s.Attributes() {
		if strings.Contains(string(a.Key), "key") || a.Value.Emit() == "user-42" {
			t.Errorf("the record key leaked into span attribute %s=%s", a.Key, a.Value.Emit())
		}
	}
}

func TestNew_TraceOffStillInjectsContext(t *testing.T) {
	rec := tracing(t)
	_, cfg := cluster(t, 1, "orders")
	cfg.Trace = false
	cl := newProducer(t, cfg)

	ctx, parent := otel.Tracer("test").Start(context.Background(), "http request")
	if err := cl.ProduceSync(ctx, &kgo.Record{Topic: "orders", Value: []byte("v")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	parent.End()
	got := readAll(t, cfg, "orders", 1)[0]
	if !strings.Contains(header(got, "traceparent"), parent.SpanContext().SpanID().String()) {
		t.Fatalf("with Trace off the caller's context should still be injected, got %q", header(got, "traceparent"))
	}
	if n := len(spansNamed(rec, "orders publish")); n != 0 {
		t.Fatalf("Trace off but %d producer spans", n)
	}
}

func TestNew_ProduceFailureLogsWarnWithoutValue(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	cl := newProducer(t, cfg)
	err := cl.ProduceSync(context.Background(), &kgo.Record{Topic: "missing", Key: []byte("user-42"), Value: []byte("secret-value")}).FirstErr()
	if err == nil {
		t.Fatal("produce to a missing topic should fail")
	}
	lines := withMsg(logs(), "kafka produce failed")
	if len(lines) != 1 {
		t.Fatalf("want one WARN, got %v", logs())
	}
	l := lines[0]
	if l["level"] != "WARN" || l["topic"] != "missing" || l["key"] != "user-42" || l["error"] == nil || l["partition"] == nil {
		t.Errorf("produce failure log: %v", l)
	}
	for _, all := range logs() {
		for k, v := range all {
			if s, ok := v.(string); ok && strings.Contains(s, "secret-value") {
				t.Errorf("the record value leaked into log field %s", k)
			}
		}
	}
}

func TestNew_LogOffSilencesProduceFailures(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	cfg.Log = false
	cl := newProducer(t, cfg)
	_ = cl.ProduceSync(context.Background(), &kgo.Record{Topic: "missing", Value: []byte("v")}).FirstErr()
	if lines := withMsg(logs(), "kafka produce failed"); len(lines) != 0 {
		t.Fatalf("Log off but %v", lines)
	}
}

func TestNew_UnreachableBrokerFailsWithAddress(t *testing.T) {
	cfg := DefaultClientConfig()
	cfg.Brokers = []string{refusedAddr(t)}
	start := time.Now()
	_, _, err := New(context.Background(), cfg)
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xkafka" || xe.Op != "connect" {
		t.Fatalf("want xkafka connect error, got %v", err)
	}
	if !strings.Contains(err.Error(), cfg.Brokers[0]) {
		t.Errorf("the error should name the address: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a refused port took %v", d)
	}
}

// The peer accepts the connection and never answers: Ping itself waits 20s (measured on v1.21.7), and each attempt's budget is 2 x DialTimeout.
func TestNew_SilentBrokerBoundedByDialTimeout(t *testing.T) {
	cfg := DefaultClientConfig()
	cfg.Brokers = []string{silentAddr(t)}
	cfg.DialTimeout = 100 * time.Millisecond
	start := time.Now()
	_, _, err := New(context.Background(), cfg)
	// The underlying error here is context deadline exceeded, which carries no address: the address can only be the one xkafka wrote itself.
	if err == nil || !strings.Contains(err.Error(), "cannot reach "+cfg.Brokers[0]) {
		t.Fatalf("a silent broker should fail the probe naming the address: %v", err)
	}
	// 3 x 200ms plus two backoffs (10ms and 20ms in the test)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("probe against a silent broker took %v, want about 0.6s", d)
	}
}

func TestNew_ExitSignalAbortsProbe(t *testing.T) {
	cfg := DefaultClientConfig()
	cfg.Brokers = []string{silentAddr(t)}
	cfg.DialTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := New(ctx, cfg)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("canceled probe: %v after %v", err, time.Since(start))
	}
}

// kfake closes the connection outright (EOF) on an auth failure instead of replying SASL_AUTHENTICATION_FAILED, so this only verifies that the password does not leak.
// "an auth failure is not retried and is reported as authentication failed" is pinned down by TestAuthFailed and by e2e (a real Kafka).
func TestNew_WrongPasswordNotLeaked(t *testing.T) {
	logs := captureLogs(t)
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.EnableSASL(), kfake.Superuser("PLAIN", "app", "right-pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	cfg := DefaultClientConfig()
	cfg.Brokers = c.ListenAddrs()
	cfg.SASL = SASLConfig{Mechanism: "PLAIN", Username: "app", Password: "wrong-pw-s3cret"}

	_, _, err = New(context.Background(), cfg)
	if err == nil {
		t.Fatal("wrong password should fail")
	}
	if strings.Contains(err.Error(), "wrong-pw-s3cret") {
		t.Errorf("password in error: %v", err)
	}
	for _, l := range logs() {
		for k, v := range l {
			if s, ok := v.(string); ok && strings.Contains(s, "wrong-pw-s3cret") {
				t.Errorf("password in log field %s", k)
			}
		}
	}

	// The correct password connects.
	cfg.SASL.Password = "right-pw"
	newProducer(t, cfg)
}

func TestAuthFailed(t *testing.T) {
	for _, err := range []error{kerr.SaslAuthenticationFailed, kerr.UnsupportedSaslMechanism, fmt.Errorf("wrapped: %w", kerr.SaslAuthenticationFailed)} {
		if !authFailed(err) {
			t.Errorf("%v should count as an authentication failure", err)
		}
	}
	if authFailed(io.EOF) || authFailed(context.DeadlineExceeded) {
		t.Error("network errors are not authentication failures")
	}
	if p := probePolicy(DefaultClientConfig()); p.AuthFailed == nil || p.Attempts != 3 || p.Timeout != 2*time.Second {
		t.Errorf("probe policy: %+v", p)
	}
}

func TestNew_InvalidConfigIsConfigError(t *testing.T) {
	_, _, err := New(context.Background(), ClientConfig{})
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "config" || !strings.Contains(err.Error(), "Brokers") {
		t.Fatalf("want config error naming Brokers, got %v", err)
	}
}

func TestNew_AppliesProducerSettings(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	cfg.ClientID = "billing"
	cfg.Producer = ProducerConfig{Acks: "leader", Linger: 0, Compression: "zstd"}
	cl := newProducer(t, cfg)
	if cl.OptValue(kgo.ClientID) != "billing" || cl.OptValue(kgo.ProducerLinger) != time.Duration(0) ||
		cl.OptValue(kgo.DisableIdempotentWrite) != true || cl.OptValue(kgo.RequiredAcks) != kgo.LeaderAck() {
		t.Errorf("producer settings not applied: id=%v linger=%v idemp-off=%v acks=%v", cl.OptValue(kgo.ClientID),
			cl.OptValue(kgo.ProducerLinger), cl.OptValue(kgo.DisableIdempotentWrite), cl.OptValue(kgo.RequiredAcks))
	}
	if got := cl.OptValues(kgo.ProducerBatchCompression)[0].([]kgo.CompressionCodec); got[0] != kgo.ZstdCompression() {
		t.Errorf("compression = %v", got)
	}
	if cl.OptValue(kgo.DialTimeout) != time.Second {
		t.Errorf("DialTimeout = %v", cl.OptValue(kgo.DialTimeout))
	}
	if _, ok := cl.OptValue(kgo.WithLogger).(slogLogger); !ok {
		t.Errorf("franz-go's logger should be bridged to slog, got %T", cl.OptValue(kgo.WithLogger))
	}
}

// Built by the framework: the stop hook Flushes before it Closes, so a message sent with Produce (asynchronously) at the moment of exit is not lost.
func TestCloseXKafka_FlushesBeforeClose(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	cfg.Producer.Linger = time.Minute // without a Flush it would just keep accumulating
	if err := xclient.Build(context.Background(), reg, map[string]ClientConfig{DefaultName: cfg}, build); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	C().Produce(context.Background(), &kgo.Record{Topic: "orders", Value: []byte("last")}, func(_ *kgo.Record, err error) { done <- err })
	if err := closeXKafka(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("buffered record lost on shutdown: %v", err)
	}
	readAll(t, cfg, "orders", 1)
}

func TestBuild_ClientIDDefaultsToAppName(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	useConfig(t, "XApp:\n  Name: billing-svc\n")
	runHook(t, "xapp.loadConfig")
	inst, closer, err := build(context.Background(), "default", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if got := inst.client.OptValue(kgo.ClientID); got != "billing-svc" {
		t.Fatalf("ClientID = %v, want XApp.Name", got)
	}
}

func TestBuild_ClientIDFallsBackToTheExecutableWithoutXApp(t *testing.T) {
	// XApp is optional, so there may be no application name anywhere. The brokers still have to be able to tell
	// this service apart in their logs, quotas and monitoring, and franz-go's own default has every service on
	// the cluster calling itself kgo.
	want := xutil.ExecutableName()
	if want == "" {
		t.Skip("this operating system cannot name the executable, which leaves franz-go its own default")
	}
	_, cfg := cluster(t, 1, "orders")
	useConfig(t, "XLog:\n  Level: info\n")
	runHook(t, "xapp.loadConfig")

	inst, closer, err := build(context.Background(), "default", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if got := inst.client.OptValue(kgo.ClientID); got != want {
		t.Errorf("ClientID = %v, want the executable name %q", got, want)
	}
	// The consumers on this cluster are built from the ClientConfig kept here, so settling it once covers both sides.
	if inst.cfg.ClientID != want {
		t.Errorf("the kept config has ClientID = %q, so the consumers would send something else", inst.cfg.ClientID)
	}
}

func TestInitXKafka_CSaysUnconfiguredNotTooEarly(t *testing.T) {
	useConfig(t, "XApp:\n  Name: x\n")
	if err := initXKafka(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	defer func() {
		r := recover()
		if s, _ := r.(string); !strings.Contains(s, "none is configured") {
			t.Fatalf("panic = %v", r)
		}
	}()
	C()
}

func TestHooks_Stages(t *testing.T) {
	stages := map[string]hook.Stage{}
	for _, e := range hook.Start() {
		stages[e.Name] = e.Stage
	}
	if stages["xkafka.initXKafka"] != hook.StageClient {
		t.Errorf("clients should start at StageClient, got %v", stages["xkafka.initXKafka"])
	}
	if stages["xkafka.startConsumers"] != hook.StageServer {
		t.Errorf("consumers should start at StageServer, got %v", stages["xkafka.startConsumers"])
	}
}

func TestFranzLogger_BridgedToSlogAtWarn(t *testing.T) {
	logs := captureLogs(t)
	l := slogLogger{name: "default"}
	if l.Level() != kgo.LogLevelWarn {
		t.Fatalf("level = %v", l.Level())
	}
	l.Log(kgo.LogLevelError, "unable to commit", "group", "g1", "err", "boom")
	got := withMsg(logs(), "xkafka franz-go log")
	if len(got) != 1 || got[0]["level"] != "ERROR" || got[0]["detail"] != "unable to commit" || got[0]["name"] != "default" {
		t.Fatalf("bridged log: %v", got)
	}
	if f, _ := got[0]["fields"].(map[string]any); f["group"] != "g1" || f["err"] != "boom" {
		t.Errorf("fields: %v", got[0]["fields"])
	}
}

func TestKeyAttrs(t *testing.T) {
	long := strings.Repeat("\u00e9", 200) // 400 bytes
	for _, c := range []struct {
		name string
		key  []byte
		want []any
	}{
		{"nil", nil, nil},
		{"printable", []byte("user-42"), []any{"key", "user-42"}},
		{"binary", []byte{0, 1, 2, 0xff}, []any{"key_len", 4}},
		{"control", []byte("a\nb"), []any{"key_len", 3}},
		{"long", []byte(long), []any{"key", strings.Repeat("é", 128), "key_truncated", true}},
	} {
		got := keyAttrs(c.key)
		if len(got) != len(c.want) {
			t.Errorf("%s: %q", c.name, got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %q, want %q", c.name, got, c.want)
			}
		}
	}
}

// A dead letter is written again carrying the original message's headers: appending with Set would give two traceparents and leave the downstream guessing which one it gets.
func TestHeaderCarrier_SetReplacesAndTrustsPeer(t *testing.T) {
	r := &kgo.Record{Headers: []kgo.RecordHeader{{Key: "traceparent", Value: []byte("old")}, {Key: "tenant", Value: []byte("acme")}}}
	c := headerCarrier{r: r}
	c.Set("traceparent", "new")
	c.Set("baggage", "k=v")
	if len(r.Headers) != 3 || c.Get("traceparent") != "new" || c.Get("baggage") != "k=v" || c.Get("tenant") != "acme" {
		t.Fatalf("headers: %v", r.Headers)
	}
	if got := c.Keys(); len(got) != 3 || got[0] != "traceparent" {
		t.Fatalf("keys: %v", got)
	}
	if !c.TrustedPeer() {
		t.Fatal("Kafka headers are trusted, see headerCarrier.TrustedPeer")
	}
}
