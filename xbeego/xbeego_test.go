package xbeego

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	beeweb "github.com/beego/beego/v2/server/web"
	beectx "github.com/beego/beego/v2/server/web/context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xbeego/middleware"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xmetric"
)

// ---- The fixture ----

// abortController covers beego's own way of stopping a handler: CustomAbort writes the response
// and then panics with web.ErrAbort, which must not be taken for a failure.
type abortController struct{ beeweb.Controller }

func (c *abortController) Get() { c.CustomAbort(http.StatusUnauthorized, "nope") }

// teapotController is how a beego application answers an error code it registered a handler for:
// Controller.Abort panics with the code as a string, for the recovery to render that handler.
type teapotController struct{ beeweb.Controller }

func (c *teapotController) Get() { c.Abort("418") }

// userController is a beego controller written the ordinary way: it logs with the request's ctx
// and adds a per-request field from deep inside business code.
type userController struct{ beeweb.Controller }

func (c *userController) Get() {
	ctx := c.Ctx.Request.Context()
	xlog.AddKV(ctx, "tenant", "acme")
	slog.InfoContext(ctx, "get user", "user_id", c.Ctx.Input.Param(":id"))
	c.Data["json"] = map[string]string{"id": c.Ctx.Input.Param(":id")}
	_ = c.ServeJSON()
}

// TestMain registers the routes once for the whole package.
//
// Beego has a single global router and registering a pattern twice panics, so this cannot live in
// the tests themselves -- which is itself the reason xbeego binds to that global, see the package
// comment.
// beegoOwnRecover is beego's own recovery, taken before any assembly has replaced it, so a test
// covering "the framework did not take it over" can put the real thing back.
var beegoOwnRecover func(*beectx.Context, *beeweb.Config)

func TestMain(m *testing.M) {
	beegoOwnRecover = beeweb.BConfig.RecoverFunc
	beeweb.Get("/ping", func(ctx *beectx.Context) { _ = ctx.Output.Body([]byte("pong")) })
	beeweb.Get("/users/:id", func(ctx *beectx.Context) {
		_ = ctx.Output.Body([]byte("user " + ctx.Input.Param(":id")))
	})
	beeweb.Post("/login", func(ctx *beectx.Context) {
		body, _ := io.ReadAll(ctx.Request.Body)
		ctx.Output.SetStatus(http.StatusCreated)
		_ = ctx.Output.Body(append([]byte("got "), body...))
	})
	beeweb.Get("/boom", func(*beectx.Context) { panic("boom with password=s3cret") })
	beeweb.Get("/drop", func(*beectx.Context) { panic(http.ErrAbortHandler) })
	beeweb.Get("/client-ip", func(ctx *beectx.Context) {
		_ = ctx.Output.Body([]byte(ClientIP(ctx.Request)))
	})
	beeweb.Get("/trace-id", func(ctx *beectx.Context) {
		_ = ctx.Output.Body([]byte(trace.SpanContextFromContext(ctx.Request.Context()).TraceID().String()))
	})
	beeweb.Get("/slow", func(ctx *beectx.Context) {
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Request.Context().Done():
		}
		_ = ctx.Output.Body([]byte("slow"))
	})
	beeweb.Router("/abort", &abortController{})

	// An error handler registered the way an application registers its own. 418 rather than a code
	// the framework or beego answers by itself, so that no other test's expectations move.
	beeweb.ErrorHandler("418", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("i am a teapot"))
	})
	beeweb.Router("/teapot", &teapotController{})

	// Context.Abort always panics with the body, registered or not, having set the status first.
	beeweb.Get("/ctx-abort", func(ctx *beectx.Context) { ctx.Abort(http.StatusForbidden, "nope") })

	// A controller, which is how a beego application is really written: it reaches the request ctx
	// through c.Ctx.Request.Context().
	beeweb.Router("/users-ctl/:id", &userController{})

	// Beego copies the request body into ctx.Input.RequestBody only with this on, which is what an
	// API service turns on in conf/app.conf. The access log reads the body from the http.Request
	// instead, and the two must not take it from each other.
	beeweb.BConfig.CopyRequestBody = true
	beeweb.Post("/echo-body", func(ctx *beectx.Context) {
		_ = ctx.Output.Body(ctx.Input.RequestBody)
	})

	// A filter that answers before the router runs while letting the chain carry on, which is what
	// WithReturnOnOutput(false) means. xbeego's own filter has to survive it, see
	// TestAccessLog_RouteSurvivesAnEarlierFilterThatWrote.
	beeweb.Get("/started", func(ctx *beectx.Context) { _ = ctx.Output.Body([]byte("late")) })
	beeweb.InsertFilter("/started", beeweb.BeforeRouter, func(ctx *beectx.Context) {
		ctx.ResponseWriter.WriteHeader(http.StatusAccepted)
	}, beeweb.WithReturnOnOutput(false))

	// A filter chain, which only runs once beego's Handlers.Init has folded it into the chain root.
	beeweb.Get("/chained", func(ctx *beectx.Context) { _ = ctx.Output.Body([]byte("handler")) })
	beeweb.InsertFilterChain("/chained", func(next beeweb.FilterFunc) beeweb.FilterFunc {
		return func(ctx *beectx.Context) {
			ctx.Output.Header("X-Chain", "ran")
			next(ctx)
		}
	})

	os.Exit(m.Run())
}

// ---- Helpers ----

func load(t *testing.T, yml string) Config {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)
	if err := config.LoadInto(path, ConfigKey, &c); err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	return c
}

func loadErr(t *testing.T, yml string) error {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.Reset)
	return config.LoadInto(path, ConfigKey, &c)
}

// configWith changes a few fields on top of the defaults.
//
// Tests always hand the config in through WithConfig and never read the config file, so an
// XONE_CONFIG set on the machine running the tests makes no difference.
func configWith(mutate ...func(*Config)) Config {
	c := DefaultConfig()
	for _, m := range mutate {
		m(&c)
	}
	return c
}

// quiet turns off the access log and the metrics: when they are not what is being tested, this
// keeps the log from flooding and the metrics endpoint from taking a path.
func quiet(c *Config) { c.Log, c.Metric, c.Trace = false, false, false }

// on listens on this port, on localhost.
func on(port int) func(*Config) {
	return func(c *Config) { c.Host, c.Port = "127.0.0.1", port }
}

// do sends one request through the whole chain and returns what the client would have received.
func do(t *testing.T, x *XBeego, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	x.Handler().ServeHTTP(w, httptest.NewRequest(method, target, body))
	return w
}

// doFrom is do with the directly connected peer and the headers set.
func doFrom(t *testing.T, x *XBeego, target, remote string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	x.Handler().ServeHTTP(w, req)
	return w
}

// captureLog replaces slog's default output with JSON written into a buffer, and restores it when
// the test ends. lines() returns one map per record written so far.
func captureLog(t *testing.T) func() []map[string]any {
	t.Helper()
	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		buf.Write(p)
		return len(p), nil
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("the log line is not JSON: %v\n%s", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// only is the one record whose msg matches, or a failure when there is not exactly one.
func only(t *testing.T, lines []map[string]any, msg string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, l := range lines {
		if l["msg"] == msg {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("exactly one %q record was expected, got=%d\n%v", msg, len(found), lines)
	}
	return found[0]
}

// access is the one access log record among the lines.
func access(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	return only(t, lines, "request completed")
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

// attrsOf flattens a span's attributes into a map.
func attrsOf(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// serving starts the server, waits until it really listens, and stops it when the test ends.
func serving(t *testing.T, x *XBeego, port int) string {
	t.Helper()
	go func() { _ = x.Start(context.Background()) }()
	t.Cleanup(func() { _ = x.Stop(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/ping")
		if err == nil {
			resp.Body.Close()
			return base
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the server did not start listening on %s", base)
	return ""
}

// ---- Configuration ----

func TestConfig_Defaults(t *testing.T) {
	c := DefaultConfig()
	if c.Host != "0.0.0.0" || c.Port != 8080 {
		t.Errorf("the listen defaults are wrong, got=%+v", c)
	}
	// Without a limit, a slow client can hold a connection indefinitely.
	if c.ReadHeaderTimeout != 10*time.Second || c.IdleTimeout != 60*time.Second {
		t.Errorf("reading headers and idle connections must have timeouts, got=%v %v", c.ReadHeaderTimeout, c.IdleTimeout)
	}
	if c.ReadTimeout != 0 || c.WriteTimeout != 0 {
		t.Errorf("read and write timeouts are unlimited by default, got=%v %v", c.ReadTimeout, c.WriteTimeout)
	}
	if !c.Log || !c.Trace || !c.Metric {
		t.Errorf("the three built-in middlewares should all be on by default, got=%+v", c)
	}
	if c.LogRequestBody || c.LogResponseBody || c.LogQuery || c.LogRequestHeaders || c.LogResponseHeaders || c.UseH2C {
		t.Errorf("recording bodies, query strings, request and response headers, and h2c should all be off by default, got=%+v", c)
	}
	// Beego's own recovery writes plain text with no trace_id, and its logs package writes plain
	// text to the console: both are taken over unless asked otherwise.
	if !c.RecoverPanic || !c.BeegoLogs {
		t.Errorf("beego's recovery and its logs should be taken over by default, got=%+v", c)
	}
	if c.MetricPath != "/metrics" || c.TLS.MinVersion != "1.2" {
		t.Errorf("the metrics path should default to /metrics and MinVersion to 1.2, got=%+v", c)
	}
	if len(c.LogSkipPaths) != 0 {
		t.Errorf("no path is skipped by default, got=%v", c.LogSkipPaths)
	}
	if !reflect.DeepEqual(c.TrustedProxies, []string{"private"}) {
		t.Errorf("only private ranges are trusted by default, got=%v", c.TrustedProxies)
	}
}

func TestConfig_LoadFromFile(t *testing.T) {
	c := load(t, "XBeego:\n  Port: 9090\n  ReadTimeout: 30s\n  UseH2C: true\n  Log: false\n  BeegoLogs: false\n  LogSkipPaths: [/healthz, /static/]\n")
	if c.Port != 9090 || c.ReadTimeout != 30*time.Second || !c.UseH2C {
		t.Errorf("the config did not take effect, got=%+v", c)
	}
	if c.Log || c.BeegoLogs || strings.Join(c.LogSkipPaths, " ") != "/healthz /static/" {
		t.Errorf("the middleware switches are in the config too, got=%+v", c)
	}
	if c.Host != "0.0.0.0" || !c.Trace || c.MetricPath != "/metrics" || !c.RecoverPanic {
		t.Errorf("fields left out should keep their defaults, got=%+v", c)
	}
}

func TestConfig_FailsFastOnTypo(t *testing.T) {
	if err := loadErr(t, "XBeego:\n  Prot: 9090\n"); err == nil {
		t.Fatal("a misspelled field should fail startup")
	}
	// The fields of the other web integrations do not exist here: copying an XGin or XEcho block
	// over has to be reported on the spot rather than silently doing nothing.
	for _, f := range []string{"Mode: release", "MaxMultipartMemory: 1024", "ZHTranslations: true", "Debug: true"} {
		if err := loadErr(t, "XBeego:\n  "+f+"\n"); err == nil {
			t.Errorf("%s is not an XBeego field and should fail startup", f)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("the default config should be valid: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"port 0":                          func(c *Config) { c.Port = 0 },
		"port out of range":               func(c *Config) { c.Port = 70000 },
		"certificate only":                func(c *Config) { c.TLS.CertFile = "a.pem" },
		"key only":                        func(c *Config) { c.TLS.KeyFile = "a.key" },
		"client CA without a certificate": func(c *Config) { c.TLS.ClientCAFile = "ca.pem" },
		"unknown TLS version": func(c *Config) {
			c.TLS.CertFile, c.TLS.KeyFile, c.TLS.MinVersion = "a.pem", "a.key", "1.1"
		},
		"ReadHeaderTimeout 0":            func(c *Config) { c.ReadHeaderTimeout = 0 },
		"IdleTimeout 0":                  func(c *Config) { c.IdleTimeout = 0 },
		"negative ReadTimeout":           func(c *Config) { c.ReadTimeout = -time.Second },
		"malformed proxy range":          func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8/8"} },
		"IPv4-mapped proxy":              func(c *Config) { c.TrustedProxies = []string{"::ffff:10.0.0.1"} },
		"MetricPath without a leading /": func(c *Config) { c.MetricPath = "metrics" },
		"MetricPath empty":               func(c *Config) { c.MetricPath = "" },
	} {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}

	// With metrics off MetricPath is unused and must not block anything.
	c := DefaultConfig()
	c.Metric, c.MetricPath = false, ""
	if err := c.Validate(); err != nil {
		t.Errorf("MetricPath should not be checked with metrics off: %v", err)
	}
}

func TestValidate_MessagesMatchTheOtherIntegrations(t *testing.T) {
	// The rules live in internal/web so that the three web integrations report them identically; a
	// copy here would drift.
	c := DefaultConfig()
	c.Port = 0
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "Port must be within 1..65535") {
		t.Errorf("the port message should be the shared one, got=%v", err)
	}
}

// ---- Assembly ----

func TestHandler_ServesTheGlobalBeegoRouter(t *testing.T) {
	// The point of binding to web.BeeApp: routes an application registered from its own routers
	// package are served without being handed to xbeego at all.
	x := New().WithConfig(configWith(quiet))
	if w := do(t, x, "GET", "/ping", nil); w.Code != 200 || w.Body.String() != "pong" {
		t.Errorf("the globally registered route should be served, got=%d %q", w.Code, w.Body.String())
	}
}

func TestHandler_AssemblesOnce(t *testing.T) {
	// Assembling twice would mount the middleware twice: two access log lines and doubled metrics
	// per request.
	var built, seen int
	x := New().
		WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).
		WithMiddleware(func(next http.Handler) http.Handler {
			built++
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen++
				next.ServeHTTP(w, r)
			})
		})

	lines := captureLog(t)
	a, b := x.Handler(), x.Handler()
	if reflect.ValueOf(a).Pointer() != reflect.ValueOf(b).Pointer() {
		t.Error("Handler should assemble once and hand out the same chain")
	}
	if built != 1 {
		t.Errorf("the middleware should have been mounted once, got=%d", built)
	}

	do(t, x, "GET", "/ping", nil)
	if seen != 1 {
		t.Errorf("each layer should run once per request, got=%d", seen)
	}
	// access fails unless there is exactly one: a chain assembled twice writes two.
	access(t, lines())

	// Settings after assembly have no effect, rather than quietly applying to nothing.
	x.WithMiddleware(func(next http.Handler) http.Handler { return next })
	if w := do(t, x, "GET", "/ping", nil); w.Code != 200 {
		t.Errorf("the chain should keep working, got=%d", w.Code)
	}
	if built != 1 {
		t.Errorf("a middleware added after assembly must not be mounted, got=%d", built)
	}
}

func TestHandler_InvalidConfigAssemblesWithDefaults(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.Port = 0 }))
	if x.Handler() == nil {
		t.Fatal("a caller of Handler() has to get a chain even when the config is invalid")
	}
	// The error itself is Start's to report.
	err := x.Start(context.Background())
	if err == nil {
		t.Fatal("Start should return the config error and never listen")
	}
	if !xerror.Is(err, "xbeego") {
		t.Errorf("the error should be reported under this module, got=%v", err)
	}
}

func TestApp_ReturnsTheGlobalServer(t *testing.T) {
	x := New().WithConfig(configWith(quiet))
	if x.App() != beeweb.BeeApp {
		t.Error("App should hand out beego's own global server")
	}
}

func TestWithRoutes_RunsDuringAssembly(t *testing.T) {
	var got *beeweb.HttpServer
	x := New().WithConfig(configWith(quiet)).WithRoutes(func(app *beeweb.HttpServer) { got = app })
	x.Handler()
	if got != beeweb.BeeApp {
		t.Error("the callback should run during assembly and receive web.BeeApp")
	}
}

func TestWithMiddleware_RunsOutsideBeegoAndCoversMetrics(t *testing.T) {
	// An endpoint a caller believes to be behind their shared authentication must not be the one
	// exception, which is why the metrics path sits inside the caller's middleware.
	var seen []string
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		})
	}
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace = false, false })).WithMiddleware(deny)

	if w := do(t, x, "GET", "/ping", nil); w.Code != http.StatusForbidden {
		t.Errorf("the caller's middleware should run, got=%d", w.Code)
	}
	if w := do(t, x, "GET", "/metrics", nil); w.Code != http.StatusForbidden {
		t.Errorf("the metrics endpoint should go through the caller's middleware too, got=%d", w.Code)
	}
	if strings.Join(seen, " ") != "/ping /metrics" {
		t.Errorf("both requests should have reached the middleware, got=%v", seen)
	}
}

// ---- Access log ----

func TestAccessLog_RouteIsTheTemplate(t *testing.T) {
	// The headline of this module: without the FinishRouter filter the route is unknown outside
	// beego, and every id in a URL would become a route of its own in the logs and the metrics.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	do(t, x, "GET", "/users/42", nil)

	a := access(t, lines())
	if a["route"] != "/users/:id" {
		t.Errorf("route should be the template, got=%v", a["route"])
	}
	if a["path"] != "/users/42" {
		t.Errorf("the real path belongs in path, got=%v", a["path"])
	}
	if a["method"] != "GET" || a["status"] != float64(200) {
		t.Errorf("method and status are wrong, got=%v", a)
	}
	if a["bytes_out"] != float64(len("user 42")) {
		t.Errorf("bytes_out should count the response body, got=%v", a["bytes_out"])
	}
	if _, ok := a["elapsed_ms"]; !ok {
		t.Errorf("elapsed_ms should be recorded, got=%v", a)
	}
}

func TestAccessLog_UnmatchedRouteUsesFixedValue(t *testing.T) {
	// Nothing matched, so beego never reached FinishRouter and the template does not exist. A
	// fixed value keeps the series count bounded for a scanner walking random paths.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	do(t, x, "GET", "/nope/12345", nil)

	a := access(t, lines())
	if a["route"] != "unmatched" {
		t.Errorf("an unmatched route should be recorded as the fixed value, got=%v", a["route"])
	}
	if a["status"] != float64(404) {
		t.Errorf("the status should still be the real one, got=%v", a["status"])
	}
}

func TestAccessLog_StatusAndBytesComeFromTheRecorder(t *testing.T) {
	// net/http tells nobody what went out, so the recorder in front of the writer is the only
	// source for these two.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	do(t, x, "POST", "/login", strings.NewReader("hi"))

	a := access(t, lines())
	if a["status"] != float64(201) {
		t.Errorf("the status the handler set should be recorded, got=%v", a["status"])
	}
	if a["bytes_out"] != float64(len("got hi")) {
		t.Errorf("bytes_out is wrong, got=%v", a["bytes_out"])
	}
}

func TestAccessLog_SkipPaths(t *testing.T) {
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogSkipPaths = []string{"/ping"}
	}))
	do(t, x, "GET", "/ping", nil)

	for _, l := range lines() {
		if l["msg"] == "request completed" {
			t.Errorf("a skipped path should write no access log, got=%v", l)
		}
	}
}

func TestAccessLog_MetricPathSkippedWithoutBeingListed(t *testing.T) {
	// The scraper polls it every few seconds; logging it is pure noise.
	withMetrics(t)
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Trace = false }))
	do(t, x, "GET", "/metrics", nil)

	for _, l := range lines() {
		if l["msg"] == "request completed" {
			t.Errorf("the metrics endpoint should not be access-logged, got=%v", l)
		}
	}
}

func TestAccessLog_RedactsTheRequestBody(t *testing.T) {
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogRequestBody = true
	}))
	w := do(t, x, "POST", "/login", strings.NewReader(`{"user":"a","password":"s3cret"}`))

	a := access(t, lines())
	body, _ := a["request_body"].(string)
	if strings.Contains(body, "s3cret") {
		t.Errorf("the password must not reach the log, got=%v", body)
	}
	if !strings.Contains(body, middleware.Redacted) {
		t.Errorf("the masked field should be visible as such, got=%v", body)
	}
	// Reading the body for the log must not take it away from the handler.
	if !strings.Contains(w.Body.String(), "s3cret") {
		t.Errorf("the handler should still receive the whole body, got=%q", w.Body.String())
	}
}

func TestAccessLog_QueryAndHeadersAreOptionalAndMasked(t *testing.T) {
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogQuery, c.LogRequestHeaders = true, true
	}))
	doFrom(t, x, "/ping?token=abc123&page=2", "192.0.2.1:1234", map[string]string{"Authorization": "Bearer xyz"})

	a := access(t, lines())
	q, _ := a["query"].(string)
	if strings.Contains(q, "abc123") || !strings.Contains(q, "page=2") {
		t.Errorf("the token should be masked and the rest kept, got=%v", q)
	}
	headers, _ := a["request_headers"].(map[string]any)
	if auth, _ := headers["Authorization"].(string); auth != middleware.Redacted {
		t.Errorf("Authorization should be masked, got=%v", headers)
	}
}

func TestAccessLog_ClientIPIgnoresForgedHeadersFromAnUntrustedPeer(t *testing.T) {
	// Beego's own ctx.Input.IP() would take 1.2.3.4 here, which is exactly the forgery.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	doFrom(t, x, "/ping", "203.0.113.9:4321", map[string]string{"X-Forwarded-For": "1.2.3.4"})

	if a := access(t, lines()); a["client_ip"] != "203.0.113.9" {
		t.Errorf("an untrusted peer's X-Forwarded-For must be ignored, got=%v", a["client_ip"])
	}
}

func TestAccessLog_ClientIPFollowsATrustedProxy(t *testing.T) {
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	// 10.0.0.1 is inside the default private ranges.
	doFrom(t, x, "/ping", "10.0.0.1:4321", map[string]string{"X-Forwarded-For": "1.2.3.4"})

	if a := access(t, lines()); a["client_ip"] != "1.2.3.4" {
		t.Errorf("a trusted proxy's X-Forwarded-For should be followed, got=%v", a["client_ip"])
	}
}

func TestClientIP_IsWhatTheHandlerSeesToo(t *testing.T) {
	// A controller has to be able to read the same value the log went by, rather than beego's
	// forgeable one.
	x := New().WithConfig(configWith(quiet))
	w := doFrom(t, x, "/client-ip", "10.0.0.1:4321", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if w.Body.String() != "1.2.3.4" {
		t.Errorf("xbeego.ClientIP should give the handler the framework's answer, got=%q", w.Body.String())
	}

	w = doFrom(t, x, "/client-ip", "203.0.113.9:4321", map[string]string{"X-Forwarded-For": "1.2.3.4"})
	if w.Body.String() != "203.0.113.9" {
		t.Errorf("from an untrusted peer it is the peer itself, got=%q", w.Body.String())
	}
}

func TestClientIP_FallsBackToThePeerWithoutTheMiddleware(t *testing.T) {
	// Called outside a request the framework handled, it must not panic and must not invent an
	// address it cannot verify.
	req := httptest.NewRequest("GET", "/ping", nil)
	req.RemoteAddr = "203.0.113.9:4321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(req); got != "203.0.113.9" {
		t.Errorf("without the middleware it is the peer alone, got=%q", got)
	}
}

// ---- Metrics ----

func TestMetrics_EndpointIsServedWithoutARoute(t *testing.T) {
	// No beego controller is written for it: it is answered before the router.
	m := withMetrics(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace = false, false }))
	do(t, x, "GET", "/users/7", nil)

	w := do(t, x, "GET", "/metrics", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http_requests_total") {
		t.Errorf("the metrics endpoint should answer, got=%d\n%s", w.Code, w.Body.String())
	}
	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `route="/users/:id"`) {
		t.Errorf("the route label should be the template\ngot=\n%s", out)
	}
}

func TestMetrics_RouteLabelFoldsTheIdsAway(t *testing.T) {
	m := withMetrics(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace = false, false }))
	for _, p := range []string{"/users/1", "/users/2", "/users/3"} {
		do(t, x, "GET", p, nil)
	}

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `route="/users/:id",status="200"} 3`) {
		t.Errorf("the three requests should fold into one series\ngot=\n%s", out)
	}
	if strings.Contains(out, `route="/users/1"`) {
		t.Errorf("the real path must not reach the labels\ngot=\n%s", out)
	}
}

func TestMetrics_CustomPath(t *testing.T) {
	withMetrics(t)
	x := New().WithConfig(configWith(func(c *Config) {
		c.Log, c.Trace = false, false
		c.MetricPath = "/internal/metrics"
	}))
	if w := do(t, x, "GET", "/internal/metrics", nil); w.Code != 200 {
		t.Errorf("the configured path should answer, got=%d", w.Code)
	}
	// /metrics is then an ordinary unmatched path.
	if w := do(t, x, "GET", "/metrics", nil); w.Code != 404 {
		t.Errorf("the default path should no longer answer, got=%d", w.Code)
	}
}

func TestMetrics_Off(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false }))
	if w := do(t, x, "GET", "/metrics", nil); w.Code != 404 {
		t.Errorf("with metrics off the endpoint should not exist, got=%d", w.Code)
	}
}

// ---- Tracing ----

func TestTrace_SpanNameUsesTheRouteTemplate(t *testing.T) {
	// The template is only known once beego has routed, which is after this layer started the
	// span: the name is set on the way out.
	spans := recording(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Metric = false, false }))
	w := do(t, x, "GET", "/users/42", nil)

	if w.Header().Get(middleware.TraceIDHeader) == "" {
		t.Error("the response should carry the trace identifier back")
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
}

func TestTrace_HandlerSeesTheSpan(t *testing.T) {
	// The ctx carrying the span has to reach the beego controller, or nothing a handler logs or
	// calls downstream joins the trace.
	recording(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Metric = false, false }))
	w := do(t, x, "GET", "/trace-id", nil)

	if w.Body.String() == "" || w.Body.String() == "00000000000000000000000000000000" {
		t.Errorf("the handler's ctx should carry the span, got=%q", w.Body.String())
	}
	if w.Body.String() != w.Header().Get(middleware.TraceIDHeader) {
		t.Errorf("the handler and the response header should agree, got=%q and %q",
			w.Body.String(), w.Header().Get(middleware.TraceIDHeader))
	}
}

func TestTrace_ContinuesUpstreamTrace(t *testing.T) {
	recording(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Metric = false, false }))
	w := doFrom(t, x, "/trace-id", "10.0.0.1:1234", map[string]string{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	})
	if w.Body.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("the upstream trace should be continued, got=%q", w.Body.String())
	}
}

func TestTrace_MetricsEndpointOpensNoSpan(t *testing.T) {
	withMetrics(t)
	spans := recording(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log = false }))
	do(t, x, "GET", "/metrics", nil)

	if n := len(spans()); n != 0 {
		t.Errorf("a span per scrape only floods the tracing backend, got=%d", n)
	}
}

func TestTrace_Off(t *testing.T) {
	spans := recording(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Metric, c.Trace = false, false, false }))
	w := doFrom(t, x, "/trace-id", "10.0.0.1:1234", map[string]string{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	})

	if n := len(spans()); n != 0 {
		t.Errorf("with tracing off no span is opened, got=%d", n)
	}
	// Propagate still runs: the upstream identifiers reach the handler and travel downstream.
	if w.Body.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("the upstream trace identifiers should still be accepted, got=%q", w.Body.String())
	}
	if w.Header().Get(middleware.TraceIDHeader) != "" {
		t.Error("with no span of its own there is no X-Trace-Id to echo back")
	}
}

// ---- Panic recovery ----

func TestRecover_BeegoHandlerPanic(t *testing.T) {
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	w := do(t, x, "GET", "/boom", nil)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("a panic should answer 500, got=%d", w.Code)
	}
	var panicLine map[string]any
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			panicLine = l
		}
	}
	if panicLine == nil {
		t.Fatalf("the panic should be logged, got=%v", lines())
	}
	if msg, _ := panicLine["error"].(string); strings.Contains(msg, "s3cret") {
		t.Errorf("a credential inside the panic value must be masked, got=%v", msg)
	}
	// The stack is taken inside beego's own recovery, where it still points at the handler.
	if stack, _ := panicLine["stack"].(string); !strings.Contains(stack, "xbeego_test.go") {
		t.Errorf("the stack should point at the handler that panicked, got=%v", stack)
	}
	// The outer layers still finished their work.
	if a := access(t, lines()); a["status"] != float64(500) {
		t.Errorf("the access log should record the 500, got=%v", a["status"])
	}
}

func TestRecover_PanicStillCountedAndTraced(t *testing.T) {
	// Without this, the requests that went wrong vanish from the error-rate metric and the trace.
	m := withMetrics(t)
	spans := recording(t)
	testkit.QuietSlog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log = false }))
	do(t, x, "GET", "/boom", nil)

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `route="/boom",status="500"`) {
		t.Errorf("a panicking request should be counted as a 500\ngot=\n%s", out)
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("the span should still be ended, got=%d", len(got))
	}
	if attrsOf(got[0])["http.response.status_code"] != "500" {
		t.Errorf("the span should record the 500, got=%v", attrsOf(got[0]))
	}
	if got[0].Status.Code.String() != "Error" {
		t.Errorf("a 5xx should mark the span as an error, got=%v", got[0].Status)
	}
}

func TestRecover_BeegoAbortIsNotAPanic(t *testing.T) {
	// Controller.Abort / CustomAbort stop a handler by panicking with web.ErrAbort after writing
	// the response. Logging that as a crash would fill the error log with ordinary 401s.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	w := do(t, x, "GET", "/abort", nil)

	if w.Code != http.StatusUnauthorized || w.Body.String() != "nope" {
		t.Errorf("CustomAbort should answer as it always did, got=%d %q", w.Code, w.Body.String())
	}
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			t.Errorf("an abort is control flow, not a failure: %v", l)
		}
	}
	if a := access(t, lines()); a["status"] != float64(401) {
		t.Errorf("the access log should record the 401, got=%v", a["status"])
	}
}

func TestRecover_ErrAbortHandlerIsRePanicked(t *testing.T) {
	// net/http expects it: it breaks the connection silently. Answering a 500 instead would send a
	// response the handler deliberately abandoned.
	testkit.QuietSlog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		do(t, x, "GET", "/drop", nil)
	}()
	if recovered != http.ErrAbortHandler {
		t.Errorf("http.ErrAbortHandler should travel on to net/http, got=%v", recovered)
	}
}

func TestRecover_AbortedRequestRecordedAs499(t *testing.T) {
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	func() {
		defer func() { recover() }()
		do(t, x, "GET", "/drop", nil)
	}()

	if a := access(t, lines()); a["status"] != float64(499) {
		t.Errorf("an aborted request should be recorded as 499, got=%v", a["status"])
	}
}

func TestWithRecoverFunc(t *testing.T) {
	testkit.QuietSlog(t)
	x := New().
		WithConfig(configWith(func(c *Config) { c.Log, c.Metric, c.Trace = false, false, false })).
		WithRecoverFunc(func(w http.ResponseWriter, _ *http.Request, _ any) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("try later"))
		})

	w := do(t, x, "GET", "/boom", nil)
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "try later" {
		t.Errorf("the custom response should be used, got=%d %q", w.Code, w.Body.String())
	}
}

func TestRecoverPanic_OffLeavesBeegosOwnRecovery(t *testing.T) {
	// The slot is beego's one global and an earlier assembly in this process has taken it over, so
	// the real thing goes back first -- which is also the state a fresh process is in.
	installed := beeweb.BConfig.RecoverFunc
	beeweb.BConfig.RecoverFunc = beegoOwnRecover
	t.Cleanup(func() { beeweb.BConfig.RecoverFunc = installed })

	testkit.QuietSlog(t)
	x := New().WithConfig(configWith(func(c *Config) {
		c.Log, c.Metric, c.Trace, c.RecoverPanic = false, false, false, false
	}))
	x.Handler() // assembly is what would install it

	if got := reflect.ValueOf(beeweb.BConfig.RecoverFunc).Pointer(); got != reflect.ValueOf(beegoOwnRecover).Pointer() {
		t.Error("with RecoverPanic off the framework must not touch beego's recovery slot")
	}
	// Beego's own recovery still answers; what is lost is the slog line, the masking and the
	// stack. The request must not escape to net/http either way.
	if w := do(t, x, "GET", "/boom", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("beego's own recovery should still answer 500, got=%d", w.Code)
	}
}

func TestWithRecoverFunc_IsPerInstance(t *testing.T) {
	// Beego has one global slot for the recovery, so the answer has to travel on the request:
	// otherwise whichever instance assembled last would answer for both.
	testkit.QuietSlog(t)
	cfg := configWith(func(c *Config) { c.Log, c.Metric, c.Trace = false, false, false })
	plain := New().WithConfig(cfg)
	custom := New().WithConfig(cfg).WithRecoverFunc(func(w http.ResponseWriter, _ *http.Request, _ any) {
		w.WriteHeader(http.StatusBadGateway)
	})
	// Assemble the custom one last: without the per-request answer it would decide for both.
	plain.Handler()
	custom.Handler()

	if w := do(t, custom, "GET", "/boom", nil); w.Code != http.StatusBadGateway {
		t.Errorf("the instance with its own recovery should use it, got=%d", w.Code)
	}
	if w := do(t, plain, "GET", "/boom", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("the other instance should keep the default, got=%d", w.Code)
	}
}

func TestRecover_CatchesWhatEscapesBeego(t *testing.T) {
	// A panic in middleware mounted with WithMiddleware never reaches beego's router, so beego's
	// recovery cannot see it. Without the backstop it would reach net/http and kill the
	// connection with no log at all.
	lines := captureLog(t)
	x := New().
		WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).
		WithMiddleware(func(http.Handler) http.Handler {
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("outside beego") })
		})

	w := do(t, x, "GET", "/ping", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("the backstop should answer 500, got=%d", w.Code)
	}
	var seen bool
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("the panic should be logged, got=%v", lines())
	}
}

// ---- Starting and stopping ----

func TestStart_ListensAndServes(t *testing.T) {
	testkit.QuietSlog(t)
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	base := serving(t, x, port)

	resp, err := http.Get(base + "/users/7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "user 7" {
		t.Errorf("the real server should serve beego's routes, got=%q", body)
	}
}

func TestStart_InvalidConfigNeverListens(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.ReadHeaderTimeout = 0 }))
	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an invalid config should be returned as an error")
		}
	case <-time.After(2 * time.Second):
		_ = x.Stop(context.Background())
		t.Fatal("with an invalid config Start should return an error, but it started listening")
	}
}

func TestStart_TwiceIsAnError(t *testing.T) {
	testkit.QuietSlog(t)
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	serving(t, x, port)

	if err := x.Start(context.Background()); err == nil {
		t.Fatal("starting an already running server should be an error")
	}
}

func TestStart_UsesConfigFromAssemblyTime(t *testing.T) {
	// The listen address, the timeouts, the middleware and the trusted proxies must all come from
	// one config. "WithConfig after assembly" has no effect, so the port must not change either.
	testkit.QuietSlog(t)
	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(portA)))
	x.Handler()
	x.WithConfig(configWith(quiet, on(portB)))

	serving(t, x, portA)
}

func TestStop_WaitsForRequestsInFlight(t *testing.T) {
	testkit.QuietSlog(t)
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	base := serving(t, x, port)

	done := make(chan string, 1)
	go func() {
		resp, err := http.Get(base + "/slow")
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		done <- string(body)
	}()
	// Let the request reach the handler before shutting down.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := x.Stop(ctx); err != nil {
		t.Fatalf("a graceful stop within the deadline should succeed: %v", err)
	}
	if got := <-done; got != "slow" {
		t.Errorf("the request in flight should have been finished, got=%q", got)
	}
	// Having stopped, it has to be stopped: a Stop that waited and then left the listener open
	// would keep the process alive and keep taking traffic.
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		conn.Close()
		t.Error("the port should no longer accept connections")
	}
}

func TestStop_BeforeStartDoesNotListen(t *testing.T) {
	testkit.QuietSlog(t)
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	if err := x.Stop(context.Background()); err != nil {
		t.Fatalf("stopping before starting should be a no-op: %v", err)
	}
	if err := x.Start(context.Background()); err != nil {
		t.Fatalf("after a stop that arrived first, Start should return without listening: %v", err)
	}
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", port)); err == nil {
		t.Error("the server must not be listening")
	}
}

// ---- Reading the config file ----

func TestCurrentConfig_ReadsTheFile(t *testing.T) {
	testkit.UseConfigEnv(t, "XBeego:\n  Port: 18081\n  TrustedProxies: [203.0.113.0/24]\n")
	c := CurrentConfig()
	if c.Port != 18081 {
		t.Errorf("CurrentConfig should read the file, got=%d", c.Port)
	}
	if !reflect.DeepEqual(c.TrustedProxies, []string{"203.0.113.0/24"}) {
		t.Errorf("the list replaces the default wholesale, got=%v", c.TrustedProxies)
	}
}

func TestLoadConfig_ClaimsTheBlockAndReportsAMistake(t *testing.T) {
	testkit.UseConfigEnv(t, "XBeego:\n  Port: 18082\n")
	if err := loadConfig(context.Background()); err != nil {
		t.Fatalf("a valid block should load: %v", err)
	}

	testkit.UseConfigEnv(t, "XBeego:\n  Prot: 18082\n")
	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("a misspelled key should be reported during startup, not when the server starts")
	}
}

func TestAccessLog_RouteSurvivesAnEarlierFilterThatWrote(t *testing.T) {
	// A filter registered before xbeego's may answer and still let the chain carry on
	// (WithReturnOnOutput(false)). A filter left at beego's default is skipped as soon as anything
	// has been written, which would lose the route template for exactly those requests.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	do(t, x, "GET", "/started", nil)

	if a := access(t, lines()); a["route"] != "/started" {
		t.Errorf("the route should still be captured, got=%v", a["route"])
	}
}

func TestHandler_FilterChainsAreFoldedIn(t *testing.T) {
	// web.Run calls Handlers.Init right before it listens, and a filter chain registered with
	// InsertFilterChain is silently never run without it.
	x := New().WithConfig(configWith(quiet))
	w := do(t, x, "GET", "/chained", nil)

	if w.Header().Get("X-Chain") != "ran" {
		t.Errorf("the filter chain should have run, got headers=%v", w.Header())
	}
	if w.Body.String() != "handler" {
		t.Errorf("the handler should still be reached, got=%q", w.Body.String())
	}
}

func TestConfig_ServerMapping(t *testing.T) {
	// Every one of these reaches the http.Server internal/web builds. A field that quietly stops
	// being copied is a timeout silently turning into "unlimited".
	c := Config{
		Host: "10.0.0.5", Port: 9999, UseH2C: true,
		TLS:               TLSConfig{CertFile: "c.pem", KeyFile: "k.pem", ClientCAFile: "ca.pem", MinVersion: "1.3"},
		ReadHeaderTimeout: 1 * time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       4 * time.Second,
	}
	got := c.server()
	want := web.ServerConfig{
		Host: "10.0.0.5", Port: 9999, UseH2C: true,
		CertFile: "c.pem", KeyFile: "k.pem", ClientCAFile: "ca.pem", MinVersion: "1.3",
		ReadHeaderTimeout: 1 * time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       4 * time.Second,
	}
	if got != want {
		t.Errorf("the server config is wrong:\ngot = %+v\nwant= %+v", got, want)
	}
}

func TestCurrentConfig_InvalidBlockFallsBackToSafeDefaults(t *testing.T) {
	// A half-decoded config may well have TrustedProxies as 0.0.0.0/0, which trusts every
	// X-Forwarded-For in the world. The error is reported by xone.Run; what comes back here has to
	// be the safe side.
	testkit.UseConfigEnv(t, "XBeego:\n  TrustedProxies: [0.0.0.0/0]\n  Port: 0\n")
	c := CurrentConfig()
	if !reflect.DeepEqual(c.TrustedProxies, []string{"private"}) {
		t.Errorf("an invalid block should fall back to the defaults, got=%v", c.TrustedProxies)
	}
	if c.Port != 8080 {
		t.Errorf("the whole config should be the defaults, got=%d", c.Port)
	}
}

func TestStart_TimeoutsReachTheServer(t *testing.T) {
	// ReadHeaderTimeout is the defense against a slow client holding a connection forever; a value
	// that never reaches the http.Server is no defense at all.
	testkit.QuietSlog(t)
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port), func(c *Config) {
		c.ReadHeaderTimeout = 300 * time.Millisecond
	}))
	serving(t, x, port)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Half a request header, and then nothing.
	if _, err := conn.Write([]byte("GET /ping HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("the connection should have been closed by the server, got=%v", err)
	}
}

// xlogCapture replaces the default logger with a real xlog writing to a file, and returns the
// records written so far.
//
// It has to be xlog rather than any slog handler: trace_id and the fields LogScope collects are
// pulled back out by xlog's handler, and another handler shows none of them.
func xlogCapture(t *testing.T) func() []map[string]any {
	t.Helper()
	dir := t.TempDir()
	c := xlog.DefaultConfig()
	c.Console = false
	c.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}

	l, closer, err := xlog.New(c)
	if err != nil {
		t.Fatal(err)
	}
	old := slog.Default()
	slog.SetDefault(l)
	// trace_id reaches the log through an extractor, normally installed by xtrace's Install; the
	// test installs its own rather than depending on another test having run first.
	xlog.SetTraceExtractor(func(ctx context.Context) (string, string) {
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			return "", ""
		}
		return sc.TraceID().String(), sc.SpanID().String()
	})
	t.Cleanup(func() { xlog.SetTraceExtractor(nil); slog.SetDefault(old); _ = closer.Close() })

	return func() []map[string]any {
		_ = closer.Close() // flush before reading
		files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
		if len(files) == 0 {
			return nil
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("the log line is not JSON: %v\n%s", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

// ---- A beego controller, end to end ----

func TestController_LogsCarryTraceIDAndPerRequestFields(t *testing.T) {
	// The whole point of the module in one test: a controller written the ordinary way gets a ctx
	// carrying the trace, what it logs lands beside the access log under the same trace_id, and a
	// field added from business code is on both.
	recording(t)
	lines := xlogCapture(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric = false }))
	w := do(t, x, "GET", "/users-ctl/42", nil)

	if w.Code != 200 {
		t.Fatalf("the controller should have answered, got=%d %q", w.Code, w.Body.String())
	}
	traceID := w.Header().Get(middleware.TraceIDHeader)
	if traceID == "" {
		t.Fatal("the response should carry the trace identifier")
	}

	got := lines()
	var business, accessLine map[string]any
	for _, l := range got {
		switch l["msg"] {
		case "get user":
			business = l
		case "request completed":
			accessLine = l
		}
	}
	if business == nil || accessLine == nil {
		t.Fatalf("both the controller's line and the access log were expected, got=%v", got)
	}
	for name, l := range map[string]map[string]any{"the controller's line": business, "the access log": accessLine} {
		if l["trace_id"] != traceID {
			t.Errorf("%s should carry this request's trace_id %q, got=%v", name, traceID, l["trace_id"])
		}
		if l["tenant"] != "acme" {
			t.Errorf("%s should carry the field added by business code, got=%v", name, l)
		}
	}
	if accessLine["route"] != "/users-ctl/:id" {
		t.Errorf("the access log should use the route template, got=%v", accessLine["route"])
	}
}

func TestController_CopyRequestBodyStillWorksWithTheAccessLog(t *testing.T) {
	// The access log buffers the request body from the http.Request; beego copies it into
	// ctx.Input.RequestBody. Reading it for the log must not take it away from beego.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogRequestBody = true
	}))
	w := do(t, x, "POST", "/echo-body", strings.NewReader(`{"user":"a","password":"s3cret"}`))

	if w.Body.String() != `{"user":"a","password":"s3cret"}` {
		t.Errorf("beego should still receive the whole body, got=%q", w.Body.String())
	}
	if body := access(t, lines())["request_body"].(string); strings.Contains(body, "s3cret") {
		t.Errorf("the log's copy should still be masked, got=%v", body)
	}
}

// ---- A second server in the same process ----

func TestWithConfig_SecondServerOnAnotherPort(t *testing.T) {
	// Beego has one global router, so both serve the same routes; what differs is the port and the
	// switches.
	testkit.QuietSlog(t)
	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	a := New().WithConfig(configWith(quiet, on(portA)))
	b := New().WithConfig(configWith(quiet, on(portB)))
	baseA, baseB := serving(t, a, portA), serving(t, b, portB)

	for _, base := range []string{baseA, baseB} {
		resp, err := http.Get(base + "/users/7")
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "user 7" {
			t.Errorf("%s should serve the same routes, got=%q", base, body)
		}
	}
}

// ---- Registration ----

func TestRegister_OnlyReadsConfigNoStopHook(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xbeego" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("without a start hook the XBeego block is unclaimed and a misconfiguration waits until Start")
	}
	if got.Stage != hook.StageServer {
		t.Errorf("the server belongs in the last stage, got=%v", got.Stage)
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xbeego" {
			t.Error("the server is started and stopped by the caller through xone.Run, so no stop hook belongs here")
		}
	}
}

func TestXBeego_ImplementsRunnable(t *testing.T) {
	// Structural satisfaction is enough, without importing the root package -- "an integration does
	// not depend on the framework" has to hold at compile time. Without this, a signature drifting
	// away from what xone.Run expects would compile here and fail only in a caller's main.
	var _ interface {
		Start(context.Context) error
		Stop(context.Context) error
	} = New()
}

func TestImportingBeegoRegistersTwoCommandLineFlags(t *testing.T) {
	// A side effect of importing beego that a caller can see: its grace package's init registers
	// -graceful and -socketorder on flag.CommandLine (server/web/grace/grace.go). They do nothing
	// here, since web.Run -- the only thing that reads them -- is never called, but they appear in
	// the program's -h output, and a caller defining a flag of the same name panics at startup
	// with "flag redefined".
	//
	// xone itself is unaffected: it reads --config / --profile by scanning os.Args and never
	// touches flag.CommandLine.
	//
	// Pinned by a test so that the note in the package documentation is checked rather than
	// believed: if beego ever stops doing this, this goes red and the note goes.
	for _, name := range []string{"graceful", "socketorder"} {
		if flag.Lookup(name) == nil {
			t.Errorf("beego no longer registers the %q flag; the note about it in config.go can go", name)
		}
	}
}

func TestLoadConfig_NoBlockIsNotAnError(t *testing.T) {
	// A program that imports xbeego for one of two servers, or imports it and configures nothing,
	// must start: the hook claims the block and an absent one means "take the defaults".
	testkit.UseConfigEnv(t, "XApp:\n  Name: demo\n")
	if err := loadConfig(context.Background()); err != nil {
		t.Fatalf("an absent XBeego block should be fine: %v", err)
	}
	if c := CurrentConfig(); c.Port != 8080 || !c.Log {
		t.Errorf("an absent block should give the defaults, got=%+v", c)
	}
}

func TestRecover_RegisteredErrorHandlerIsRendered(t *testing.T) {
	// c.Abort("418") is how a beego application answers an error code it registered a handler for:
	// the controller panics with the code as a string and the recovery renders the handler. Taking
	// beego's recovery over must not turn that into a 500 with an ERROR line per request.
	lines := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false }))
	w := do(t, x, "GET", "/teapot", nil)

	if w.Code != http.StatusTeapot {
		t.Errorf("the registered handler's status should be answered, got=%d", w.Code)
	}
	if w.Body.String() != "i am a teapot" {
		t.Errorf("the registered handler should have written the body, got=%q", w.Body.String())
	}
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			t.Errorf("an abort onto a registered error handler is control flow, not a crash: %v", l)
		}
	}
	if a := access(t, lines()); a["status"] != float64(http.StatusTeapot) {
		t.Errorf("the access log should record what went out, got=%v", a["status"])
	}
}

func TestRecover_ContextAbortKeepsItsStatus(t *testing.T) {
	// Context.Abort sets ctx.Output.Status and then panics with the body. Beego's own recovery
	// answers with that status; answering a bare 500 would turn every such abort into a 500.
	testkit.QuietSlog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Metric, c.Trace = false, false, false }))
	if w := do(t, x, "GET", "/ctx-abort", nil); w.Code != http.StatusForbidden {
		t.Errorf("the status the handler set should be answered, got=%d", w.Code)
	}
}

func TestRecover_WithRecoverFuncStillWinsOverBeegosStatus(t *testing.T) {
	// The caller's answer is the caller's, whichever of the two recoveries catches the panic.
	testkit.QuietSlog(t)
	x := New().
		WithConfig(configWith(func(c *Config) { c.Log, c.Metric, c.Trace = false, false, false })).
		WithRecoverFunc(func(w http.ResponseWriter, _ *http.Request, _ any) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
	if w := do(t, x, "GET", "/ctx-abort", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("WithRecoverFunc should decide, got=%d", w.Code)
	}
}

func TestMetrics_ScrapeHasItsOwnRouteLabel(t *testing.T) {
	// Beego never sees this request, so nothing else fills the route template in: recorded as
	// unmatched, every scrape would land in the bucket a dashboard watches for scanners and typos,
	// at scrape frequency. xgin and xecho register the endpoint as an ordinary route and get
	// route="/metrics" from their router; this has to match.
	m := withMetrics(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace = false, false }))
	do(t, x, "GET", "/metrics", nil)
	do(t, x, "GET", "/metrics", nil)

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `route="/metrics",status="200"} 2`) {
		t.Errorf("a scrape should be labelled with the metrics path\ngot=\n%s", out)
	}
	if strings.Contains(out, `route="unmatched"`) {
		t.Errorf("a scrape must not land in the unmatched bucket\ngot=\n%s", out)
	}
}

func TestAppStartHooksDoNotRun(t *testing.T) {
	// A limitation worth a test rather than only a line of prose: beego runs the callbacks given to
	// web.AddAPPStartHook from initBeforeHTTPRun (server/web/beego.go), which only web.Run reaches.
	// Here they never run -- with no error and no log -- so an application initialising its ORM
	// that way has to be told, and this goes red if a beego upgrade ever changes it.
	var ran bool
	beeweb.AddAPPStartHook(func() error { ran = true; return nil })

	x := New().WithConfig(configWith(quiet))
	do(t, x, "GET", "/ping", nil)

	if ran {
		t.Error("beego now runs AddAPPStartHook callbacks without web.Run; the note about it can go")
	}
}
