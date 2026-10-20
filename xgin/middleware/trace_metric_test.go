package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xgin/internal/peer"
	"github.com/jupiter-eng/xone/xmetric"
)

// recording installs a tracing setup of its own and returns a function yielding the ended spans.
func recording(t *testing.T) func() tracetest.SpanStubs {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))

	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
	})

	return exp.GetSpans
}

func TestTrace_StartsSpanAndReturnsTraceID(t *testing.T) {
	spans := recording(t)
	w := serve(t, get("/hello/42"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
		c.String(200, "ok")
	})

	if w.Header().Get(TraceIDHeader) == "" {
		t.Error("the response header should carry the TraceID, or one call does not lead to its trace")
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	// The route template rather than the real path: naming by real path grows the span names with
	// the number of users.
	if got[0].Name != "GET /hello/:id" {
		t.Errorf("the span name should use the route template, got=%q", got[0].Name)
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["http.route"] != "/hello/:id" || attrs["url.path"] != "/hello/42" {
		t.Errorf("the route and path attributes are wrong, got=%v", attrs)
	}
	if attrs["http.response.status_code"] != "200" {
		t.Errorf("the status code should be recorded, got=%v", attrs)
	}
}

func TestTrace_ContinuesUpstreamTrace(t *testing.T) {
	spans := recording(t)
	req := get("/hello")
	// A valid W3C traceparent.
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	serve(t, req, []gin.HandlerFunc{Trace()}, func(c *gin.Context) { c.Status(200) })

	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	if id := got[0].SpanContext.TraceID().String(); id != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("the upstream TraceID should be joined, got=%s", id)
	}
}

func TestTrace_OnlyServerErrorsMarkedAsError(t *testing.T) {
	// A 4xx means the client sent something wrong; marking it as an error floods the trace until
	// real failures no longer stand out.
	for status, wantErr := range map[int]bool{200: false, 404: false, 400: false, 500: true, 503: true} {
		spans := recording(t)
		serve(t, get("/hello"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
			c.Status(status)
		})
		got := spans()
		if len(got) != 1 {
			t.Fatalf("status=%d should produce exactly one span", status)
		}
		isErr := got[0].Status.Code == codes.Error
		if isErr != wantErr {
			t.Errorf("status=%d should be marked as an error=%v, got=%v", status, wantErr, isErr)
		}
	}
}

func TestTrace_UnmatchedRouteUsesFixedValue(t *testing.T) {
	// With real paths, a scanner hitting a few URLs blows up the cardinality of both traces and
	// metrics.
	spans := recording(t)
	e := gin.New()
	e.Use(Trace())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/whatever/123"))

	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	if strings.Contains(got[0].Name, "whatever") {
		t.Errorf("an unmatched route must not put the real path into the span name, got=%q", got[0].Name)
	}
}

func TestTrace_CustomMethodNormalizedToOTHER(t *testing.T) {
	// Same reasoning as the metrics: the span name is the dimension the tracing backend indexes,
	// and the method is a free-form token anyone can set to CUSTOM1 or CUSTOM2. Copied verbatim,
	// every new value adds another span name. The original method stays in
	// http.request.method_original and remains visible while debugging.
	spans := recording(t)
	serve(t, httptest.NewRequest("CUSTOM1", "/hello", nil), []gin.HandlerFunc{Trace()},
		func(c *gin.Context) { c.Status(200) })

	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	if !strings.HasPrefix(got[0].Name, "OTHER ") {
		t.Errorf("a custom method should fold into OTHER, got=%q", got[0].Name)
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["http.request.method"] != "OTHER" || attrs["http.request.method_original"] != "CUSTOM1" {
		t.Errorf("the method attributes are wrong, got=%v", attrs)
	}
}

// trustProbe is a Propagator that only records whether the carrier handed to Extract declared the
// peer trusted.
type trustProbe struct{ got *bool }

func (p trustProbe) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	t, ok := c.(interface{ TrustedPeer() bool })
	*p.got = ok && t.TrustedPeer()
	return ctx
}
func (trustProbe) Inject(context.Context, propagation.TextMapCarrier) {}
func (trustProbe) Fields() []string                                   { return nil }

func TestTrace_PeerTrustFollowsXginMarker(t *testing.T) {
	// Forwarded headers are accepted only from a trusted peer. xgin decides that from
	// TrustedProxies; this layer only hands the verdict to xtrace, and no mark (the middleware used
	// on its own) means untrusted.
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	markTrusted := func(c *gin.Context) { c.Set(peer.TrustedKey, true) }
	for name, c := range map[string]struct {
		mws  []gin.HandlerFunc
		want bool
	}{
		"on its own":                        {[]gin.HandlerFunc{Trace()}, false},
		"xgin marked it trusted":            {[]gin.HandlerFunc{markTrusted, Trace()}, true},
		"Propagate on its own":              {[]gin.HandlerFunc{Propagate()}, false},
		"Propagate, xgin marked it trusted": {[]gin.HandlerFunc{markTrusted, Propagate()}, true},
	} {
		got = !c.want
		serve(t, get("/hello"), c.mws, func(c *gin.Context) { c.Status(200) })
		if got != c.want {
			t.Errorf("%s: the carrier declared trusted=%v, want %v", name, got, c.want)
		}
	}
}

func TestPropagate_ContinuesUpstreamTraceWithoutSpan(t *testing.T) {
	spans := recording(t)
	req := get("/hello")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	var seen string
	w := serve(t, req, []gin.HandlerFunc{Propagate()}, func(c *gin.Context) {
		seen = trace.SpanContextFromContext(c.Request.Context()).TraceID().String()
		c.Status(200)
	})
	if seen != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("the handler ctx should hold the upstream trace identifiers, got=%q", seen)
	}
	if n := len(spans()); n != 0 {
		t.Errorf("Propagate opens no span, got=%d", n)
	}
	if w.Header().Get(TraceIDHeader) != "" {
		t.Error("with no span there is no X-Trace-Id to echo back")
	}
}

// ---- Metric ----

// withMetrics installs a metrics setup of its own.
func withMetrics(t *testing.T) *xmetric.Metrics {
	t.Helper()
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	m.Install()
	return m
}

func TestMetric_RecordsCountAndDuration(t *testing.T) {
	m := withMetrics(t)
	serve(t, get("/hello/42"), []gin.HandlerFunc{Metric()}, func(c *gin.Context) {
		c.Status(201)
	})

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `http_requests_total{method="GET",route="/hello/:id",status="201"} 1`) {
		t.Errorf("requests should be counted by method, route and status\ngot=\n%s", out)
	}
	if !strings.Contains(out, "http_request_duration_seconds_count") {
		t.Errorf("the duration should be recorded\ngot=\n%s", out)
	}
}

func TestMetric_RouteUsesTemplate(t *testing.T) {
	// Labelling by real path grows the time series without bound with every id in a URL, which
	// buries Prometheus.
	m := withMetrics(t)
	for _, p := range []string{"/hello/1", "/hello/2", "/hello/3"} {
		serve(t, get(p), []gin.HandlerFunc{Metric()}, func(c *gin.Context) { c.Status(200) })
	}

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `route="/hello/:id",status="200"} 3`) {
		t.Errorf("the three requests should fold into one series\ngot=\n%s", out)
	}
	if strings.Contains(out, `route="/hello/1"`) {
		t.Errorf("the real path must not reach the labels\ngot=\n%s", out)
	}
}

func TestMetric_UnmatchedRouteUsesFixedValue(t *testing.T) {
	m := withMetrics(t)
	e := gin.New()
	e.Use(Metric())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/12345"))

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `route="unmatched"`) {
		t.Errorf("an unmatched route should be recorded as the fixed value\ngot=\n%s", out)
	}
}

func TestMetric_PanicStillCounted(t *testing.T) {
	// Without counting it, the requests that went wrong vanish from the error-rate metric.
	m := withMetrics(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []gin.HandlerFunc{Metric()}, func(c *gin.Context) { panic("boom") })
	}()

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, "http_requests_total") {
		t.Errorf("a panicking request should be counted too\ngot=\n%s", out)
	}
}

var _ = http.StatusOK

func TestMetric_CustomMethodNormalizedToOTHER(t *testing.T) {
	// The route dimension already hides the ids in URLs behind templates, while the method
	// dimension is copied straight from the request - and an HTTP method is a free-form token
	// anyone can set to CUSTOM1 or CUSTOM2, each new value adding a time series that is never
	// evicted.
	//
	// This part goes through the real middleware rather than calling web.NormalizeMethod directly:
	// mutation testing found that testing the function alone still passes with the call site
	// bypassed (`web.NormalizeMethod(m)` -> `m`) — the function itself is right, nobody uses it, and
	// that is exactly the shape of this bug.
	m := withMetrics(t)
	for _, method := range []string{"CUSTOM1", "FOOBAR", "PROPFIND"} {
		serve(t, httptest.NewRequest(method, "/hello", nil), []gin.HandlerFunc{Metric()},
			func(c *gin.Context) { c.Status(200) })
	}
	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `method="OTHER"`) {
		t.Errorf("a custom method should fold into OTHER\ngot=\n%s", out)
	}
	for _, leaked := range []string{`method="CUSTOM1"`, `method="FOOBAR"`, `method="PROPFIND"`} {
		if strings.Contains(out, leaked) {
			t.Errorf("%s reached the labels, so the caller can blow up the time series\ngot=\n%s", leaked, out)
		}
	}

	// The mapping table itself is covered in internal/web (TestNormalizeMethod).
}

func TestTrace_ErrAbortHandlerAbortRecordedAsError(t *testing.T) {
	// An aborted request passes through Trace carrying a panic, so cleanup written after c.Next()
	// never runs; and the status code read back is the 200 already sent, so the span would look
	// like a success.
	spans := recording(t)
	serveAborted(t, Trace())

	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	if got[0].Status.Code != codes.Error {
		t.Errorf("an aborted request should be marked as an error, got=%v", got[0].Status)
	}
	found := false
	for _, a := range got[0].Attributes {
		if a.Key == "http.response.status_code" {
			found = true
			if a.Value.AsInt64() != statusAborted {
				t.Errorf("the status code should be recorded as %d, got=%d", statusAborted, a.Value.AsInt64())
			}
		}
	}
	if !found {
		t.Error("the span carries no status code")
	}
}

func TestMetric_ErrAbortHandlerAbortRecordedAs499(t *testing.T) {
	// Recorded as 200, a truncated response vanishes from the error rate.
	m := withMetrics(t)
	serveAborted(t, Metric())

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `status="499"`) {
		t.Errorf("an aborted request should be recorded as 499\ngot=\n%s", out)
	}
}

func TestTrace_ErrorTextIsRedactedOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
		_ = c.Error(errors.New("login failed password=hunter2"))
		c.Status(500)
	})
	got := "(not written)"
	for _, a := range spans()[0].Attributes {
		if a.Key == "gin.errors" {
			got = a.Value.AsString()
		}
	}
	if got != Redacted {
		t.Errorf("gin.errors on the span should be masked, got=%q", got)
	}
}

func TestTrace_ErrorTextKeepsSeparationAndMasksDSNOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
		_ = c.Error(errors.New("dial postgres://app:hunter2@db:5432/prod failed"))
		_ = c.Error(errors.New("retry gave up"))
		c.Status(500)
	})
	got := "(not written)"
	for _, a := range spans()[0].Attributes {
		if a.Key == "gin.errors" {
			got = a.Value.AsString()
		}
	}
	if want := "Error #01: dial postgres://app:" + Redacted + "@db:5432/prod failed; Error #02: retry gave up"; got != want {
		t.Errorf("gin.errors=%q, want %q", got, want)
	}
}

// ---- The 499 rule: only for responses that really never went out ----

// recordedAs runs one request and returns the status recorded by the access log, the metrics and
// the span, and whether the span was marked as an error.
func recordedAs(t *testing.T, req *http.Request, h gin.HandlerFunc) (logged any, metric string, span string, spanErr bool) {
	t.Helper()
	lines := capture(t)
	spans := recording(t)
	m := withMetrics(t)
	serve(t, req, []gin.HandlerFunc{Trace(), Log(), Metric(), Recover(nil)}, h)

	var access []map[string]any
	for _, l := range lines() {
		if l["msg"] == "request completed" {
			access = append(access, l)
		}
	}
	logged = access[0]["status"]
	out := testkit.Scrape(m.Handler)
	const prefix = `http_requests_total{method="GET",route="/hello",status="`
	if i := strings.Index(out, prefix); i >= 0 {
		rest := out[i+len(prefix):]
		metric = rest[:strings.Index(rest, `"`)]
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	var code string
	for _, kv := range got[0].Attributes {
		if kv.Key == "http.response.status_code" {
			code = kv.Value.Emit()
		}
	}
	return logged, metric, code, got[0].Status.Code == codes.Error
}

// goneRequest is a request whose client is already gone: net/http cancels the request ctx when
// the connection breaks (a reset stream on HTTP/2) or is force-closed.
func goneRequest() *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return get("/hello").WithContext(ctx)
}

func TestClientGoneBeforeAnythingSentRecordedAs499(t *testing.T) {
	// Regression: after the client disconnects, a handler returning without writing anything used
	// to be recorded as a 200 with bytes_out 0 — while the client received nothing at all. Same
	// rule as xecho.
	logged, metric, span, spanErr := recordedAs(t, goneRequest(), func(c *gin.Context) {
		_ = c.Error(c.Request.Context().Err()) //nolint:errcheck // only recording it
	})
	if logged != float64(499) || metric != "499" || span != "499" || spanErr {
		t.Errorf("should record 499 with the span unmarked (this is not a server error), got log=%v metric=%s span=%s spanErr=%v", logged, metric, span, spanErr)
	}
}

func TestClientGoneAfterResponseWrittenKeepsSentStatus(t *testing.T) {
	// The response has started going out: the client received at least part of it, so the status
	// that was sent is what gets recorded.
	logged, metric, span, _ := recordedAs(t, goneRequest(), func(c *gin.Context) { c.String(201, "partial") })
	if logged != float64(201) || metric != "201" || span != "201" {
		t.Errorf("the 201 that was sent should be recorded, got log=%v metric=%s span=%s", logged, metric, span)
	}
}

func TestClientGoneIgnoresContextsSetByInnerMiddleware(t *testing.T) {
	// A ctx installed by an inner layer belongs to the business: it wraps a timeout, cancels it on
	// the way out, and outside that layer it reads as Canceled while the client is still waiting.
	logged, metric, span, _ := recordedAs(t, get("/hello"), func(c *gin.Context) {
		ctx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(ctx)
	})
	if logged != float64(200) || metric != "200" || span != "200" {
		t.Errorf("the client is still there, so 200 should be recorded, got log=%v metric=%s span=%s", logged, metric, span)
	}
}
