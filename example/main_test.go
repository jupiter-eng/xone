package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"time"

	"go.opentelemetry.io/otel"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
)

// This is the only test that runs the modules together: a unit test cannot prove the order of the
// stages, the correlation of a trace across modules, or whether shutdown really is in reverse.

// oneShot is a Runnable that returns once its work is done, so that Run goes through the whole
// shutdown without waiting for a signal
type oneShot struct{ work func(context.Context) error }

func (o *oneShot) Start(ctx context.Context) error { return o.work(ctx) }

// serveOnce starts a real xgin server, makes one request and stops it again.
//
// It complements the oneShot path: that one verifies the order of the stages and the correlation
// across modules, and this one verifies that "the few lines in main really do start a server".
type serveOnce struct {
	g    *xgin.XGin
	port int
	hit  func(base string)
}

func (s *serveOnce) Start(ctx context.Context) error {
	go func() {
		base := fmt.Sprintf("http://127.0.0.1:%d", s.port)
		for i := 0; i < 100; i++ {
			if resp, err := http.Get(base + "/metrics"); err == nil {
				resp.Body.Close()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		s.hit(base)
		s.g.Stop(context.Background())
	}()
	return s.g.Start(ctx)
}

func (s *serveOnce) Stop(ctx context.Context) error { return s.g.Stop(ctx) }

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "application.yml")
	os.WriteFile(cfg, []byte(`
XApp:
  Name: xone.demo.app
  Version: v0.1.0
XLog:
  Console: false
  File:
    Enable: true
    Path: `+dir+`
    Name: app.log
XTrace:
  SampleRatio: 1
XMetric:
  Namespace: demo
  ConstLabels:
    env: "${DEMO_ENV:dev}"
  GoMetrics: false
  ProcessMetrics: false
`), 0o644)

	// Each case starts from a clean configuration: what was loaded is cleared, this file is loaded,
	// and it is cleared again when the case ends. Each package's configuration is a package-level
	// variable of its own, and loading a configuration "merges" rather than "replaces" -- so what a
	// previous case left behind (ConstLabels, for one) would make a later assertion fail for no
	// apparent reason.
	// What Run finds already loaded is exactly the file WithConfigPath names, and it reuses it as
	// usual
	xonetest.UseConfig(t, cfg)

	// The framework's own logs are collected separately, to assert the order of initialization and
	// shutdown
	var framework strings.Builder
	fwLogger := slog.New(slog.NewTextHandler(&framework, nil))

	err := xone.Run(&oneShot{work: func(ctx context.Context) error {
		// The business code knows the native OpenTelemetry / slog / prometheus alone
		ctx, span := otel.Tracer("demo").Start(ctx, "do_work")
		slog.InfoContext(ctx, "working")
		span.End()

		xmetric.CounterInc("jobs", xmetric.T("result", "ok"))

		// Without one line of instrumentation, this error log becomes log_errors_total by itself
		slog.ErrorContext(ctx, "something went wrong")
		return nil
	}}, xone.WithConfigPath(cfg), xone.WithLogger(fwLogger))
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	t.Run("the order of the stages", func(t *testing.T) {
		// Logging has to come up first: otherwise the later components' initialization logs have
		// nowhere to go. Tracing and metrics have to come before the clients: otherwise the Spans the
		// clients emit attach to nothing and the points they record are never collected.
		// The relative order within one stage is the registration order (that is, the lexical order of
		// the import paths) and is not a promise of the framework, so the comparison here groups by
		// stage
		wantInit := []string{
			"xapp.loadConfig", "xlog.initXLog", // StageLog
			"xmetric.initXMetric", "xtrace.initXTrace", // StageTelemetry
			"xcache.initXCache", "xhttp.initXHttp", // StageClient
			"xgin.loadConfig", // StageServer
		}
		if got := extract(framework.String(), "starting"); !equal(got, wantInit) {
			t.Errorf("initialization order=%v want=%v", got, wantInit)
		}
	})

	t.Run("shutdown is the strict reverse", func(t *testing.T) {
		// Logging is closed last, so every component's shutdown log still gets written
		// xapp / xgin / xmetric have no resource to close and do not appear here
		wantStop := []string{
			"xhttp.closeXHttp", "xcache.closeXCache",
			"xtrace.closeXTrace",
			"xlog.closeXLog",
		}
		if got := extract(framework.String(), "stopping"); !equal(got, wantStop) {
			t.Errorf("shutdown order=%v want=%v", got, wantStop)
		}
	})

	t.Run("logs carry the trace identifiers by themselves", func(t *testing.T) {
		// xlog does not depend on OpenTelemetry; this rests entirely on the extension point xtrace
		// installs
		line := findLog(t, dir, "working")
		if line["trace_id"] == nil || line["span_id"] == nil {
			t.Errorf("a business log must carry trace_id / span_id, got=%v", line)
		}
	})

	t.Run("metrics carry the prefix, the constant labels and the application identity", func(t *testing.T) {
		// service_name / service_version come from the XApp block by themselves, named as the log fields are
		out := scrape(t)
		if !strings.Contains(out, `demo_jobs{env="dev",result="ok",service_name="xone.demo.app",service_version="v0.1.0"} 1`) {
			t.Errorf("the prefix, the constant labels, the environment variable default and the XApp identity must all take effect\ngot=\n%s", out)
		}
	})

	t.Run("error logs are counted by themselves", func(t *testing.T) {
		// This one spans three modules: xlog's observer -> xmetric's counter -> /metrics
		out := scrape(t)
		if !strings.Contains(out, `demo_log_errors_total{`) {
			t.Fatalf("log_errors_total must be there (the business wrote no instrumentation at all)\ngot=\n%s", out)
		}
		if !strings.Contains(out, `level="ERROR",service_name="xone.demo.app",service_version="v0.1.0"} 1`) {
			t.Errorf("that one Error must have been counted\ngot=\n%s", out)
		}
		// The trace identifiers serve as an exemplar, so a dashboard can jump from this point back to
		// the trace
		if !strings.Contains(out, "env=\"dev\"") {
			t.Errorf("the constant labels must come along too\ngot=\n%s", out)
		}
	})
}

// extract takes, in order, the component names involved in one action in the framework's logs
func extract(log, action string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "msg="+action) {
			continue
		}
		if _, after, ok := strings.Cut(line, "hook="); ok {
			out = append(out, strings.Fields(after)[0])
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// findLog finds the line with the given msg in the log file
func findLog(t *testing.T, dir, msg string) map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if len(files) == 0 {
		t.Fatal("no log file was written")
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		m := map[string]any{}
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			return m
		}
	}
	t.Fatalf("no msg=%q in the logs, contents=\n%s", msg, b)
	return nil
}

// scrape scrapes /metrics once
func scrape(t *testing.T) string {
	t.Helper()
	w := newRecorder()
	xmetric.Handler().ServeHTTP(w, newRequest())
	return w.body()
}

func newRecorder() *recorder { return &recorder{} }

type recorder struct {
	http.ResponseWriter
	buf    strings.Builder
	header http.Header
}

func (r *recorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}
func (r *recorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *recorder) WriteHeader(int)             {}
func (r *recorder) body() string                { return r.buf.String() }

func newRequest() *http.Request {
	req, _ := http.NewRequest("GET", "/metrics", nil)
	return req
}

// TestServe runs the few lines of main as they are: start the server, make a request, shut down
// gracefully
func TestServe(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	cfg := filepath.Join(dir, "application.yml")
	// The port goes through ${VAR} rather than being written down: this path has to reach all the way
	// to xgin's int field. Without re-deciding the scalar type after the placeholder is expanded, this
	// fails startup with "cannot unmarshal !!str into int" -- which is exactly what it used to do
	t.Setenv("XONE_EXAMPLE_PORT", strconv.Itoa(port))
	os.WriteFile(cfg, []byte(fmt.Sprintf(`
XApp:
  Name: xone.demo.app
XLog:
  Console: false
  File: {Enable: true, Path: %s, Name: app.log}
XMetric:
  Namespace: demo
  GoMetrics: false
  ProcessMetrics: false
XGin:
  Host: 127.0.0.1
  Port: ${XONE_EXAMPLE_PORT}
`, dir)), 0o644)
	xonetest.UseConfig(t, cfg) // as in TestEndToEnd

	var body, metrics string
	var traceID string
	srv := &serveOnce{
		g:    xgin.New().WithRoutes(routes),
		port: port,
		hit: func(base string) {
			resp, err := http.Get(base + "/hello")
			if err != nil {
				t.Errorf("the request failed: %v", err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			traceID = resp.Header.Get("X-Trace-Id")

			// An endpoint that panics is called deliberately, to confirm the recover middleware caught it
			if r, err := http.Get(base + "/boom"); err == nil {
				if r.StatusCode != 500 {
					t.Errorf("a panic must be caught as a 500, got=%d", r.StatusCode)
				}
				r.Body.Close()
			}

			if r, err := http.Get(base + "/metrics"); err == nil {
				m, _ := io.ReadAll(r.Body)
				r.Body.Close()
				metrics = string(m)
			}
		},
	}

	if err := xone.Run(srv, xone.WithConfigPath(cfg), xone.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	t.Run("the endpoint answers", func(t *testing.T) {
		if !strings.Contains(body, "hello") {
			t.Errorf("the wrong response, got=%q", body)
		}
	})

	t.Run("the response carries the TraceID back", func(t *testing.T) {
		// Jumping from one call straight to the trace rests on this header
		if traceID == "" {
			t.Error("X-Trace-Id must be carried back in the response headers")
		}
	})

	t.Run("logs carry the trace identifiers", func(t *testing.T) {
		line := findLog(t, dir, "request received")
		if line["trace_id"] != traceID {
			t.Errorf("a business log's trace_id must match the response header, log=%v header=%s", line["trace_id"], traceID)
		}
	})

	t.Run("the access log is written by itself", func(t *testing.T) {
		// The business code wrote not one line; the access log is written by the built-in middleware
		line := findLog(t, dir, "request completed")
		if line["route"] != "/hello" || line["status"] != float64(200) {
			t.Errorf("the wrong access log, got=%v", line)
		}
	})

	t.Run("request metrics are collected by themselves", func(t *testing.T) {
		if !strings.Contains(metrics, `demo_http_requests_total{method="GET",route="/hello",service_name="xone.demo.app",status="200"} 1`) {
			t.Errorf("request metrics must be collected by themselves\ngot=\n%s", metrics)
		}
		// The route is the template rather than the real path, or the time series would grow without
		// bound with the ids in the URLs
		if !strings.Contains(metrics, `demo_http_requests_total{method="GET",route="/boom",service_name="xone.demo.app",status="500"}`) {
			t.Errorf("a request that panicked must be counted as a 500 too\ngot=\n%s", metrics)
		}
	})

	t.Run("a panic is counted into the error log metric", func(t *testing.T) {
		// This spans three modules: xgin's recover -> xlog's observer -> xmetric's counter
		if !strings.Contains(metrics, "demo_log_errors_total") {
			t.Errorf("a panic must be counted into the error log metric\ngot=\n%s", metrics)
		}
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
