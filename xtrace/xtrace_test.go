package xtrace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xonetest"
)

// recorder is a SpanProcessor collecting Spans, used to assert that the Spans really reached the processor.
type recorder struct {
	mu    sync.Mutex
	spans []string
	shut  bool
}

func (r *recorder) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (r *recorder) OnEnd(s sdktrace.ReadOnlySpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, s.Name())
}
func (r *recorder) Shutdown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shut = true
	return nil
}
func (r *recorder) ForceFlush(context.Context) error { return nil }
func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.spans...)
}
func (r *recorder) isShut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shut
}

func TestNew_DefaultConfigYieldsRealProvider(t *testing.T) {
	rec := &recorder{}
	tr, closer, err := New(context.Background(), DefaultConfig(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("with tracing on it must be the SDK implementation, got=%T", tr.TracerProvider)
	}

	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "work")
	if !span.SpanContext().IsValid() {
		t.Error("everything is sampled by default, so the SpanContext must be valid")
	}
	span.End()

	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := rec.names(); len(got) != 1 || got[0] != "work" {
		t.Errorf("the Span must reach the registered processor, got=%v", got)
	}
	if !rec.isShut() {
		t.Error("closing the provider must close the processors attached to it as well")
	}
}

func TestNew_NoopWhenTracingDisabled(t *testing.T) {
	c := DefaultConfig()
	c.Enable = false
	rec := &recorder{}
	tr, closer, err := New(context.Background(), c, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); ok {
		t.Error("with tracing off no SDK provider may be built")
	}
	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "work")
	span.End()
	if len(rec.names()) != 0 {
		t.Errorf("with tracing off the processor must receive no Span, got=%v", rec.names())
	}
}

func TestNew_TracingDisabledKeepsHeaderForwarding(t *testing.T) {
	// Whether X-Request-Id should be carried downstream is a different question from whether to sample Spans.
	c := DefaultConfig()
	c.Enable = false
	c.ForwardHeaders = []string{"X-Request-Id"}
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	out := http.Header{}
	ctx := tr.Propagator.Extract(context.Background(), trusted(http.Header{"X-Request-Id": {"r1"}}))
	tr.Propagator.Inject(ctx, headerCarrier(out))
	if out.Get("X-Request-Id") != "r1" {
		t.Errorf("with tracing off forwarding must still work, got=%v", out)
	}
	if out.Get("traceparent") != "" {
		t.Errorf("with tracing off no traceparent may be injected, got=%v", out)
	}
}

func TestNew_InstalledPropagatorAcceptsHeadersOnlyFromTrustedPeers(t *testing.T) {
	// Complementary to HeaderPropagator's own cases: this goes through the composite Propagator New assembles --
	// the carrier passes TraceContext, Baggage and B3 before reaching it, and the trusted marker must survive the
	// journey while an untrusted one must not be let through by another propagator either.
	c := DefaultConfig()
	c.ForwardHeaders = []string{"X-Tenant-Id"}
	c.ForwardHeaderRules = []ForwardHeaderRule{{Domains: []string{"*.internal.com"}, Headers: []string{"X-Internal-Token"}}}
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	in := http.Header{"X-Tenant-Id": {"forged"}, "X-Internal-Token": {"forged"}}
	for _, from := range []struct {
		name    string
		carrier propagation.TextMapCarrier
		want    bool
	}{
		{"untrusted", headerCarrier(in), false},
		{"trusted", trusted(in), true},
	} {
		ctx := tr.Propagator.Extract(context.Background(), from.carrier)
		out := http.Header{}
		tr.Propagator.Inject(WithTargetHost(ctx, "api.internal.com"), headerCarrier(out))
		for _, h := range []string{"X-Tenant-Id", "X-Internal-Token"} {
			if got := out.Get(h) != ""; got != from.want {
				t.Errorf("a %s peer: %s forwarded=%v, want %v", from.name, h, got, from.want)
			}
		}
	}
}

func TestNew_InstalledPropagatorAcceptsBaggageOnlyFromTrustedPeers(t *testing.T) {
	// Baggage and forwarding headers are the same thing: key-value pairs the upstream gave, carried into every call.
	// Accepted from anybody, the X-Tenant-Id that ForwardHeaders keeps out would reach the network rewritten as
	// baggage: tenant=... traceparent is only a trace identifier and is accepted from anybody.
	tr, closer, err := New(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	in := http.Header{
		"Baggage":     {"tenant=forged"},
		"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	}
	for _, from := range []struct {
		name    string
		carrier propagation.TextMapCarrier
		want    string
	}{
		{"untrusted", headerCarrier(in), ""},
		{"trusted", trusted(in), "tenant=forged"},
	} {
		ctx := tr.Propagator.Extract(context.Background(), from.carrier)
		out := http.Header{}
		tr.Propagator.Inject(ctx, headerCarrier(out))
		if got := out.Get("Baggage"); got != from.want {
			t.Errorf("a %s peer: the downstream received baggage=%q, want %q", from.name, got, from.want)
		}
		if !strings.Contains(out.Get("Traceparent"), "4bf92f3577b34da6a3ce929d0e0e4736") {
			t.Errorf("a %s peer: traceparent is unaffected by the trust boundary, got=%q", from.name, out.Get("Traceparent"))
		}
	}
}

func TestTrustedBaggage_WarnsOnceForUntrustedPeerBaggage(t *testing.T) {
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	b := &trustedBaggage{}
	b.Extract(context.Background(), headerCarrier(http.Header{"X-Other": {"x"}}))
	if buf.Len() != 0 {
		t.Fatalf("without baggage there must be no warning, got=%s", buf.String())
	}
	for range 3 {
		b.Extract(context.Background(), headerCarrier(http.Header{"Baggage": {"k=v"}}))
	}
	if n := strings.Count(buf.String(), "xtrace ignored baggage from an untrusted peer"); n != 1 {
		t.Errorf("it must warn only once, got=%d times: %s", n, buf.String())
	}
	if want := "xtrace ignored baggage from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted"; !strings.Contains(buf.String(), want) {
		t.Errorf("the warning must read %q, got=%s", want, buf.String())
	}
}

func TestNew_InstallsW3CAndB3WhenEnabled(t *testing.T) {
	tr, closer, err := New(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	out := http.Header{}
	tr.Propagator.Inject(ctx, headerCarrier(out))

	for _, h := range []string{"Traceparent", "B3"} {
		if out.Get(h) == "" {
			t.Errorf("%s must be injected, got=%v", h, out)
		}
	}
}

func TestNew_ContradictoryForwardingConfigFails(t *testing.T) {
	c := DefaultConfig()
	c.ForwardHeaders = []string{"X-Internal-Token"}
	c.ForwardHeaderRules = []ForwardHeaderRule{{Domains: []string{"*.internal.com"}, Headers: []string{"X-Internal-Token"}}}

	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("a self-contradicting configuration must fail at startup rather than run on with a guessed meaning")
	}
}

func TestNew_ZeroStopBudgetFails(t *testing.T) {
	// 0 does not mean "no limit" but "do not wait at all": Shutdown receives an already expired context and the Spans
	// still in the buffer are dropped outright, while the config file merely looks like no limit was set.
	c := DefaultConfig()
	c.ShutdownTimeout = 0
	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("ShutdownTimeout=0 must be an error")
	}
}

func TestNew_ZeroSampleRateStillGeneratesAndPropagatesTraceID(t *testing.T) {
	// "not sampling" and "tracing off" are two different things: with 0, Spans are still created and TraceIDs are still
	// generated, they simply do not land -- the downstream gets a TraceID while no Span is stored locally.
	// To produce no Spans at all, use Enable: false.
	c := DefaultConfig()
	c.SampleRatio = 0
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("with a sampling ratio of 0 the SpanContext must still be valid, or the TraceID does not travel downstream")
	}
	if sc.IsSampled() {
		t.Error("with a sampling ratio of 0 nothing may be sampled")
	}
}

func TestSamplerOf(t *testing.T) {
	// Every ratio has to respect the upstream's decision, full sampling included.
	for _, r := range []float64{0, 0.1, 1} {
		if got := samplerOf(r).Description(); !strings.HasPrefix(got, "ParentBased{") {
			t.Errorf("SampleRatio=%v must be ParentBased, got=%s", r, got)
		}
	}
	// TraceIDRatioBased(1) is full sampling in itself and needs no branch of its own -- this pins that premise down.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(samplerOf(1)))
	defer tp.Shutdown(context.Background())
	for i := 0; i < 1000; i++ {
		_, span := tp.Tracer("t").Start(context.Background(), "s")
		if !span.SpanContext().IsSampled() {
			t.Fatal("with SampleRatio=1 a root Span must be fully sampled")
		}
		span.End()
	}
}

func TestNew_FullSamplingRespectsUpstreamNotSampled(t *testing.T) {
	// Regression case. SampleRatio>=1 used to be AlwaysSample, which ignores the parent Span:
	// an upstream sending sampled=00 was sampled here anyway and propagated downstream as -01 --
	// the upstream's decision overturned here, so the trace is either cut in two or the traffic the upstream
	// deliberately dropped is picked back up and pushed to the tracing backend.
	for _, ratio := range []float64{1, 0.5} {
		c := DefaultConfig()
		c.SampleRatio = ratio
		tr, closer, err := New(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}

		h := http.Header{"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"}}
		ctx := tr.Propagator.Extract(context.Background(), headerCarrier(h))
		ctx, span := tr.TracerProvider.Tracer("t").Start(ctx, "s")
		if span.SpanContext().IsSampled() {
			t.Errorf("SampleRatio=%v: the upstream said do not sample, so we must not", ratio)
		}
		out := http.Header{}
		tr.Propagator.Inject(ctx, headerCarrier(out))
		if tp := out.Get("Traceparent"); !strings.HasSuffix(tp, "-00") {
			t.Errorf("SampleRatio=%v: what goes downstream must say not sampled too, got=%s", ratio, tp)
		}
		span.End()
		closer.Close()
	}
}

func TestNew_OutOfRangeSampleRateFails(t *testing.T) {
	// >1 used to count as full sampling, a negative as no sampling, and NaN as nothing recognizable --
	// all of them misconfigurations that could start up silently.
	for _, r := range []float64{-0.1, 1.5, math.NaN(), math.Inf(1)} {
		c := DefaultConfig()
		c.SampleRatio = r
		_, _, err := New(context.Background(), c)
		if err == nil {
			t.Errorf("SampleRatio=%v must be an error", r)
			continue
		}
		// One module boundary, one xerror: validate returns a plain error and New wraps it once.
		if n := strings.Count(err.Error(), "xtrace"); n != 1 {
			t.Errorf("the module name must appear only once, got=%v", err)
		}
	}
}

func TestNew_StartsWhenSomeResourceAttrsFail(t *testing.T) {
	// Regression case. resource.New returns ErrPartialResource when some detection fails, together with whatever was
	// collected. That used to count as fatal: one wrong character in OTEL_RESOURCE_ATTRIBUTES, or a container where
	// user.Current cannot find the random UID, kept the service from starting --
	// shutting the whole process out for the sake of a few descriptive attributes.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod,this-is-not-key-value")
	rec := tracetest.NewSpanRecorder()
	tr, closer, err := New(context.Background(), DefaultConfig(), rec)
	if err != nil {
		t.Fatalf("failing to collect some attributes must not fail startup: %v", err)
	}
	defer closer.Close()
	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("a real provider must still be installed, got=%T", tr.TracerProvider)
	}

	// What is used is the part that was collected, not an empty resource: the correctly written entry,
	// the other detectors' attributes and our own service.name are all still there.
	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	span.End()
	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("one Span must be produced, got=%d", len(ended))
	}
	got := map[string]string{}
	for _, kv := range ended[0].Resource().Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["deployment.environment"] != "prod" {
		t.Errorf("the correctly written entry must still take effect, got=%v", got)
	}
	for _, k := range []string{"service.name", "telemetry.sdk.language"} {
		if _, ok := got[k]; !ok {
			t.Errorf("the other attributes must all still be there, missing %s, got=%v", k, got)
		}
	}
}

func TestCloseXTrace_HonorsFrameworkDeadline(t *testing.T) {
	// Regression case. Shutdown used to use Background + ShutdownTimeout and ignore the ctx the framework passed in:
	// with 100ms left of the framework's stop budget it still waited its full 5s,
	// squeezing out the components not yet closed, and K8s SIGKILLs the whole process once the grace period ends.
	keepGlobals(t)
	resetRegistration(t)
	AddSpanProcessor(blockingProcessor{})
	c := DefaultConfig()
	c.ShutdownTimeout = 5 * time.Second
	if err := install(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := closeXTrace(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("it must stop at the deadline the framework gave, waited %v", elapsed)
	}
	if err == nil {
		t.Error("a stuck export endpoint must be reported faithfully")
	}
}

func TestNew_SampleRateTakesEffect(t *testing.T) {
	c := DefaultConfig()
	c.SampleRatio = 0.5
	tr, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	// At half sampling, a thousand runs must produce both sampled and dropped ones.
	var sampled, dropped int
	for i := 0; i < 1000; i++ {
		_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
		if span.SpanContext().IsSampled() {
			sampled++
		} else {
			dropped++
		}
		span.End()
	}
	if sampled == 0 || dropped == 0 {
		t.Errorf("a sampling ratio of 0.5 must both sample and drop, sampled=%d dropped=%d", sampled, dropped)
	}
}

func TestClose_DoesNotHangOnTimeout(t *testing.T) {
	// With the export endpoint unreachable, Shutdown blocks forever, and without a deadline that is a hang on exit.
	c := DefaultConfig()
	c.ShutdownTimeout = 50 * time.Millisecond
	_, closer, err := New(context.Background(), c, blockingProcessor{})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- closer.Close() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stuck export endpoint must return a timeout error rather than pretend it closed cleanly")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the timeout")
	}
}

// blockingProcessor simulates a processor that cannot reach the export endpoint and whose Shutdown waits forever.
type blockingProcessor struct{}

func (blockingProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (blockingProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (blockingProcessor) ForceFlush(context.Context) error                { return nil }
func (blockingProcessor) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInstall_LogsGetTraceID(t *testing.T) {
	// xlog does not depend on OpenTelemetry, so the trace_id in the logs relies entirely on this package injecting that extractor.
	t.Cleanup(func() { xlog.SetTraceExtractor(nil) })

	tr, closer, err := New(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	tr.Install()

	ctx, span := otel.Tracer("t").Start(context.Background(), "s")
	defer span.End()

	dir := t.TempDir()
	lc := xlog.DefaultConfig()
	lc.Console = false
	lc.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}
	l, lcloser, err := xlog.New(lc)
	if err != nil {
		t.Fatal(err)
	}
	l.InfoContext(ctx, "a log with a trace")
	lcloser.Close()

	if got := readLog(t, dir); got[traceIDKey] != span.SpanContext().TraceID().String() {
		t.Errorf("the trace_id in the log must equal the current Span's, got=%v", got)
	}
}

func TestTransport_WritesTargetHostToCtx(t *testing.T) {
	// Without it, not one header from ForwardHeaderRules is ever injected.
	var seen string
	tr := &Transport{Next: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = TargetHostFromContext(r.Context())
		return &http.Response{StatusCode: 204, Body: io.NopCloser(nilReader{})}, nil
	})}

	req, _ := http.NewRequest("GET", "https://api.example.com:8443/x", nil)
	before := req.Context()
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if seen != "api.example.com:8443" {
		t.Errorf("the host with its port must be written, got=%q", seen)
	}
	if TargetHostFromContext(before) != "" {
		t.Error("by the RoundTripper contract the request passed in must not be modified")
	}
}

func TestTransport_NextNilUsesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := (&Transport{}).RoundTrip(req)
	if err != nil {
		t.Fatalf("with Next empty it must fall back to http.DefaultTransport, got=%v", err)
	}
	resp.Body.Close()
}

func TestAddSpanProcessor(t *testing.T) {
	t.Run("nil panics outright", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("passing nil must panic rather than leaving a nil pointer for runtime")
			}
		}()
		AddSpanProcessor(nil)
	})

	t.Run("registered before initialization gets attached", func(t *testing.T) {
		resetRegistration(t)
		rec := &recorder{}
		AddSpanProcessor(rec)

		initComponent(t)
		otel.Tracer("t").Start(context.Background(), "registered-first")
		_, span := otel.Tracer("t").Start(context.Background(), "registered-first")
		span.End()
		_ = closeXTrace(context.Background())

		if len(rec.names()) == 0 {
			t.Error("a processor registered before initialization must be attached at initialization")
		}
	})

	t.Run("registered after initialization takes effect immediately", func(t *testing.T) {
		resetRegistration(t)
		initComponent(t)

		rec := &recorder{}
		AddSpanProcessor(rec)
		_, span := otel.Tracer("t").Start(context.Background(), "registered-later")
		span.End()
		_ = closeXTrace(context.Background())

		if got := rec.names(); len(got) != 1 || got[0] != "registered-later" {
			t.Errorf("one registered after initialization must receive the Span immediately, got=%v", got)
		}
	})

	t.Run("registered after closing goes to pending and does not attach to a closed instance", func(t *testing.T) {
		resetRegistration(t)
		initComponent(t)
		_ = closeXTrace(context.Background())

		rec := &recorder{}
		AddSpanProcessor(rec)

		mu.Lock()
		n, l := len(pending), live
		mu.Unlock()
		if l != nil {
			t.Error("the provider must be cleared after closing")
		}
		if n != 1 {
			t.Errorf("one registered after closing must go to the pending queue, got=%d", n)
		}
	})
}

func TestAddSpanProcessor_NotLostWhenConcurrentWithInit(t *testing.T) {
	// Registration has two branches: before initialization it goes into pending to be taken, and after initialization
	// it attaches to live directly. If the lock were released between taking pending and installing live, a
	// registration landing in that window would get neither -- it would go into a pending that is never read again
	// and then be dropped silently. That is a completely silent failure: Spans are still produced and simply never reach the reporting endpoint.
	for i := 0; i < 50; i++ {
		resetRegistration(t)

		rec := &recorder{}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); initComponent(t) }()
		go func() { defer wg.Done(); AddSpanProcessor(rec) }()
		wg.Wait()

		// Whichever of the two comes first, the processor must be attached once registration completes:
		// either taken from pending by Init, or attached to live directly.
		mu.Lock()
		leftover := len(pending)
		mu.Unlock()
		if leftover > 0 {
			_ = closeXTrace(context.Background())
			t.Fatalf("round %d: the processor landed in the window unclaimed, %d left in pending", i, leftover)
		}

		_, span := otel.Tracer("t").Start(context.Background(), "s")
		span.End()
		got := len(rec.names())
		_ = closeXTrace(context.Background())
		if got == 0 {
			t.Fatalf("round %d: the processor was swallowed and received no Span at all", i)
		}
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// This is the only wire between this package and the framework: a missing hook registration or a wrong stage shows
	// up as "the config has no effect" or "it is ready later than what uses it", and nothing else catches it.
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xtrace" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("no start hook was registered")
	}
	if got.Stage != hook.StageTelemetry {
		t.Errorf("tracing must be ready before the clients, or their Spans have nothing to attach to, got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xtrace" {
			stopped = true
		}
	}
	if stopped != true {
		t.Errorf("the stop hook registration is wrong, got=%v", stopped)
	}
}

func TestInit_UsesAppNameAsServiceName(t *testing.T) {
	// The service name lives in the shared App block, which tracing and metrics both read, so they cannot be
	// configured separately and disagree. This walks the whole path: config file -> config.Load -> component Init -> OTel resource.
	resetRegistration(t)
	loadConfig(t, "XApp:\n  Name: xone.demo.app\n  Version: v1.2.0\nXTrace:\n  SampleRatio: 1\n")

	if xapp.Name() != "xone.demo.app" {
		t.Fatalf("the configuration did not reach xapp, got=%q", xapp.Name())
	}

	exp := tracetest.NewInMemoryExporter()
	AddSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))

	initComponent(t)
	_, span := otel.Tracer("t").Start(context.Background(), "s")
	span.End()
	// Taken before closing: InMemoryExporter's Shutdown clears the Spans already collected.
	spans := exp.GetSpans()
	_ = closeXTrace(context.Background())

	if len(spans) == 0 {
		t.Fatal("no Span was received")
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["service.name"] != "xone.demo.app" {
		t.Errorf("service.name must come from App.Name, got=%q", attrs["service.name"])
	}
	if attrs["service.version"] != "v1.2.0" {
		t.Errorf("service.version must come from App.Version, got=%q", attrs["service.version"])
	}
}

// ---- helpers ----

// resetRegistration clears the package-level registration state so every subtest starts from the same point.
func resetRegistration(t *testing.T) {
	t.Helper()
	reset := func() {
		mu.Lock()
		pending, live = nil, nil
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// initComponent walks the path the framework really takes: assemble the tracing setup from the default configuration.
//
// It calls install directly rather than initXTrace: the latter would read from the already loaded configuration,
// while these cases are about the assembly itself, not about where the configuration comes from.
func initComponent(t *testing.T) {
	t.Helper()
	if err := install(context.Background(), DefaultConfig()); err != nil {
		t.Errorf("initialization failed: %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, errors.New("EOF") }

var _ oteltrace.TracerProvider = (*sdktrace.TracerProvider)(nil)

// headerCarrier saves the tests one import.
func headerCarrier(h http.Header) propagation.HeaderCarrier { return propagation.HeaderCarrier(h) }

// traceIDKey is the name of the trace field in the logs, decided by xlog's handler.
const traceIDKey = "trace_id"

// readLog reads the one log line in the directory and parses it into a map.
func readLog(t *testing.T, dir string) map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no log file found: %v", err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	out := map[string]any{}
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("the log line is not JSON: %v, content=%q", err, line)
	}
	return out
}

// loadConfig feeds a piece of YAML in through the real loading path.
func loadConfig(t *testing.T, yml string) {
	t.Helper()
	xonetest.UseConfigYAML(t, yml)

	// Each package reads its configuration in its own start hook, so the hooks have to run too --
	// and this case is exactly about the service name xapp reads reaching the traces.
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xapp" {
			if err := e.Run(context.Background()); err != nil {
				t.Fatalf("%s failed: %v", e.Name, err)
			}
		}
	}
}

func TestClose_ClosingStandaloneInstanceLeavesGlobalAlone(t *testing.T) {
	// The instance New built is not necessarily the one installed globally -- that happens when a test wants a clean
	// tracing setup, or when two configurations exist at once. Closing one of them used to wipe the global one as well,
	// after which every AddSpanProcessor attached to a pending that nobody reads again:
	// Spans would still be produced and never reach the reporting endpoint, with no sign of it.
	reset := func() { mu.Lock(); live, pending = nil, nil; mu.Unlock() }
	reset()
	t.Cleanup(reset)

	c := DefaultConfig()
	c.Enable = true
	if err := install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeXTrace(context.Background()) })

	mu.Lock()
	installed := live
	mu.Unlock()
	if installed == nil {
		t.Fatal("live must have a value after start")
	}

	// Build another one that was never Installed, and close only that.
	_, standalone, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	after := live
	mu.Unlock()
	if after != installed {
		t.Errorf("closing an instance that was never installed cleared the globally installed one: before %p after %p", installed, after)
	}

	// With the global one still alive, a later SpanProcessor must attach to it rather than to pending.
	AddSpanProcessor(noopProcessor{})
	mu.Lock()
	n := len(pending)
	mu.Unlock()
	if n != 0 {
		t.Errorf("the SpanProcessor landed in a pending nobody reads, pending=%d", n)
	}
}

// noopProcessor is a SpanProcessor that does nothing.
type noopProcessor struct{}

func (noopProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (noopProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (noopProcessor) Shutdown(context.Context) error                  { return nil }
func (noopProcessor) ForceFlush(context.Context) error                { return nil }

// keepGlobals records the globals install will change and restores them when the test ends.
func keepGlobals(t *testing.T) {
	t.Helper()
	oldTP := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	mu.Lock()
	oldPending, oldLive := pending, live
	mu.Unlock()
	t.Cleanup(func() {
		_ = closeXTrace(context.Background())
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
		mu.Lock()
		pending, live = oldPending, oldLive
		mu.Unlock()
	})
}

func TestInitXTrace_InstallsDefaultTracingWhenUnconfigured(t *testing.T) {
	// Not installing it when tracing is not configured would leave the processors AddSpanProcessor registered hanging
	// in pending forever, so Spans would still be produced and never reach the reporting endpoint.
	keepGlobals(t)
	loadConfig(t, "XApp:\n  Name: demo\n")

	if err := initXTrace(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	mu.Lock()
	got := live
	mu.Unlock()
	if got == nil {
		t.Fatal("no global provider was installed")
	}
}

func TestInitXTrace_ConfigTypoFailsStartup(t *testing.T) {
	keepGlobals(t)
	loadConfig(t, "XTrace:\n  Sample: 0.5\n")

	if err := initXTrace(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup")
	}
}

func TestInitXTrace_InvalidValueFailsStartup(t *testing.T) {
	// 0 does not mean "no limit" but "do not wait at all": Shutdown receives an already expired context
	// and the Spans still in the buffer are dropped outright.
	keepGlobals(t)
	loadConfig(t, "XTrace:\n  ShutdownTimeout: 0\n")

	if err := initXTrace(context.Background()); err == nil {
		t.Fatal("a ShutdownTimeout of 0 must fail startup")
	}
}

func TestInitXTrace_BadForwardingRulesFailAtConfigRead(t *testing.T) {
	// Validate checks the forwarding rules too: xconfig.Unmarshal catches it while reading this block, so the error
	// comes from xconfig and names the block, without waiting for the Propagator to be assembled.
	for name, conf := range map[string]string{
		"a malformed wildcard":          "XTrace:\n  ForwardHeaderRules:\n    - Domains: [\"*trusted.com\"]\n      Headers: [X-Internal-Token]\n",
		"one header written both ways":  "XTrace:\n  ForwardHeaders: [X-Token]\n  ForwardHeaderRules:\n    - Domains: [api.internal.com]\n      Headers: [x-token]\n",
		"a sampling ratio out of range": "XTrace:\n  SampleRatio: 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			keepGlobals(t)
			loadConfig(t, conf)
			err := initXTrace(context.Background())
			if err == nil {
				t.Fatal("a misconfigured XTrace block must fail while the config is read")
			}
			if !xerror.Is(err, "xconfig") || !strings.Contains(err.Error(), ConfigKey) {
				t.Errorf("the error must come from xconfig and name %s, got=%v", ConfigKey, err)
			}
		})
	}
}

func TestInitXTrace_ConfigAppliedToGlobalProvider(t *testing.T) {
	keepGlobals(t)
	loadConfig(t, "XApp:\n  Name: xone.demo.app\n  Version: v9.9.9\nXTrace:\n  Enable: true\n")

	if err := initXTrace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Errorf("the global provider was not replaced by the one this module built, got=%T", otel.GetTracerProvider())
	}
	if err := closeXTrace(context.Background()); err != nil {
		t.Errorf("closing must not be an error: %v", err)
	}
}

func TestCloseXTrace_ClosesWithoutInit(t *testing.T) {
	keepGlobals(t)
	mu.Lock()
	live = nil
	mu.Unlock()
	if err := closeXTrace(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

// idleCloser is a RoundTripper that keeps a count.
type idleCloser struct {
	roundTripperFunc
	closed int
}

func (c *idleCloser) CloseIdleConnections() { c.closed++ }

func TestTransport_CloseIdleConnections_ForwardsToUnderlying(t *testing.T) {
	// http.Client.CloseIdleConnections() finds this method by type assertion:
	// wrapping without forwarding still satisfies the assertion (this type has the method) while the call becomes a
	// no-op -- and since tracing is on by default, by default idle connections were simply never cleaned up on exit.
	inner := &idleCloser{roundTripperFunc: func(*http.Request) (*http.Response, error) { return nil, nil }}
	tr := &Transport{Next: inner}

	tr.CloseIdleConnections()
	if inner.closed != 1 {
		t.Errorf("the underlying CloseIdleConnections was not called, got=%d", inner.closed)
	}

	// Confirm it once more through the real http.Client path: that is what asserts on this type.
	(&http.Client{Transport: tr}).CloseIdleConnections()
	if inner.closed != 2 {
		t.Errorf("the http.Client path did not forward it, got=%d", inner.closed)
	}
}

func TestTransport_CloseIdleConnections_NoPanicWhenUnderlyingLacksIt(t *testing.T) {
	tr := &Transport{Next: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}
	tr.CloseIdleConnections() // not panicking is a pass
}

func TestTransport_CloseIdleConnections_ForwardsToDefaultWithoutNext(t *testing.T) {
	(&Transport{}).CloseIdleConnections() // not panicking is a pass
}

// resourceAttrs builds a tracing setup from the default configuration and takes the resource attributes carried on a Span.
func resourceAttrs(t *testing.T) map[string]string {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tr, closer, err := New(context.Background(), DefaultConfig(), rec)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	span.End()
	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("one Span must be produced, got=%d", len(ended))
	}
	got := map[string]string{}
	for _, kv := range ended[0].Resource().Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	return got
}

// useApp sets the App block through the real configuration path and restores "not configured" when the test ends.
func useApp(t *testing.T, name, version string) {
	t.Helper()
	set := func(n, v string) {
		loadConfig(t, fmt.Sprintf("XApp:\n  Name: %q\n  Version: %q\n", n, v))
	}
	set(name, version)
	t.Cleanup(func() { set("", "") })
}

func TestNew_Resource_NoEmptyServiceNameWithoutAppName(t *testing.T) {
	// Regression case. With App.Name unconfigured it used to write service.name="" anyway,
	// covering OTel's own unknown_service fallback and adding a nameless service to the dashboards.
	useApp(t, "", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	got := resourceAttrs(t)
	if !strings.HasPrefix(got["service.name"], "unknown_service:") {
		t.Errorf("without an application name it must fall back to OTel's own name, got=%q", got["service.name"])
	}
	if v, ok := got["service.version"]; ok {
		t.Errorf("without a configured version no service.version may be written, got=%q", v)
	}
}

func TestNew_Resource_UsesOTEL_SERVICE_NAMEWithoutAppName(t *testing.T) {
	useApp(t, "", "")
	t.Setenv("OTEL_SERVICE_NAME", "from-env")

	if got := resourceAttrs(t); got["service.name"] != "from-env" {
		t.Errorf("service.name must come from OTEL_SERVICE_NAME, got=%q", got["service.name"])
	}
}

func TestNew_Resource_OTelEnvOverridesAppConfig(t *testing.T) {
	// An environment variable is the deployer's last word and should override a config file baked into the image.
	useApp(t, "from-config", "v1.0.0")
	t.Setenv("OTEL_SERVICE_NAME", "from-env")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=v2.0.0")

	got := resourceAttrs(t)
	if got["service.name"] != "from-env" {
		t.Errorf("OTEL_SERVICE_NAME must override App.Name, got=%q", got["service.name"])
	}
	if got["service.version"] != "v2.0.0" {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES must override App.Version, got=%q", got["service.version"])
	}
}

func TestNew_TracingDisabledStillShutsDownProcessors(t *testing.T) {
	// Regression case. With tracing off, a no-op Closer used to be handed back and the processors passed in were never
	// Shut down: the exporters' connections and goroutines were left uncleaned on exit.
	c := DefaultConfig()
	c.Enable = false
	rec := &recorder{}
	_, closer, err := New(context.Background(), c, rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if !rec.isShut() {
		t.Error("the processors passed in must be Shut down on close")
	}
}

func TestCloseXTrace_TracingDisabledStillClosesRegisteredProcessors(t *testing.T) {
	// AddSpanProcessor promised this package closes them, with tracing on or off alike.
	keepGlobals(t)
	resetRegistration(t)
	before, after := &recorder{}, &recorder{}
	AddSpanProcessor(before)
	c := DefaultConfig()
	c.Enable = false
	if err := install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	AddSpanProcessor(after)

	if err := closeXTrace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !before.isShut() {
		t.Error("a processor registered before initialization must be Shut down on close")
	}
	if !after.isShut() {
		t.Error("a processor registered after initialization must be Shut down on close")
	}
}
