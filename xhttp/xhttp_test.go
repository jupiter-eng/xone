package xhttp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
	"github.com/jupiter-eng/xone/xtrace"
)

// TestMain turns resty's internal logging off: the cases that cannot connect would fill a screen with retry warnings anyway.
func TestMain(m *testing.M) {
	silent = true
	os.Exit(m.Run())
}

// silent keeps a client built inside a test quiet.
var silent bool

// echo starts a server that records the requests it receives.
func echo(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Pointer[http.Header]) {
	t.Helper()
	var last atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Clone()
		last.Store(&hdr)
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func TestNew_ReturnsUsableResty(t *testing.T) {
	srv, _ := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	resp, err := client.R().SetContext(context.Background()).Get(srv.URL)
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	if resp.StatusCode() != 204 {
		t.Errorf("wrong status code, got=%d", resp.StatusCode())
	}
}

func TestNew_PoolSettingsPassedThrough(t *testing.T) {
	c := DefaultConfig()
	c.MaxIdleConnsPerHost, c.MaxIdleConns, c.IdleConnTimeout, c.MaxConnsPerHost = 33, 77, 11*time.Second, 5
	c.Trace = false // otelhttp.Transport does not export its inner layer, so do not wrap it when looking at the bottom
	client, _ := newQuiet(t, c)

	// The standard library keeps only 2 idle connections per host by default, too few for a service calling a handful of downstreams.
	tr := unwrapTransport(t, client)
	if tr.MaxIdleConnsPerHost != 33 || tr.MaxIdleConns != 77 || tr.IdleConnTimeout != 11*time.Second || tr.MaxConnsPerHost != 5 {
		t.Errorf("the pool parameters did not reach it, got=%+v", tr)
	}
	if tr.DialContext == nil {
		t.Error("a Dialer with a timeout must be installed")
	}
	// Cloned from DefaultTransport rather than built fresh, so the proxy and TLS defaults are not lost.
	if tr.Proxy == nil {
		t.Error("DefaultTransport's proxy setting must be kept")
	}
}

// unwrapTransport peels off the tracing layers to reach the underlying *http.Transport.
func unwrapTransport(t *testing.T, client *resty.Client) *http.Transport {
	t.Helper()
	rt := client.GetClient().Transport
	for i := 0; i < 5; i++ {
		switch v := rt.(type) {
		case *http.Transport:
			return v
		case *xtrace.Transport:
			rt = v.Next
		case propagateOnly:
			rt = v.next
		default:
			t.Fatalf("Transport type that cannot be peeled: %T (otelhttp does not export its inner layer, so turn Trace off when testing this layer)", rt)
		}
	}
	t.Fatal("too many wrapping layers")
	return nil
}

func TestNew_TraceOffOnlySkipsSpans_TraceIDAndForwardHeadersStillSent(t *testing.T) {
	// Regression case. XHttp.Trace: false used to remove xtrace.Transport and the injection with it,
	// so X-Request-Id and the upstream traceparent died at this hop while the documentation says it governs the Span only.
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })

	exp := tracetest.NewInMemoryExporter()
	tc := xtrace.DefaultConfig()
	tc.ForwardHeaders = []string{"X-Request-Id"}
	tc.ForwardHeaderRules = []xtrace.ForwardHeaderRule{{Domains: []string{"127.0.0.1"}, Headers: []string{"X-Tenant-Id"}}}
	tr, tcloser, err := xtrace.New(context.Background(), tc, sdktrace.NewSimpleSpanProcessor(exp))
	if err != nil {
		t.Fatal(err)
	}
	defer tcloser.Close()
	tr.Install()

	srv, last := echo(t, nil)
	c := DefaultConfig()
	c.Trace = false
	client, _ := newQuiet(t, c)

	const upstream = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := tr.Propagator.Extract(context.Background(), fromTrusted{propagation.HeaderCarrier(http.Header{
		"Traceparent":  {"00-" + upstream + "-00f067aa0ba902b7-01"},
		"Baggage":      {"tenant=acme"},
		"X-Request-Id": {"req-1"},
		"X-Tenant-Id":  {"t-1"},
	})})
	req := client.R().SetContext(ctx)
	if _, err := req.Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	got := *last.Load()
	if got.Get("X-Request-Id") != "req-1" {
		t.Errorf("X-Request-Id must be forwarded with Trace off too, downstream received=%v", got)
	}
	if got.Get("X-Tenant-Id") != "t-1" {
		t.Errorf("the per-domain forwarding rules must work too (the target host is written into the ctx by xtrace.Transport), downstream received=%v", got)
	}
	if tp := got.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+upstream+"-") {
		t.Errorf("the upstream trace identifiers must reach the downstream with Trace off, got=%q", tp)
	}
	if got.Get("Baggage") != "tenant=acme" {
		t.Errorf("baggage must reach the downstream too, got=%q", got.Get("Baggage"))
	}
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Errorf("with Trace off no outbound Span may be opened, got=%d", len(spans))
	}
	if req.RawRequest.Header.Get("X-Request-Id") != "" {
		t.Error("the injection must be written on a clone and must not modify the caller's request")
	}
}

func TestNew_TraceOffCloseIdleConnectionsStillReachesPool(t *testing.T) {
	// A RawClient user calls http.Client.CloseIdleConnections, which searches downwards by type assertion,
	// and any layer in between that does not forward makes it a no-op. Asserting that the method exists proves nothing; the connections have to be really released.
	var idle atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateIdle:
			idle.Add(1)
		case http.StateClosed, http.StateHijacked:
			idle.Add(-1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := DefaultConfig()
	c.Trace = false
	client, _ := newQuiet(t, c)
	if _, err := client.R().Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return idle.Load() > 0 }, "there must be one idle connection once the request finishes")
	client.GetClient().CloseIdleConnections()
	waitFor(t, func() bool { return idle.Load() == 0 }, "the idle connection must be released after CloseIdleConnections")
}

func TestTransport_OtelhttpInjectsForwardHeadersUnderNoop(t *testing.T) {
	// The single-Transport simplification rests on this premise: otelhttp calls the global Propagator to inject
	// whether or not the TracerProvider is a noop. Without it, the "inject ourselves when tracing is off" layer would have to come back.
	// It is pinned here rather than in xtrace: pulling otelhttp in there would add four modules to every xtrace user's
	// module graph for nothing.
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })
	otel.SetTracerProvider(noop.NewTracerProvider())

	tc := xtrace.DefaultConfig()
	tc.Enable = false // tracing off
	tc.ForwardHeaders = []string{"X-Request-Id"}
	tr, tcloser, err := xtrace.New(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	defer tcloser.Close()
	tr.Install()

	srv, last := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	// Forwarding accepts values only from a trusted peer; this stands in for the headers xgin hands over after judging it trusted.
	ctx := tr.Propagator.Extract(context.Background(),
		fromTrusted{propagation.HeaderCarrier(http.Header{"X-Request-Id": {"req-1"}})})
	if _, err := client.R().SetContext(ctx).Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	got := *last.Load()
	if got.Get("X-Request-Id") != "req-1" {
		t.Fatalf("headers must be forwarded with tracing off too, received=%v", got)
	}
	if got.Get("traceparent") != "" {
		t.Errorf("with tracing off no traceparent may be injected, got=%q", got.Get("traceparent"))
	}
}

// fromTrusted is a carrier declaring "the peer is trusted"; the contract is in xtrace.HeaderPropagator.Extract.
type fromTrusted struct{ propagation.HeaderCarrier }

func (fromTrusted) TrustedPeer() bool { return true }

func TestTransport_TraceOnInjectsTraceparentAndStartsSpan(t *testing.T) {
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	srv, last := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	if _, err := client.R().SetContext(context.Background()).Get(srv.URL + "/api/orders"); err != nil {
		t.Fatal(err)
	}

	if got := (*last.Load()).Get("traceparent"); got == "" {
		t.Error("traceparent must be injected")
	}
	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("a Span must be produced")
	}
	if name := spans[0].Name; name != "GET" {
		t.Errorf("the Span name must be the method alone, got=%q", name)
	}
}

func TestTransport_TargetHostStoredInCtx(t *testing.T) {
	// Without it, not one per-domain forwarding rule ever matches.
	var seen string
	tr := &xtrace.Transport{Next: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = xtrace.TargetHostFromContext(r.Context())
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})}
	req, _ := http.NewRequest("GET", "https://api.example.com/x", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if seen != "api.example.com" {
		t.Errorf("the target host must be written, got=%q", seen)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ---- retries ----

func TestRetry_OnlyIdempotentMethods(t *testing.T) {
	// A timeout cannot tell "the request never arrived" from "it was processed and the response was lost",
	// so resending a POST may mean a duplicate order.
	if !onlyIdempotent.condition(respFor("GET"), timeoutErr) {
		t.Error("a GET must be allowed to retry")
	}
	for _, m := range []string{"POST", "PATCH"} {
		if onlyIdempotent.condition(respFor(m), timeoutErr) {
			t.Errorf("%s must not be retried", m)
		}
	}
	if onlyIdempotent.condition(respFor("GET"), nil) {
		t.Error("with a response in hand there must be no retry, matching resty's default condition")
	}
	if onlyIdempotent.condition(nil, timeoutErr) {
		t.Error("an unrecognized method must conservatively not be retried")
	}
}

// onlyIdempotent is the policy installed with RetryOnlyIdempotent on.
var onlyIdempotent = retryPolicy{onlyIdempotent: true}

// timeoutErr is a transport-layer error shaped like the one http.Client.Do reports on a timeout.
var timeoutErr error = &url.Error{Op: "Get", URL: "http://h", Err: context.DeadlineExceeded}

func TestRetry_NoRetryOnResponseParseFailure(t *testing.T) {
	// Attaching a retry condition voids resty's own judgment. This used to retry an idempotent method on any error,
	// and measured, a 200 with broken JSON and RetryCount=3 sent the same request 4 times -- the response was already
	// fully received, and resending it would only fetch the same broken JSON again. With no condition attached resty sends it once.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{bad`))
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 3, time.Millisecond, 5*time.Millisecond
	client, _ := newQuiet(t, c)

	var out map[string]any
	if _, err := client.R().SetContext(context.Background()).SetResult(&out).Get(srv.URL); err == nil {
		t.Fatal("broken JSON must give a parse error")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("a parse failure must not be retried and must be exactly 1 request, got=%d", got)
	}
}

func TestRetry_RetriesOnBodyCutMidway(t *testing.T) {
	// Matching resty's default condition: an incomplete response body is a transport problem, io.ErrUnexpectedEOF.
	if !onlyIdempotent.condition(respFor("GET"), io.ErrUnexpectedEOF) {
		t.Error("a connection dropped halfway through the response body must be retried")
	}
}

func respFor(method string) *resty.Response {
	return &resty.Response{Request: &resty.Request{Method: method}}
}

func TestRetry_ActuallyResends(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Drop the connection outright to produce a transport-layer error
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	client, _ := newQuiet(t, c)

	client.R().SetContext(context.Background()).Get(srv.URL)
	if got := hits.Load(); got != 3 {
		t.Errorf("2 retries must be 3 requests in total, got=%d", got)
	}

	hits.Store(0)
	client.R().SetContext(context.Background()).Post(srv.URL)
	if got := hits.Load(); got != 1 {
		t.Errorf("a POST must not be retried and must be exactly 1 request, got=%d", got)
	}
}

func TestRetry_POSTRetriedWhenIdempotencyCheckOff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	c.RetryOnlyIdempotent = false
	client, _ := newQuiet(t, c)

	client.R().SetContext(context.Background()).Post(srv.URL)
	if got := hits.Load(); got != 3 {
		t.Errorf("with the restriction off a POST must be retried too, got=%d", got)
	}
}

// ---- the global instance ----

func TestC_FallbackWithTimeoutBeforeInit(t *testing.T) {
	// A zero timeout means "never time out": one such request during shutdown would stall the whole exit.
	emptyCell(t)

	if C() == nil {
		t.Fatal("a usable instance must be returned at any time")
	}
	if got := C().GetClient().Timeout; got != fallbackTimeout {
		t.Errorf("the fallback instance must carry a timeout, got=%v", got)
	}
	if got := RawClient().Timeout; got != fallbackTimeout {
		t.Errorf("the fallback native client must carry a timeout too, got=%v", got)
	}
}

func TestR_BindsCtx(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	if got := R(ctx).Context().Value(key{}); got != "v" {
		t.Errorf("R must bind the ctx passed in, got=%v", got)
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// This is the only wire between this package and the framework: a missing hook registration or a wrong stage shows
	// up as "the config has no effect" or "it is ready later than what uses it", and nothing else catches it.
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xhttp" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("no start hook was registered")
	}
	if got.Stage != hook.StageClient {
		t.Errorf("the outbound client must be ready before business code (StageClient), got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xhttp" {
			stopped = true
			if e.Stage != hook.StageClient {
				// If start and stop are not on the same stage, it is closed while business code is still using it.
				t.Errorf("the stop hook must be on StageClient too, got=%v", e.Stage)
			}
		}
	}
	if stopped != true {
		t.Errorf("the stop hook registration is wrong, got=%v", stopped)
	}
}

func TestInit_WorksWithoutConfig(t *testing.T) {
	// Unlike xgorm / xredis: an HTTP client connects to no external resource, so it must give a usable one even unconfigured.
	withMetrics(t)
	initComponent(t, DefaultConfig())

	if C().GetClient().Timeout != DefaultConfig().Timeout {
		t.Errorf("the default timeout must be used, got=%v", C().GetClient().Timeout)
	}
	if RawClient() == nil {
		t.Error("the native client must be usable too")
	}

	if err := closeXHttp(context.Background()); err != nil {
		t.Errorf("closing must not be an error: %v", err)
	}
	// After closing it falls back rather than being nil: a component may still send a request while shutting down,
	// and failing with a timeout beats getting a closed client.
	if got := C().GetClient().Timeout; got != fallbackTimeout {
		t.Errorf("after closing it must fall back, got=%v", got)
	}
}

// emptyCell clears the global instance, simulating "the framework has not initialized yet".
func emptyCell(t *testing.T) {
	t.Helper()
	mu.Lock()
	old := current
	current = nil
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); current = old; mu.Unlock() })
}

// initComponent walks the path the framework really takes: set this module up from this configuration.
func initComponent(t *testing.T, c Config) {
	t.Helper()
	if err := install(c); err != nil {
		t.Fatalf("initialization failed: %v", err)
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("the default configuration must be valid: %v", err)
	}
	bad := DefaultConfig()
	bad.RetryCount = -1
	if err := bad.Validate(); err == nil {
		t.Error("a negative retry count must be an error")
	}
	bad = DefaultConfig()
	bad.MaxIdleConns = -1
	if err := bad.Validate(); err == nil {
		t.Error("a negative connection count must be an error")
	}
	bad = DefaultConfig()
	bad.MaxConnsPerHost = -1
	if err := bad.Validate(); err == nil {
		t.Error("a negative per-host connection limit must be an error")
	}
}

func TestTunedTransport_UntouchedDefaultsMatchDocs(t *testing.T) {
	// The comments and xhttp/README.md state these numbers (Go 1.25, resty v2.17.2).
	// When an upgrade changes them this goes red first and the documentation follows.
	tr := tunedTransport(DefaultConfig(), nil).(*http.Transport)
	if tr.Proxy == nil || tr.TLSHandshakeTimeout != 10*time.Second || tr.ResponseHeaderTimeout != 0 ||
		tr.MaxConnsPerHost != 0 || !tr.ForceAttemptHTTP2 {
		t.Errorf("the standard library defaults changed: proxy=%v tls=%v header=%v maxConns=%d h2=%v",
			tr.Proxy != nil, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.MaxConnsPerHost, tr.ForceAttemptHTTP2)
	}
	client, _ := newQuiet(t, DefaultConfig())
	if client.GetClient().CheckRedirect != nil {
		t.Error("resty now sets a redirect policy of its own, so \"the standard library default, at most 10\" in the docs no longer holds")
	}
}

func TestNew_MaxConnsPerHostApplied(t *testing.T) {
	// Unlimited opens as many connections as the concurrency asks for; with a limit, excess requests queue for one.
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := DefaultConfig()
	c.MaxConnsPerHost = 2
	client, _ := newQuiet(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.R().Get(srv.URL); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if conns > 2 {
		t.Errorf("with MaxConnsPerHost=2 the downstream must see at most 2 connections, got=%d", conns)
	}
}

func TestNew_DirectCallAlsoValidatesConfig(t *testing.T) {
	// A caller going straight to New without the config file has no xconfig.Unmarshal to call Validate for them.
	bad := DefaultConfig()
	bad.DialTimeout = -time.Second
	_, _, err := New(bad)
	if err == nil {
		t.Fatal("an invalid configuration must fail a direct New too")
	}
	if xerror.Module(err) != "xhttp" || strings.Count(err.Error(), "xhttp") != 1 {
		t.Errorf("the error must be wrapped once by xhttp, got=%v", err)
	}
}

func TestSpanName_MethodOnly(t *testing.T) {
	// A path holds an id and a query often holds ids and tokens: putting them in the Span name would burst the cardinality and carry sensitive values out.
	req, _ := http.NewRequest("GET", "https://h/api/orders/42?token=hunter2&id=1", nil)
	if got := spanName("", req); got != "GET" {
		t.Errorf("the Span name must be the method alone, got=%q", got)
	}
	if strings.Contains(spanName("", req), "hunter2") {
		t.Error("a token appeared in the Span name")
	}
}

// newQuiet builds a client that logs nothing and gives it a metrics setup of its own.
//
// Every case gets a fresh registry: metrics are process-wide global state, and a conflict one case registers would
// stay there and fail New for every case after it -- which is exactly how the previous version bled across.
func newQuiet(t *testing.T, c Config) (*resty.Client, *xmetric.Metrics) {
	t.Helper()
	m := withMetrics(t)
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if silent {
		client.SetLogger(discardLogger{})
	}
	t.Cleanup(func() { closer.Close() })
	return client, m
}

type discardLogger struct{}

func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Warnf(string, ...any)  {}
func (discardLogger) Debugf(string, ...any) {}

func TestNew_CloseReallyClearsIdleConns(t *testing.T) {
	// Regression case. The previous version had the Closer call http.Client.CloseIdleConnections(),
	// which searches downwards by type assertion; with tracing on there is an otelhttp.Transport in between,
	// which does not implement the method, so the assertion stops there -- the whole call is a no-op,
	// and tracing is on by default. Asserting that "the wrapper implements the method" proves nothing;
	// the connections have to be really released.
	for _, trace := range []bool{false, true} {
		t.Run(fmt.Sprintf("Trace=%v", trace), func(t *testing.T) {
			var idle atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			}))
			// It has to be set before Start: changing it afterwards races the server goroutine already reading it
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				switch s {
				case http.StateIdle:
					idle.Add(1)
				case http.StateClosed, http.StateHijacked:
					idle.Add(-1)
				}
			}
			srv.Start()
			defer srv.Close()

			c := DefaultConfig()
			c.Trace = trace
			client, closer, err := New(c)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := client.R().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp

			waitFor(t, func() bool { return idle.Load() > 0 }, "there must be one idle connection once the request finishes")
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return idle.Load() == 0 }, "the idle connection must be released after closing")
		})
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(what)
}

func TestMetric_RetryDurationCoversWholeLogicalRequest(t *testing.T) {
	// resty resets Request.Time on every attempt, so resp.Time() is only the last attempt's duration.
	// The count is kept per "one logical request", so the duration has to be as well -- otherwise, during an incident,
	// the request count rises while the duration does not move and the monitoring looks unusually healthy.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // the first one fails, triggering a retry
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	const backoff = 150 * time.Millisecond
	c := DefaultConfig()
	c.Trace, c.Metric = false, false
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, backoff, backoff
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	hist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "probe_duration_seconds", Buckets: []float64{0.001, 10}},
		[]string{"method", "host", "status"})
	installMetrics(client, hist)
	client.AddRetryCondition(func(r *resty.Response, _ error) bool { return r.StatusCode() >= 500 })

	if _, err := client.R().Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits.Load() < 2 {
		t.Fatalf("it must have retried, the server saw only %d", hits.Load())
	}

	got := histogramSum(t, hist)
	if got < backoff.Seconds() {
		t.Errorf("the duration must cover the whole logical request (the %v backoff included), got=%.3fs", backoff, got)
	}
}

func histogramSum(t *testing.T, h *prometheus.HistogramVec) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleSum()
			}
		}
	}
	t.Fatal("no histogram sample was collected")
	return 0
}

func TestNew_CtxCapsWholeLogicalRequest(t *testing.T) {
	// Timeout governs one attempt. With RetryCount on, the worst case is
	// (RetryCount+1) x Timeout plus the backoff -- 300ms configured can really run to 1.2s.
	// The only thing that caps the whole logical request is the caller's ctx, which is pinned down here.
	// Timeout is 1s here: without honoring the ctx it would start at 4s, and the 2s bound is far from both the 400ms budget and 4s.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // stop when the client disconnects: srv.Close waits for the requests in flight
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.Timeout = time.Second
	c.RetryCount = 3
	c.RetryWaitTime, c.RetryMaxWaitTime = 10*time.Millisecond, 20*time.Millisecond
	c.Trace, c.Metric = false, false
	cli, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := cli.R().SetContext(ctx).Get(srv.URL); err == nil {
		t.Fatal("it must time out")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the ctx gave a 400ms budget and retries must not stretch it to %v", elapsed.Round(10*time.Millisecond))
	}
}

// keepGlobals records the globals install will change and restores them when the test ends.
func keepGlobals(t *testing.T) {
	t.Helper()
	mu.Lock()
	oldCurrent, oldCloser := current, liveCloser
	mu.Unlock()
	t.Cleanup(func() {
		_ = closeXHttp(context.Background())
		mu.Lock()
		current, liveCloser = oldCurrent, oldCloser
		mu.Unlock()
	})
}

func TestInitXHttp_NoConfigInstallsUsableDefaultClient(t *testing.T) {
	// Not installing it when the HTTP client is unconfigured would have C() fall back every time,
	// with another set of timeouts and retries -- while not configuring it should simply mean "use the defaults".
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXHttp(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	mu.Lock()
	got := current
	mu.Unlock()
	if got == nil {
		t.Fatal("no global client was installed")
	}
}

func TestInitXHttp_BadConfigFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XHttp:\n  TimeOut: 3s\n")

	if err := initXHttp(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup")
	}
}

func TestInitXHttp_InvalidValueFailsStartup(t *testing.T) {
	// A negative duration is a silent failure bought with one minus sign: a negative DialTimeout makes every request
	// fail with i/o timeout on the spot, while a negative Timeout is taken by the standard library as "no limit"
	// and the timeout protection disappears entirely -- neither leaves any sign.
	keepGlobals(t)
	for _, field := range []string{"Timeout", "DialTimeout", "IdleConnTimeout", "RetryWaitTime", "RetryMaxWaitTime"} {
		t.Run(field, func(t *testing.T) {
			xonetest.UseConfigYAML(t, "XHttp:\n  "+field+": -1s\n")
			if err := initXHttp(context.Background()); err == nil {
				t.Fatalf("a negative %s must fail startup", field)
			} else if !strings.Contains(err.Error(), field) {
				t.Errorf("the error must name the field, got=%v", err)
			} else if !xerror.Is(err, "xconfig") {
				// It is caught while the config is read (xconfig.Unmarshal calls Validate), not only at New
				t.Errorf("the error must come from the config-reading step, got=%v", err)
			}
		})
	}
}

func TestValidate_KeepAlive_AllowsNegative(t *testing.T) {
	// It is the one duration where a negative value is meaningful: the standard library takes it as "send no probes".
	c := DefaultConfig()
	c.DialKeepAlive = -1
	if err := c.Validate(); err != nil {
		t.Errorf("a negative DialKeepAlive is valid, got=%v", err)
	}
}

func TestInitXHttp_ConfigAppliedToGlobalClient(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XHttp:\n  Timeout: 7s\n  RetryCount: 4\n")

	if err := initXHttp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := C().GetClient().Timeout; got != 7*time.Second {
		t.Errorf("the timeout did not reach the client, got=%v", got)
	}
	if got := C().RetryCount; got != 4 {
		t.Errorf("the retry count did not reach the client, got=%v", got)
	}
}

func TestCloseXHttp_FallsBackToDefaultAfterClose(t *testing.T) {
	// C() has to stay usable after it is detached: another component's shutdown logic may still send a request.
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XHttp:\n  Timeout: 7s\n")
	if err := initXHttp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXHttp(context.Background()); err != nil {
		t.Fatalf("closing must not be an error: %v", err)
	}
	if C() == nil {
		t.Fatal("C() returned nil after closing, so the caller would hit a nil pointer")
	}
	if got := C().GetClient().Timeout; got == 7*time.Second {
		t.Error("the already closed client is still reachable")
	}
}

func TestCloseXHttp_CloseWithoutInstallIsSafe(t *testing.T) {
	keepGlobals(t)
	mu.Lock()
	current, liveCloser = nil, nil
	mu.Unlock()
	if err := closeXHttp(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

// ---- logs, span attributes, cookies ----

func TestNew_RestyLogsGoToSlogWithoutQuery(t *testing.T) {
	// Regression case. resty's logger writes to os.Stderr by default (grabbed the moment the client is built),
	// and with retries on it prints `WARN RESTY Get "http://…?token=…": …, Attempt 1` per failure
	// and one ERROR at the end -- past slog, with the token in the query string landing on disk as it is.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	var buf strings.Builder
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldLog) })

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 1, time.Millisecond, time.Millisecond
	c.Metric, c.Trace = false, false
	client, closer, err := New(c)
	if err != nil {
		os.Stderr = oldStderr
		t.Fatal(err)
	}
	defer closer.Close()
	_, reqErr := client.R().Get("http://127.0.0.1:1/x?token=hunter2")

	os.Stderr = oldStderr
	w.Close()
	stderr, _ := io.ReadAll(r)

	if reqErr == nil {
		t.Fatal("connecting to a port nobody listens on must fail")
	}
	if len(stderr) > 0 {
		t.Errorf("resty must not write to stderr directly, got=%s", stderr)
	}
	got := buf.String()
	if !strings.Contains(got, "Attempt") {
		t.Errorf("the retry warning must reach slog, got=%s", got)
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("a token from the query string reached the log: %s", got)
	}
}

func TestNew_RestyLogsScrubErrorArgsStructurally(t *testing.T) {
	// With Log off, resty's retry-path logs are printed: the *url.Error among the arguments has its query string removed by structure.
	// A space before the question mark in Location is not found by text.
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/a b?token=hunter2")
		w.WriteHeader(http.StatusFound)
	})
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 1, time.Millisecond, time.Millisecond
	c.Metric, c.Trace = false, false
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	client.SetRedirectPolicy(resty.NoRedirectPolicy())
	client.R().Get(srv.URL)
	got := buf.String()
	if !strings.Contains(got, "Attempt") || !strings.Contains(got, "/a%20b") {
		t.Fatalf("precondition: a refused redirect is retried too and resty prints its per-attempt line, got=%s", got)
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("a token from the query string reached the log: %s", got)
	}
}

func TestRestyLogger_TextScrubbed(t *testing.T) {
	// A URL in an argument that is not an error, or in the format string itself, has its query string and userinfo removed by text.
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	restyLogger{}.Warnf("see %s and https://u:p@h/y#frag", "http://h/x?token=hunter2")
	if got := buf.String(); !strings.Contains(got, `see http://h/x and https://h/y`) {
		t.Errorf("a URL in resty's log must have its query string, fragment and userinfo removed, got=%s", got)
	}
}

// lockedWriter: resty logs from its own goroutine while the test reads at the same time.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestScrubText_Query(t *testing.T) {
	for in, want := range map[string]string{
		`Get "http://h/x?token=a&b=c": dial tcp: refused, Attempt 1`: `Get "http://h/x": dial tcp: refused, Attempt 1`,
		`Post "https://h:8443/a/b#frag": EOF`:                        `Post "https://h:8443/a/b": EOF`,
		`two http://a/x?k=1 and https://b/y?k=2 end`:                 `two http://a/x and https://b/y end`,
		`no url here?token=a`:                                        `no url here?token=a`,
	} {
		if got := scrubText(in); got != want {
			t.Errorf("scrubText(%q)\n got=%q\nwant=%q", in, got, want)
		}
	}
}

func TestTransport_SpanHasNoQueryAndIsNotNamedByPath(t *testing.T) {
	// Regression case. otelhttp writes the full URL into url.full, so a token in the query string reached the
	// tracing backend with it (it only removes user:password); and the Span name was the real path,
	// so /users/42 and /users/43 were each a name and the cardinality grew with the number of users.
	old := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	srv, _ := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())
	if _, err := client.R().Get(srv.URL + "/users/42?token=hunter2#frag"); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("one Span must be produced, got=%d", len(spans))
	}
	if spans[0].Name != "GET" {
		t.Errorf("an outbound Span name must be the method alone, got=%q", spans[0].Name)
	}
	var full string
	for _, kv := range spans[0].Attributes {
		if strings.Contains(kv.Value.Emit(), "hunter2") {
			t.Errorf("the attribute %s carried a token out: %s", kv.Key, kv.Value.Emit())
		}
		if kv.Key == "url.full" {
			full = kv.Value.Emit()
		}
	}
	if full != srv.URL+"/users/42" {
		t.Errorf("url.full must have its query string removed and keep the rest, got=%q", full)
	}
}

func TestC_FallbackHasNoCookieJar(t *testing.T) {
	// Regression case. The fallback instance used to be a resty.New(), which brings a cookie jar,
	// while a configured instance (NewWithClient) does not. So requests sent before initialization and after
	// shutdown would carry a cookie set by service A into every call afterwards --
	// two entirely unrelated requests in one process sharing a session.
	emptyCell(t)
	var cookie atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "hunter2", Path: "/"})
			return
		}
		cookie.Store(r.Header.Get("Cookie"))
	}))
	t.Cleanup(srv.Close)

	if _, err := C().R().Get(srv.URL + "/set"); err != nil {
		t.Fatal(err)
	}
	if _, err := C().R().Get(srv.URL + "/get"); err != nil {
		t.Fatal(err)
	}
	if got, _ := cookie.Load().(string); got != "" {
		t.Errorf("the fallback instance must not remember a cookie another request set, got=%q", got)
	}
	if RawClient().Jar != nil {
		t.Error("the fallback instance's native client must carry no cookie jar, matching a configured instance")
	}
}
