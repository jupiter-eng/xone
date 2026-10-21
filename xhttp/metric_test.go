package xhttp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xmetric"
)

// withMetrics installs a metrics setup of its own and makes this package use it.
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

func TestMetric_RecordsStatusAndDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	client, m := newQuiet(t, DefaultConfig())
	if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `status="503"`) || !strings.Contains(out, `method="GET"`) {
		t.Errorf("the labels must be by method and status code\ngot=\n%s", out)
	}
	if !strings.Contains(out, "http_client_request_duration_seconds_count") {
		t.Errorf("the duration must be recorded\ngot=\n%s", out)
	}
}

func TestMetric_NetworkErrorRecordsStatus0(t *testing.T) {
	// Mixing it in with the real status codes would make an alert such as "the 5xx ratio" show nothing during a network failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	client, m := newQuiet(t, DefaultConfig())
	client.R().SetContext(context.Background()).Get(srv.URL)

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `status="0"`) {
		t.Errorf("a network error must record status code 0\ngot=\n%s", out)
	}
}

func TestMetric_RetriesRecordedOnce(t *testing.T) {
	// Attached as middleware it would record every retry and inflate both the request count and the duration distribution.
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
	client, m := newQuiet(t, c)
	client.R().SetContext(context.Background()).Get(srv.URL)

	if got := hits.Load(); got != 3 {
		t.Fatalf("it must really have retried, got=%d requests", got)
	}
	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `status="0"} 1`) {
		t.Errorf("3 retries must still record only 1 sample\ngot=\n%s", out)
	}
}

func TestMetric_UsesXMetricBucketsAndLabels(t *testing.T) {
	// Outbound and inbound durations have to be on the same scale for the dashboards to line up.
	m, closer, err := xmetric.New(func() xmetric.Config {
		c := xmetric.DefaultConfig()
		c.GoMetrics, c.ProcessMetrics, c.LogErrorMetric = false, false, false
		c.Namespace = "demo"
		c.ConstLabels = map[string]string{"env": "prod"}
		c.HTTPDurationBuckets = []float64{0.5}
		return c
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	srv, _ := echo(t, nil)
	client, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	client.SetLogger(discardLogger{})
	client.R().SetContext(context.Background()).Get(srv.URL)

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `demo_http_client_request_duration_seconds_bucket{env="prod"`) {
		t.Errorf("the prefix and the constant labels must be carried\ngot=\n%s", out)
	}
	if strings.Contains(out, `le="0.25"`) {
		t.Errorf("the buckets from the configuration must be used\ngot=\n%s", out)
	}
}

func TestMetric_DuplicateRegisterReusesExisting(t *testing.T) {
	// With two clients built, the second one's metrics have to land on the registered one, or what it records is never exported.
	m := withMetrics(t)
	srv, _ := echo(t, nil)

	for i := 0; i < 2; i++ {
		client, closer, err := New(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer closer.Close()
		client.SetLogger(discardLogger{})
		if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
			t.Fatal(err)
		}
	}

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `status="204"} 2`) {
		t.Errorf("the requests of both clients must be recorded on the same series\ngot=\n%s", out)
	}
}

func TestNew_MetricRegisterFailureOnlyLogs_ClientStillUsable(t *testing.T) {
	// Metrics not being exported is an observability problem, as in xgin / xgorm / xredis:
	// it must not keep every outbound call from working.
	m := withMetrics(t)
	m.Registry.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "http_client_request_duration_seconds", Help: "placeholder",
	}))
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	client, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New must not fail when the metric name is taken by another type: %v", err)
	}
	defer closer.Close()
	client.SetLogger(discardLogger{})
	srv, _ := echo(t, nil)
	if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
		t.Fatalf("the client must still work after a failed registration: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "xhttp failed to register the request duration metric") || !strings.Contains(got, `"level":"ERROR"`) {
		t.Errorf("a failed registration must log one error, got=%s", got)
	}
}

func TestMetric_MethodLabelCollapsedToFixedSet(t *testing.T) {
	// A method is a free token: copied into a label, CUSTOM1 and CUSTOM2 would each be a set of time series.
	srv, _ := echo(t, nil)
	client, m := newQuiet(t, DefaultConfig())
	for _, method := range []string{"CUSTOM1", "CUSTOM2", "get", http.MethodPatch} {
		if _, err := client.R().SetContext(context.Background()).Execute(method, srv.URL); err != nil {
			t.Fatal(err)
		}
	}

	out := testkit.Scrape(m.Handler)
	for _, bad := range []string{`method="CUSTOM1"`, `method="CUSTOM2"`, `method="get"`} {
		if strings.Contains(out, bad) {
			t.Errorf("an unrecognized method must not reach the label as it is: %s\ngot=\n%s", bad, out)
		}
	}
	if !strings.Contains(out, `method="OTHER"`) || !strings.Contains(out, `method="PATCH"`) {
		t.Errorf("unrecognized ones become OTHER and recognized ones are kept\ngot=\n%s", out)
	}
}

func TestNormalizeMethod(t *testing.T) {
	for in, want := range map[string]string{
		"GET": "GET", "POST": "POST", "PATCH": "PATCH", "CONNECT": "CONNECT",
		"get": methodOther, "CUSTOM": methodOther, "": methodOther,
	} {
		if got := normalizeMethod(in); got != want {
			t.Errorf("normalizeMethod(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestElapsed_FallsBackToAttemptDurationWithoutStart(t *testing.T) {
	// The start is written into the context before the first attempt: not finding it means this path did not go
	// through that middleware, and reporting this attempt's duration then beats reporting 0.
	want := 3 * time.Second
	if got := elapsed(nil, want); got != want {
		t.Errorf("with req nil it must fall back, got=%v", got)
	}

	req := resty.New().R()
	if got := elapsed(req, want); got != want {
		t.Errorf("without a start in the context it must fall back, got=%v", got)
	}
}

func TestElapsed_WithStartCoversWholeLogicalRequest(t *testing.T) {
	// With three retries, what the caller waited is the sum of every attempt;
	// reporting only the last one would make the P99 look far better than it is.
	start := time.Now().Add(-2 * time.Second)
	req := resty.New().R().SetContext(context.WithValue(context.Background(), startKey{}, start))

	got := elapsed(req, time.Millisecond)
	if got < 2*time.Second {
		t.Errorf("it must be computed from the start, got=%v", got)
	}
}
