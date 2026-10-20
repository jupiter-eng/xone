package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xecho/internal/peer"
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

// attrsOf is a span's attributes, with every value turned into a string.
func attrsOf(s tracetest.SpanStub) map[string]string {
	attrs := map[string]string{}
	for _, kv := range s.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	return attrs
}

func TestTrace_StartsSpanAndReturnsTraceID(t *testing.T) {
	spans := recording(t)
	var inHandler trace.SpanContext
	w := serve(t, get("/hello/42"), []echo.MiddlewareFunc{Trace()}, func(c echo.Context) error {
		inHandler = trace.SpanContextFromContext(c.Request().Context())
		return c.String(200, "ok")
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
	// This span is the one in the handler's ctx: passing c.Request().Context() downstream is what
	// keeps the trace connected.
	if inHandler.SpanID() != got[0].SpanContext.SpanID() || w.Header().Get(TraceIDHeader) != got[0].SpanContext.TraceID().String() {
		t.Errorf("the handler ctx and X-Trace-Id should both point at this span, got handler=%v header=%q", inHandler.SpanID(), w.Header().Get(TraceIDHeader))
	}
	if got[0].Name != "GET /hello/:id" || got[0].SpanKind != trace.SpanKindServer {
		t.Errorf("the span name should use the route template and the kind should be server, got=%q %v", got[0].Name, got[0].SpanKind)
	}
	attrs := attrsOf(got[0])
	if attrs["http.route"] != "/hello/:id" || attrs["url.path"] != "/hello/42" || attrs["http.request.method"] != "GET" {
		t.Errorf("the route and path attributes are wrong, got=%v", attrs)
	}
	if attrs["http.response.status_code"] != "200" {
		t.Errorf("the status code should be recorded, got=%v", attrs)
	}
	if _, ok := attrs["echo.errors"]; ok {
		t.Errorf("without an error there should be no echo.errors, got=%v", attrs)
	}
}

func TestTrace_ContinuesUpstreamTrace(t *testing.T) {
	spans := recording(t)
	req := get("/hello")
	// A valid W3C traceparent.
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	serve(t, req, []echo.MiddlewareFunc{Trace()}, statusOnly(200))

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
	// real failures no longer stand out. Both a status written directly and one echo rendered from
	// a returned error have to be right.
	for status, wantErr := range map[int]bool{200: false, 404: false, 400: false, 500: true, 503: true} {
		handlers := map[string]echo.HandlerFunc{"status written directly": statusOnly(status)}
		if status >= 400 {
			handlers["HTTPError returned"] = func(echo.Context) error { return echo.NewHTTPError(status) }
		}
		for name, h := range handlers {
			spans := recording(t)
			serve(t, get("/hello"), []echo.MiddlewareFunc{Trace()}, h)
			got := spans()
			if len(got) != 1 {
				t.Fatalf("status=%d should produce exactly one span", status)
			}
			if isErr := got[0].Status.Code == codes.Error; isErr != wantErr {
				t.Errorf("%s status=%d should be marked as an error=%v, got=%v", name, status, wantErr, isErr)
			}
			if a := attrsOf(got[0]); a["http.response.status_code"] != strconv.Itoa(status) {
				t.Errorf("%s: the span status code should be the rendered %d, got=%v", name, status, a["http.response.status_code"])
			}
		}
	}
}

func TestTrace_ReturnedErrorRecordedOnSpanAndResponseKeepsTraceID(t *testing.T) {
	// An error returned by the handler goes into the span's echo.errors (this one carries
	// password=, so the whole thing is masked); the error response echo renders still carries
	// X-Trace-Id.
	spans := recording(t)
	w := httptest.NewRecorder()
	errorEcho(Trace()).ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	a := attrsOf(got[0])
	if a["echo.errors"] != Redacted || a["http.response.status_code"] != "500" || got[0].Status.Code != codes.Error {
		t.Errorf("the span should record the error and a 500 and be marked as an error, got=%v %v", a, got[0].Status)
	}
	if w.Code != 500 || w.Header().Get(TraceIDHeader) == "" {
		t.Errorf("an error response should carry X-Trace-Id too, got=%d %q", w.Code, w.Header().Get(TraceIDHeader))
	}
}

func TestTrace_UnmatchedRouteUsesFixedValue(t *testing.T) {
	// With real paths, a scanner hitting a few URLs blows up the cardinality of both traces and
	// metrics. A wrong method (405) is recorded as unmatched too: echo hands out that route's
	// template, which would not match xgin.
	for _, c := range errorCases {
		if c.route != "unmatched" {
			continue
		}
		spans := recording(t)
		errorEcho(Trace()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		got := spans()
		if len(got) != 1 {
			t.Fatalf("%s: exactly one span should be produced, got=%d", c.name, len(got))
		}
		if want := c.method + " unmatched"; got[0].Name != want || attrsOf(got[0])["http.route"] != "unmatched" {
			t.Errorf("%s: the span name should be %q, got=%q %v", c.name, want, got[0].Name, attrsOf(got[0]))
		}
	}
}

func TestTrace_CustomMethodNormalizedToOTHER(t *testing.T) {
	// Same reasoning as the metrics: the span name is the dimension the tracing backend indexes,
	// and the method is a free-form token anyone can set to CUSTOM1 or CUSTOM2. Copied verbatim,
	// every new value adds another span name. The original method stays in
	// http.request.method_original and remains visible while debugging.
	spans := recording(t)
	serve(t, httptest.NewRequest("CUSTOM1", "/hello", nil), []echo.MiddlewareFunc{Trace()}, statusOnly(200))

	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	if got[0].Name != "OTHER /hello" {
		t.Errorf("a custom method should fold into OTHER, got=%q", got[0].Name)
	}
	attrs := attrsOf(got[0])
	if attrs["http.request.method"] != "OTHER" || attrs["http.request.method_original"] != "CUSTOM1" {
		t.Errorf("the method attributes are wrong, got=%v", attrs)
	}
}

// trustProbe is a Propagator that only records whether the carrier handed to Extract declared
// the peer trusted.
type trustProbe struct{ got *bool }

func (p trustProbe) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	t, ok := c.(interface{ TrustedPeer() bool })
	*p.got = ok && t.TrustedPeer()
	return ctx
}
func (trustProbe) Inject(context.Context, propagation.TextMapCarrier) {}
func (trustProbe) Fields() []string                                   { return nil }

func TestTrace_PeerTrustFollowsXechoMarker(t *testing.T) {
	// Forwarded headers are accepted only from a trusted peer. xecho decides that from
	// TrustedProxies; this layer only hands the verdict to xtrace, and no mark (the middleware used
	// on its own) means untrusted.
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	markTrusted := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { c.Set(peer.TrustedKey, true); return next(c) }
	}
	for name, c := range map[string]struct {
		mws  []echo.MiddlewareFunc
		want bool
	}{
		"on its own":                         {[]echo.MiddlewareFunc{Trace()}, false},
		"xecho marked it trusted":            {[]echo.MiddlewareFunc{markTrusted, Trace()}, true},
		"Propagate on its own":               {[]echo.MiddlewareFunc{Propagate()}, false},
		"Propagate, xecho marked it trusted": {[]echo.MiddlewareFunc{markTrusted, Propagate()}, true},
	} {
		got = !c.want
		serve(t, get("/hello"), c.mws, statusOnly(200))
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
	w := serve(t, req, []echo.MiddlewareFunc{Propagate()}, func(c echo.Context) error {
		seen = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
		return c.NoContent(200)
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

func TestTrace_ErrAbortHandlerAbortRecordedAsError(t *testing.T) {
	// An aborted request passes through Trace carrying a panic, so cleanup written after next(c)
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
	if a := attrsOf(got[0]); a["http.response.status_code"] != "499" {
		t.Errorf("the status code should be recorded as 499, got=%v", a["http.response.status_code"])
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
	serve(t, get("/hello/42"), []echo.MiddlewareFunc{Metric()}, statusOnly(201))

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `http_requests_total{method="GET",route="/hello/:id",status="201"} 1`) {
		t.Errorf("requests should be counted by method, route and status\ngot=\n%s", out)
	}
	if !strings.Contains(out, `http_request_duration_seconds_count{method="GET",route="/hello/:id",status="201"} 1`) {
		t.Errorf("the duration should be recorded\ngot=\n%s", out)
	}
}

func TestMetric_RouteUsesTemplate(t *testing.T) {
	// Labelling by real path grows the time series without bound with every id in a URL, which
	// buries Prometheus.
	m := withMetrics(t)
	for _, p := range []string{"/hello/1", "/hello/2", "/hello/3"} {
		serve(t, get(p), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
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
	e := errorEcho(Metric())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/12345"))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/only-get", nil))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("OPTIONS", "/only-get", nil))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/users", nil)) // lands on the group fallback

	out := testkit.Scrape(m.Handler)
	for _, want := range []string{
		`http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`http_requests_total{method="POST",route="unmatched",status="405"} 1`,
		// OPTIONS is answered by echo itself with 204 and Allow, and is no registered route either
		`http_requests_total{method="OPTIONS",route="unmatched",status="204"} 1`,
		// A wrong method under the group: the fallback wins over 405 and echo answers 404
		`http_requests_total{method="POST",route="unmatched",status="404"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%s should be present\ngot=\n%s", want, out)
		}
	}
	if strings.Contains(out, `route="/api/v1/*"`) {
		t.Errorf("a request landing on the group fallback must not be recorded as that template\ngot=\n%s", out)
	}
	if strings.Contains(out, `route="/only-get"`) {
		t.Errorf("a request with the wrong method must not be recorded as that route\ngot=\n%s", out)
	}
}

func TestMetric_ReturnedErrorRecordedWithRenderedStatus(t *testing.T) {
	// Without rendering in this layer, every one of these requests shows up as 200 in the error
	// rate.
	m := withMetrics(t)
	e := errorEcho(Metric())
	for _, c := range errorCases {
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
	}
	out := testkit.Scrape(m.Handler)
	for _, want := range []string{`route="/plain",status="500"} 1`, `route="/teapot",status="418"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("%s should be present\ngot=\n%s", want, out)
		}
	}
	if strings.Contains(out, `status="200"`) {
		t.Errorf("a failing request must not be recorded as 200\ngot=\n%s", out)
	}
}

func TestMetric_PanicStillCounted(t *testing.T) {
	// Without counting it, the requests that went wrong vanish from the error-rate metric.
	m := withMetrics(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []echo.MiddlewareFunc{Metric()}, func(c echo.Context) error { panic("boom") })
	}()
	if out := testkit.Scrape(m.Handler); !strings.Contains(out, "http_requests_total") {
		t.Errorf("a panicking request should be counted too\ngot=\n%s", out)
	}
}

func TestMetric_CustomMethodNormalizedToOTHER(t *testing.T) {
	// The route dimension already hides the ids in URLs behind templates, while the method
	// dimension is copied straight from the request - and an HTTP method is a free-form token
	// anyone can set to CUSTOM1 or CUSTOM2, each new value adding a time series that is never
	// evicted.
	//
	// This goes through the real middleware rather than calling web.NormalizeMethod directly:
	// testing that function alone would still pass with the call site bypassed (the mapping table
	// itself is covered in internal/web).
	m := withMetrics(t)
	for _, method := range []string{"CUSTOM1", "FOOBAR", "PROPFIND"} {
		serve(t, httptest.NewRequest(method, "/hello", nil), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
	}
	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `method="OTHER",route="/hello",status="200"} 3`) {
		t.Errorf("a custom method should fold into OTHER\ngot=\n%s", out)
	}
	for _, leaked := range []string{`method="CUSTOM1"`, `method="FOOBAR"`, `method="PROPFIND"`} {
		if strings.Contains(out, leaked) {
			t.Errorf("%s reached the labels, so the caller can blow up the time series\ngot=\n%s", leaked, out)
		}
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

func TestMetric_TwoInstancesShareTheSameCollectors(t *testing.T) {
	// On a duplicate registration of a metric with the same name and labels, xmetric returns the
	// existing one: two servers (or xgin and xecho) record onto the same metrics without a
	// registration conflict.
	m := withMetrics(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
	serve(t, get("/hello"), []echo.MiddlewareFunc{Metric()}, statusOnly(200))
	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `http_requests_total{method="GET",route="/hello",status="200"} 2`) {
		t.Errorf("two middleware instances should record onto the same metrics\ngot=\n%s", out)
	}
}

func TestTrace_ErrorTextIsRedactedOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Trace()}, func(c echo.Context) error {
		return errors.New("login failed password=hunter2")
	})
	if got := attrsOf(spans()[0])["echo.errors"]; got != Redacted {
		t.Errorf("echo.errors on the span should be masked, got=%q", got)
	}
}

func TestTrace_DSNPasswordInErrorIsMaskedOnSpan(t *testing.T) {
	spans := recording(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Trace()}, func(c echo.Context) error {
		return errors.New("dial postgres://app:hunter2@db:5432/prod failed")
	})
	if got, want := attrsOf(spans()[0])["echo.errors"], "dial postgres://app:"+Redacted+"@db:5432/prod failed"; got != want {
		t.Errorf("echo.errors=%q, want %q", got, want)
	}
}

// ---- The 499 rule: only for responses that really never went out ----

// recordedAs runs one request and returns the status the client received, the status recorded by
// the access log, the metrics and the span, and whether the span was marked as an error.
func recordedAs(t *testing.T, req *http.Request, h echo.HandlerFunc) (sent int, logged any, metric string, span string, spanErr bool) {
	t.Helper()
	lines := capture(t)
	spans := recording(t)
	m := withMetrics(t)
	w := serve(t, req, []echo.MiddlewareFunc{Trace(), Log(), Metric(), Recover(nil)}, h)

	logged = accessLogs(lines())[0]["status"]
	out := testkit.Scrape(m.Handler)
	if i := strings.Index(out, `http_requests_total{method="GET",route="/hello",status="`); i >= 0 {
		rest := out[i+len(`http_requests_total{method="GET",route="/hello",status="`):]
		metric = rest[:strings.Index(rest, `"`)]
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	return w.Code, logged, metric, attrsOf(got[0])["http.response.status_code"], got[0].Status.Code == codes.Error
}

func TestReturnedErrAbortHandlerRecordsWhatWasSent(t *testing.T) {
	// Regression: when a handler returns (rather than panics with) an error wrapping
	// http.ErrAbortHandler, the connection was never broken and the client received the 500
	// HTTPErrorHandler rendered, yet all three recorded 499. Only a panic makes net/http break the
	// connection.
	sent, logged, metric, span, spanErr := recordedAs(t, get("/hello"), func(echo.Context) error {
		return fmt.Errorf("proxy: %w", http.ErrAbortHandler)
	})
	if sent != 500 || logged != float64(500) || metric != "500" || span != "500" || !spanErr {
		t.Errorf("a returned error should be recorded as the 500 that was sent, got sent=%d log=%v metric=%s span=%s spanErr=%v", sent, logged, metric, span, spanErr)
	}
}

// goneRequest is a request whose client is already gone: net/http cancels the request ctx when
// the connection breaks (a reset stream on HTTP/2) or is force-closed.
func goneRequest() *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return get("/hello").WithContext(ctx)
}

func TestClientGoneBeforeAnythingSentRecordedAs499(t *testing.T) {
	// Regression: after the client disconnects a handler conventionally returns ctx.Err(), which
	// used to be recorded as 500, mark the span as an error and land in the 5xx metrics - while
	// the client received nothing at all. c.Error writing an error response to a broken connection
	// still sets Committed and records Status 500 (measured), so the decision has to be made before
	// handing it over.
	for name, h := range map[string]echo.HandlerFunc{
		"return ctx.Err()":                 func(c echo.Context) error { return c.Request().Context().Err() },
		"returns without writing anything": func(echo.Context) error { return nil },
	} {
		_, logged, metric, span, spanErr := recordedAs(t, goneRequest(), h)
		if logged != float64(499) || metric != "499" || span != "499" || spanErr {
			t.Errorf("%s: should record 499 with the span unmarked (this is not a server error), got log=%v metric=%s span=%s spanErr=%v", name, logged, metric, span, spanErr)
		}
	}
}

func TestClientGoneAfterResponseCommittedKeepsSentStatus(t *testing.T) {
	// The response has started going out: the client received at least part of it, so the status
	// that was sent is what gets recorded.
	_, logged, metric, span, _ := recordedAs(t, goneRequest(), func(c echo.Context) error {
		_ = c.String(201, "partial")
		return c.Request().Context().Err()
	})
	if logged != float64(201) || metric != "201" || span != "201" {
		t.Errorf("the 201 that was sent should be recorded, got log=%v metric=%s span=%s", logged, metric, span)
	}
}

func TestClientDeadlineIsNotClientGone(t *testing.T) {
	// Middleware like ContextTimeout puts a deadline on the request: when it expires the client is
	// still waiting and receives a 503, not a 499.
	_, logged, metric, _, _ := recordedAs(t, get("/hello"), func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 0)
		defer cancel()
		<-ctx.Done()
		return echo.ErrServiceUnavailable.WithInternal(ctx.Err())
	})
	if logged != float64(503) || metric != "503" {
		t.Errorf("503 should be recorded, got log=%v metric=%s", logged, metric)
	}
}

func TestClientGoneIgnoresContextsSetByInnerLayers(t *testing.T) {
	// A ctx installed by an inner layer belongs to the business: it wraps one, cancels it on the
	// way out, and outside that layer it reads as Canceled while the client is still waiting.
	_, logged, metric, span, _ := recordedAs(t, get("/hello"), func(c echo.Context) error {
		ctx, cancel := context.WithCancel(c.Request().Context())
		cancel()
		c.SetRequest(c.Request().WithContext(ctx))
		return nil
	})
	if logged != float64(200) || metric != "200" || span != "200" {
		t.Errorf("the client is still there, so 200 should be recorded, got log=%v metric=%s span=%s", logged, metric, span)
	}
}
