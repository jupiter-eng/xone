package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xbeego/internal/reqstate"
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

func attrsOf(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// withMetrics installs a metrics setup of its own.
func withMetrics(t *testing.T) *xmetric.Metrics {
	t.Helper()
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	m.Install()
	return m
}

// ---- Trace ----

func TestTrace_NamesTheSpanOnTheWayOut(t *testing.T) {
	// This layer sits outside beego's router, so the route template does not exist when the span
	// starts: it opens as the method alone and is renamed once the route is known.
	spans := recording(t)
	w := serve(t, get("/users/42"), []func(http.Handler) http.Handler{Capture(nil), Trace()},
		routed("/users/:id", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))

	if w.Header().Get(TraceIDHeader) == "" {
		t.Error("the response header should carry the TraceID, or one call does not lead to its trace")
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("exactly one span should be produced, got=%d", len(got))
	}
	if got[0].Name != "GET /users/:id" {
		t.Errorf("the span name should use the route template, got=%q", got[0].Name)
	}
	attrs := attrsOf(got[0])
	if attrs["http.route"] != "/users/:id" || attrs["url.path"] != "/users/42" {
		t.Errorf("the route and path attributes are wrong, got=%v", attrs)
	}
	if attrs["http.response.status_code"] != "200" {
		t.Errorf("the status code should be recorded, got=%v", attrs)
	}
	if got[0].SpanKind != trace.SpanKindServer {
		t.Errorf("an inbound request opens a server span, got=%v", got[0].SpanKind)
	}
}

func TestTrace_UnmatchedRouteKeepsTheSpanNamesBounded(t *testing.T) {
	spans := recording(t)
	serve(t, get("/nope/12345"), []func(http.Handler) http.Handler{Capture(nil), Trace()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })

	if got := spans(); got[0].Name != "GET unmatched" {
		t.Errorf("an unmatched route should use the fixed value, got=%q", got[0].Name)
	}
}

func TestTrace_CustomMethodIsFolded(t *testing.T) {
	// An HTTP method is a free-form token: copied into span names, anyone could fill the tracing
	// backend with CUSTOM1, CUSTOM2.
	spans := recording(t)
	req := httptest.NewRequest("CUSTOM1", "/x", nil)
	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Trace()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	got := spans()
	if !strings.HasPrefix(got[0].Name, "OTHER ") {
		t.Errorf("the method should be folded into the fixed set, got=%q", got[0].Name)
	}
	if attrsOf(got[0])["http.request.method_original"] != "CUSTOM1" {
		t.Errorf("the original should stay visible for debugging, got=%v", attrsOf(got[0]))
	}
}

func TestTrace_ContinuesUpstreamTrace(t *testing.T) {
	spans := recording(t)
	req := get("/x")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	var seen string
	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Trace()},
		func(w http.ResponseWriter, r *http.Request) {
			seen = trace.SpanContextFromContext(r.Context()).TraceID().String()
			w.WriteHeader(200)
		})

	if seen != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("the handler should be inside the upstream trace, got=%q", seen)
	}
	if got := spans(); got[0].Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("the span should hang under the upstream one, got=%v", got[0].Parent)
	}
}

func TestTrace_4xxIsNotAnError(t *testing.T) {
	// Marking a 4xx as an error floods the trace until real failures no longer stand out.
	spans := recording(t)
	serve(t, get("/x"), []func(http.Handler) http.Handler{Capture(nil), Trace()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })

	if got := spans(); got[0].Status.Code.String() == "Error" {
		t.Errorf("a 4xx is the client's mistake, not the server's, got=%v", got[0].Status)
	}
}

func TestTrace_5xxIsAnError(t *testing.T) {
	spans := recording(t)
	serve(t, get("/x"), []func(http.Handler) http.Handler{Capture(nil), Trace()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })

	if got := spans(); got[0].Status.Code.String() != "Error" {
		t.Errorf("a 5xx should mark the span as an error, got=%v", got[0].Status)
	}
}

func TestTrace_PanicStillEndsTheSpan(t *testing.T) {
	captureLog(t)
	spans := recording(t)
	serve(t, get("/x"), []func(http.Handler) http.Handler{Capture(nil), Trace(), Recover(nil)},
		func(http.ResponseWriter, *http.Request) { panic("boom") })

	got := spans()
	if len(got) != 1 {
		t.Fatalf("the span should be ended even when the handler panicked, got=%d", len(got))
	}
	if attrsOf(got[0])["beego.errors"] == "" {
		t.Errorf("the panic should be recorded on the span, got=%v", attrsOf(got[0]))
	}
}

func TestTrace_ForwardedHeadersOnlyFromATrustedPeer(t *testing.T) {
	// Accepting them from everyone lets a public client send X-Tenant-Id and have it treated as
	// coming from one of us, carried into every internal call.
	for _, tc := range []struct {
		name  string
		peers Peers
		want  bool
	}{
		{"trusted", proxies{trust: true}, true},
		{"untrusted", proxies{trust: false}, false},
		{"nobody configured", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var carrier propagation.TextMapCarrier
			serve(t, get("/x"), []func(http.Handler) http.Handler{Capture(tc.peers)},
				func(_ http.ResponseWriter, r *http.Request) {
					carrier = inbound(r, reqstate.From(r.Context()))
				})
			_, got := carrier.(trustedCarrier)
			if got != tc.want {
				t.Errorf("the carrier should report trusted=%v, got=%v", tc.want, got)
			}
		})
	}
}

func TestPropagate_JoinsWithoutASpan(t *testing.T) {
	// With XBeego.Trace off an upstream traceparent still has to reach the logs and downstream.
	spans := recording(t)
	req := get("/x")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	var seen string
	w := serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Propagate()},
		func(w http.ResponseWriter, r *http.Request) {
			seen = trace.SpanContextFromContext(r.Context()).TraceID().String()
			w.WriteHeader(200)
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

func TestMetric_RecordsCountAndDuration(t *testing.T) {
	m := withMetrics(t)
	serve(t, get("/users/42"), []func(http.Handler) http.Handler{Capture(nil), Metric()},
		routed("/users/:id", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201) }))

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `http_requests_total{method="GET",route="/users/:id",status="201"} 1`) {
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
	for _, p := range []string{"/users/1", "/users/2", "/users/3"} {
		serve(t, get(p), []func(http.Handler) http.Handler{Capture(nil), Metric()},
			routed("/users/:id", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	}

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `route="/users/:id",status="200"} 3`) {
		t.Errorf("the three requests should fold into one series\ngot=\n%s", out)
	}
	if strings.Contains(out, `route="/users/1"`) {
		t.Errorf("the real path must not reach the labels\ngot=\n%s", out)
	}
}

func TestMetric_UnmatchedRouteUsesFixedValue(t *testing.T) {
	m := withMetrics(t)
	serve(t, get("/nope/12345"), []func(http.Handler) http.Handler{Capture(nil), Metric()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `route="unmatched"`) {
		t.Errorf("an unmatched route should be recorded as the fixed value\ngot=\n%s", out)
	}
}

func TestMetric_CustomMethodNormalized(t *testing.T) {
	// The route dimension already hides the ids behind templates; the method is copied straight
	// from the request, and anyone can set it to CUSTOM1, each new value adding a series that is
	// never evicted.
	m := withMetrics(t)
	serve(t, httptest.NewRequest("CUSTOM1", "/x", nil),
		[]func(http.Handler) http.Handler{Capture(nil), Metric()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `method="OTHER"`) {
		t.Errorf("a free-form method should be folded\ngot=\n%s", out)
	}
	if strings.Contains(out, "CUSTOM1") {
		t.Errorf("the original must not reach the labels\ngot=\n%s", out)
	}
}

func TestMetric_PanicStillCounted(t *testing.T) {
	// Without counting it, the requests that went wrong vanish from the error-rate metric.
	m := withMetrics(t)
	captureLog(t)
	serve(t, get("/x"), []func(http.Handler) http.Handler{Capture(nil), Metric(), Recover(nil)},
		func(http.ResponseWriter, *http.Request) { panic("boom") })

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `status="500"`) {
		t.Errorf("a panicking request should be counted as a 500\ngot=\n%s", out)
	}
}
