package xecho

import (
	"bytes"
	"context"
	"errors"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xecho/middleware"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xmetric"
)

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
	os.WriteFile(path, []byte(yml), 0o644)
	t.Cleanup(config.Reset)
	return config.LoadInto(path, ConfigKey, &c)
}

// configWith changes a few fields on top of the defaults.
//
// Tests always hand the config in through WithConfig and never read the config file, so an
// XONE_CONFIG set on the machine running the tests makes no difference. The few tests that cover
// "reading the config file" use testkit.UseConfigEnv.
func configWith(mutate ...func(*Config)) Config {
	c := DefaultConfig()
	for _, m := range mutate {
		m(&c)
	}
	return c
}

// quiet turns off the access log and the metrics: when they are not what is being tested, this
// keeps the log from flooding and the metrics endpoint from taking a route.
func quiet(c *Config) { c.Log, c.Metric = false, false }

// on listens on this port, on localhost.
func on(port int) func(*Config) {
	return func(c *Config) { c.Host, c.Port = "127.0.0.1", port }
}

// serving starts the server, waits until it really listens, and stops it when the test ends.
// It returns http://127.0.0.1:port.
func serving(t *testing.T, x *XEcho, port int) string {
	t.Helper()
	go func() { _ = x.Start(context.Background()) }()
	t.Cleanup(func() { _ = x.Stop(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/nothing")
	return base
}

// startErr calls Start and takes the error it returns. With an invalid config it should return
// immediately and never listen - after 2s without returning it is taken to be listening, which
// stops it and fails the test.
func startErr(t *testing.T, x *XEcho) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = x.Stop(context.Background())
		t.Fatal("with an invalid config Start should return an error, but it started listening")
		return nil
	}
}

// ok answers 200 and nothing else.
func ok(c echo.Context) error { return c.NoContent(200) }

// echoClientIP registers GET /client-ip, whose body is the c.RealIP() the handler sees.
func echoClientIP(e *echo.Echo) {
	e.GET("/client-ip", func(c echo.Context) error { return c.String(200, c.RealIP()) })
}

// clientIPOf requests /client-ip once from remote with a forged X-Forwarded-For: 1.2.3.4 (see
// echoClientIP) and returns the c.RealIP() the handler saw.
func clientIPOf(t *testing.T, e *echo.Echo, remote string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/client-ip", nil)
	req.RemoteAddr = remote
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w.Body.String()
}

// captureLog replaces slog's default output with JSON written into buf, and restores it when the
// test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
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
	// With it on, echo's default error handler writes the error text into the response.
	if c.Debug {
		t.Errorf("Debug should be off by default, got=%+v", c)
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
	c := load(t, "XEcho:\n  Port: 9090\n  ReadTimeout: 30s\n  UseH2C: true\n  Log: false\n  LogSkipPaths: [/healthz, /static/]\n")
	if c.Port != 9090 || c.ReadTimeout != 30*time.Second || !c.UseH2C {
		t.Errorf("the config did not take effect, got=%+v", c)
	}
	if c.Log || strings.Join(c.LogSkipPaths, " ") != "/healthz /static/" {
		t.Errorf("the middleware switches are in the config too, got=%+v", c)
	}
	if c.Host != "0.0.0.0" || !c.Trace || c.MetricPath != "/metrics" {
		t.Errorf("fields left out should keep their defaults, got=%+v", c)
	}
}

func TestConfig_FailsOnTypo(t *testing.T) {
	if err := loadErr(t, "XEcho:\n  Prot: 9090\n"); err == nil {
		t.Fatal("a misspelled field should fail startup")
	}
	// The gin-only fields do not exist here: copying an XGin block over has to be reported on the
	// spot rather than silently doing nothing.
	for _, f := range []string{"Mode: release", "MaxMultipartMemory: 1024", "ZHTranslations: true"} {
		if err := loadErr(t, "XEcho:\n  "+f+"\n"); err == nil {
			t.Errorf("%s is not an XEcho field and should fail startup", f)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("the default config should be valid: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"port 0":                         func(c *Config) { c.Port = 0 },
		"port out of range":              func(c *Config) { c.Port = 70000 },
		"certificate only":               func(c *Config) { c.TLS.CertFile = "a.pem" },
		"key only":                       func(c *Config) { c.TLS.KeyFile = "a.key" },
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

func TestValidate_RejectsZeroOrNegativeTimeouts(t *testing.T) {
	// For these fields, net/http treats zero and negative values as "no limit" rather than "use a
	// default"; the premise is pinned by xgin's
	// TestNetHTTP_ReadHeaderTimeoutZeroLetsSlowClientHoldConn.
	for name, mutate := range map[string]func(*Config){
		"ReadHeaderTimeout 0":        func(c *Config) { c.ReadHeaderTimeout = 0 },
		"ReadHeaderTimeout negative": func(c *Config) { c.ReadHeaderTimeout = -time.Second },
		"IdleTimeout 0":              func(c *Config) { c.IdleTimeout = 0 },
		"IdleTimeout negative":       func(c *Config) { c.IdleTimeout = -time.Second },
		"ReadTimeout negative":       func(c *Config) { c.ReadTimeout = -time.Second },
		"WriteTimeout negative":      func(c *Config) { c.WriteTimeout = -time.Second },
	} {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}

	// A 0 for ReadTimeout / WriteTimeout is the deliberate "no limit", and is the default.
	c := DefaultConfig()
	c.ReadTimeout, c.WriteTimeout = 0, 0
	if err := c.Validate(); err != nil {
		t.Errorf("ReadTimeout / WriteTimeout of 0 is the default and must not be rejected: %v", err)
	}
}

func TestValidate_HalfConfiguredTLS(t *testing.T) {
	// This is the most dangerous way to get it wrong: the server comes up in plaintext while the
	// config file looks like a certificate was configured.
	c := DefaultConfig()
	c.TLS.CertFile = "cert.pem"
	err := c.Validate()
	if err == nil {
		t.Fatal("half-configured TLS should fail startup rather than silently fall back to plaintext")
	}
	if !strings.Contains(err.Error(), "TLS.CertFile") || !strings.Contains(err.Error(), "TLS.KeyFile") {
		t.Errorf("the error should say what is missing, got=%v", err)
	}
}

func TestValidate_InvalidProxyCIDRFailsStartup(t *testing.T) {
	// One malformed entry fails startup: a half-right proxy table makes client_ip half true and
	// half false, which is far harder to track down than not starting.
	c := DefaultConfig()
	c.TrustedProxies = []string{"10.0.0.0/8", "10.0.0.0/33"}
	if err := c.Validate(); err == nil {
		t.Fatal("a malformed range should be rejected")
	}

	c.TrustedProxies = []string{"10.0.0.0/8", "192.168.1.1", "::1"}
	if err := c.Validate(); err != nil {
		t.Errorf("these are all valid spellings and must not be rejected: %v", err)
	}
}

// ---- Assembly ----

func TestBuild_PanicRecoveredAndStillLogged(t *testing.T) {
	// Recover has to be the innermost of the built-ins: whichever layer catches a panic, the code
	// after next(c) in every layer inside it never runs.
	// Echo does not catch panics itself: measured, the connection is simply broken and the stack
	// goes to stderr.
	buf := captureLog(t)
	x := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.GET("/boom", func(echo.Context) error { panic("boom") })
	})

	w := doRequest(t, x.Engine(), "GET", "/boom")
	if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"message":"Internal Server Error"}` {
		t.Errorf("the panic should be caught and echo's default 500 returned, got=%d %q", w.Code, w.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"panic while handling request"`) ||
		!strings.Contains(out, `"msg":"request completed"`) || !strings.Contains(out, `"status":500`) {
		t.Errorf("both the panic log and an access log with status 500 are expected:\n%s", out)
	}
}

func TestEcho_PanicNotRecoveredByDefault(t *testing.T) {
	// Pins the premise of the test above: echo does not catch panics and lets them reach net/http
	// (which breaks the connection and writes the stack to stderr). The day an upgrade makes echo
	// catch them, this test goes red first and Recover can be reconsidered.
	e := echo.New()
	e.GET("/boom", func(echo.Context) error { panic("boom") })
	defer func() {
		if recover() == nil {
			t.Error("echo does not catch panics by default and should let it escape ServeHTTP")
		}
	}()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom", nil))
}

func TestBuild_MetricsEndpointAutoRegistered(t *testing.T) {
	x := New().WithConfig(DefaultConfig())
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code != 200 {
		t.Errorf("with metrics on, /metrics should be registered automatically, got=%d", w.Code)
	}
	if !strings.Contains(doRequest(t, x.Engine(), "GET", "/metrics").Body.String(), "http_requests_total") {
		t.Error("the metrics endpoint should export the request count metric")
	}
}

func TestBuild_MetricsPathConfigurable(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.MetricPath = "/internal/metrics" }))
	if w := doRequest(t, x.Engine(), "GET", "/internal/metrics"); w.Code != 200 {
		t.Errorf("it should be registered on the configured path, got=%d", w.Code)
	}
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("nothing should remain on the default path")
	}
}

func TestBuild_NoMetricsEndpointWhenDisabled(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.Metric = false }))
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("with metrics off there should be no /metrics")
	}
}

func TestEcho_MetricPathWithoutSlashIsRewritten(t *testing.T) {
	// Pins the premise of the MetricPath validation (echo v4.16.0): a path without a leading / is
	// silently rewritten, and an empty one registers on the root path and is silently replaced by a
	// home page registered later - with no error and no panic.
	e := echo.New()
	e.GET("metrics", func(c echo.Context) error { return c.String(200, "M") })
	if w := doRequest(t, e, "GET", "/metrics"); w.Body.String() != "M" {
		t.Errorf("echo should rewrite metrics into /metrics, got=%d %q", w.Code, w.Body.String())
	}
	e = echo.New()
	e.GET("", func(c echo.Context) error { return c.String(200, "M") })
	e.GET("/", func(c echo.Context) error { return c.String(200, "home") })
	if w := doRequest(t, e, "GET", "/"); w.Body.String() != "home" {
		t.Errorf("an empty path should land on / and be replaced by the home page registered later, got=%q", w.Body.String())
	}
}

func TestBuild_Idempotent(t *testing.T) {
	// Assembling twice registers the middleware twice, which shows up as two log lines per request
	// and doubled metrics.
	x := New().WithConfig(DefaultConfig())
	if x.Engine() != x.Engine() {
		t.Error("assembling again should return the same echo")
	}
}

func TestBuild_UserMiddlewareAfterBuiltin(t *testing.T) {
	var order []string
	mark := func(name string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error { order = append(order, name); return next(c) }
		}
	}
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithMiddleware(mark("user1"), mark("user2")).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/x", func(c echo.Context) error { order = append(order, "handler"); return c.NoContent(200) })
		})

	doRequest(t, x.Engine(), "GET", "/x")
	if strings.Join(order, ",") != "user1,user2,handler" {
		t.Errorf("user middleware should run in order before the handler, got=%v", order)
	}
}

func TestBuild_UserMiddlewareInsideRecover(t *testing.T) {
	// User middleware sits after every built-in: a panic of its own is caught by Recover too, and
	// the access log is still written.
	buf := captureLog(t)
	x := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).
		WithMiddleware(func(echo.HandlerFunc) echo.HandlerFunc {
			return func(echo.Context) error { panic("middleware boom") }
		})
	if w := doRequest(t, x.Engine(), "GET", "/x"); w.Code != 500 {
		t.Errorf("a panic in user middleware should be caught, got=%d", w.Code)
	}
	if !strings.Contains(buf.String(), `"status":500`) {
		t.Errorf("the access log should record the 500:\n%s", buf.String())
	}
}

// ---- Switches ----

func TestLog_NoAccessLogWhenDisabled(t *testing.T) {
	buf := captureLog(t)
	route := func(e *echo.Echo) { e.GET("/work", func(c echo.Context) error { return c.String(200, "ok") }) }

	e := New().WithConfig(configWith(quiet)).WithRoutes(route).Engine()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
	if strings.Contains(buf.String(), "request completed") {
		t.Errorf("with Log: false there should be no access log: %s", buf.String())
	}

	e = New().WithConfig(configWith(func(c *Config) { c.Metric = false })).WithRoutes(route).Engine()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
	if !strings.Contains(buf.String(), "request completed") {
		t.Errorf("the access log should be written by default: %s", buf.String())
	}
}

func TestLog_ErrorStatusesLoggedAsSent(t *testing.T) {
	// The whole assembled chain: an error returned by the handler and the router's 404 / 405 are
	// recorded by the access log as what the client received.
	for _, c := range []struct {
		method, path string
		status       int
		route        string
	}{
		{"GET", "/nope", 404, "unmatched"},
		{"POST", "/only-get", 405, "unmatched"},
		{"GET", "/fail", 500, "/fail"},
		{"GET", "/teapot", 418, "/teapot"},
	} {
		buf := captureLog(t)
		e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
			e.GET("/only-get", ok)
			e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
			e.GET("/teapot", func(echo.Context) error { return echo.NewHTTPError(418) })
		}).Engine()
		w := doRequest(t, e, c.method, c.path)
		want := fmt.Sprintf(`"route":%q,"path":%q,"status":%d`, c.route, c.path, c.status)
		if w.Code != c.status || !strings.Contains(buf.String(), want) ||
			!strings.Contains(buf.String(), fmt.Sprintf(`"bytes_out":%d`, w.Body.Len())) {
			t.Errorf("%s %s: the client received %d (%d bytes), the access log should match:\n%s", c.method, c.path, w.Code, w.Body.Len(), buf.String())
		}
	}
}

func TestLogSkipPaths_ReallySkipsThesePaths(t *testing.T) {
	// Writing it in the config is not enough: it has to travel all the way into middleware.Log.
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogSkipPaths = []string{"/healthz", "/static/"}
	})).WithRoutes(func(e *echo.Echo) {
		e.GET("/healthz", ok)
		e.GET("/static/*", ok)
		e.GET("/work", ok)
	}).Engine()

	for _, path := range []string{"/healthz", "/static/app.js"} {
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		if strings.Contains(buf.String(), path) {
			t.Errorf("the skipped path %s should leave no access log: %s", path, buf.String())
		}
	}
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
	if !strings.Contains(buf.String(), "/work") {
		t.Errorf("a path that is not skipped should still be recorded: %s", buf.String())
	}
}

func TestLogSkipPaths_MetricsPathAddedAutomatically(t *testing.T) {
	// The metrics endpoint is polled every few seconds by the scraper; logging it is pure noise.
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) {
		c.Trace = false
		c.MetricPath = "/internal/metrics"
	})).Engine()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/metrics", nil))

	if strings.Contains(buf.String(), "/internal/metrics") {
		t.Errorf("the metrics endpoint should leave no access log: %s", buf.String())
	}
}

func TestLogBody_TogglesAreIndependent(t *testing.T) {
	// The request body and the response body have a switch each. Both have to travel into
	// middleware.Log, and they must not be crossed: for someone who enabled the request body only,
	// the response body (which may carry a token) must not reach the log.
	for _, c := range []struct {
		name      string
		req, resp bool
	}{
		{"request body only", true, false},
		{"response body only", false, true},
		{"neither by default", false, false},
	} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(cfg *Config) {
			cfg.Metric, cfg.Trace = false, false
			cfg.LogRequestBody, cfg.LogResponseBody = c.req, c.resp
		})).WithRoutes(func(e *echo.Echo) {
			e.POST("/echo", func(c echo.Context) error { return c.JSON(200, map[string]string{"greeting": "hi-bob"}) })
		}).Engine()

		req := httptest.NewRequest("POST", "/echo", strings.NewReader(`{"name":"alice"}`))
		req.Header.Set("Content-Type", "application/json")
		e.ServeHTTP(httptest.NewRecorder(), req)

		out := buf.String()
		if got := strings.Contains(out, "request_body") && strings.Contains(out, "alice"); got != c.req {
			t.Errorf("%s: request body reached the log=%v, want %v\n%s", c.name, got, c.req, out)
		}
		if got := strings.Contains(out, "response_body") && strings.Contains(out, "hi-bob"); got != c.resp {
			t.Errorf("%s: response body reached the log=%v, want %v\n%s", c.name, got, c.resp, out)
		}
	}
}

func TestLogQueryAndHeaders_TogglesReachMiddleware(t *testing.T) {
	// Each of the three switches travels into middleware.Log on its own: crossed or wired to a
	// constant, whatever is off still reaches the log.
	for _, c := range []struct {
		name                string
		query, reqH, header bool
	}{
		{"query string only", true, false, false},
		{"request headers only", false, true, false},
		{"response headers only", false, false, true},
		{"none by default", false, false, false},
	} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(cfg *Config) {
			cfg.Metric, cfg.Trace = false, false
			cfg.LogQuery, cfg.LogRequestHeaders, cfg.LogResponseHeaders = c.query, c.reqH, c.header
		})).WithRoutes(func(e *echo.Echo) {
			e.GET("/search", func(c echo.Context) error {
				c.Response().Header().Set("X-Page-Total", "7")
				c.Response().Header().Set("Set-Cookie", "sid=abc123")
				return c.NoContent(200)
			})
		}).Engine()
		req := httptest.NewRequest("GET", "/search?page=2&token=t0p", nil)
		req.Header.Set("X-Visible", "keep-me")
		req.Header.Set("Authorization", "Bearer h0rse")
		e.ServeHTTP(httptest.NewRecorder(), req)

		out := buf.String()
		if got := strings.Contains(out, `"query":"page=2`); got != c.query {
			t.Errorf("%s: query string reached the log=%v, want %v\n%s", c.name, got, c.query, out)
		}
		if got := strings.Contains(out, "request_headers") && strings.Contains(out, "keep-me"); got != c.reqH {
			t.Errorf("%s: request headers reached the log=%v, want %v\n%s", c.name, got, c.reqH, out)
		}
		if got := strings.Contains(out, "response_headers") && strings.Contains(out, "X-Page-Total"); got != c.header {
			t.Errorf("%s: response headers reached the log=%v, want %v\n%s", c.name, got, c.header, out)
		}
		if strings.Contains(out, "t0p") || strings.Contains(out, "h0rse") || strings.Contains(out, "abc123") {
			t.Errorf("%s: the token in the query string, Authorization and Set-Cookie should all be masked\n%s", c.name, out)
		}
	}
}

func TestTrace_NoSpanWhenDisabled(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	for _, on := range []bool{true, false} {
		exp.Reset()
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.Trace = on })).Engine()
		w := doRequest(t, e, "GET", "/nope")

		if got := len(exp.GetSpans()) > 0; got != on {
			t.Errorf("with Trace=%v a span was opened=%v", on, got)
		}
		if got := w.Header().Get(middleware.TraceIDHeader) != ""; got != on {
			t.Errorf("with Trace=%v the response carried %s=%v", on, middleware.TraceIDHeader, got)
		}
	}
}

func TestMetric_NoRequestMetricsWhenDisabled(t *testing.T) {
	// Not registering /metrics is not enough: the metrics middleware has to follow the switch too,
	// or a server with metrics off still pays for recording them on every request.
	m, closer, err := xmetric.New(xmetric.Config{Namespace: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	for _, metric := range []bool{true, false} {
		route := fmt.Sprintf("/probe-%v", metric)
		e := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, metric })).
			WithRoutes(func(e *echo.Echo) { e.GET(route, ok) }).
			Engine()
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", route, nil))

		out := testkit.Scrape(m.Handler)
		if got := strings.Contains(out, `route="`+route+`"`); got != metric {
			t.Errorf("with Metric=%v request metrics were recorded=%v\n%s", metric, got, out)
		}
	}
}

func TestWithRecoverFunc_ReplacesPanicResponse(t *testing.T) {
	x := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithRecoverFunc(func(c echo.Context, _ any) error { return c.String(503, "custom") }).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/boom", func(echo.Context) error { panic("boom") })
		})

	w := doRequest(t, x.Engine(), "GET", "/boom")
	if w.Code != 503 || w.Body.String() != "custom" {
		t.Errorf("the custom recover did not take effect, got=%d %q", w.Code, w.Body.String())
	}
}

// validatorFunc is a minimal echo.Validator: a real one uses something like
// go-playground/validator, and which to use is the business's call - the framework only installs
// it on e.Validator.
type validatorFunc func(any) error

func (f validatorFunc) Validate(i any) error { return f(i) }

func TestEcho_NoValidatorByDefault(t *testing.T) {
	// Pins echo's default (v4.16.0): without a validator, c.Validate returns
	// ErrValidatorNotRegistered rather than "valid" - a service that forgot to install one shows
	// nothing at all until someone sends an invalid body. It is a plain error, so a handler
	// returning it directly gives the client a 500 {"message":"Internal Server Error"}.
	var got error
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
		e.GET("/users", func(c echo.Context) error {
			got = c.Validate(&struct{ Name string }{})
			return c.NoContent(204)
		})
	}).Engine()
	doRequest(t, e, "GET", "/users")

	if !errors.Is(got, echo.ErrValidatorNotRegistered) {
		t.Errorf("without a validator c.Validate should return ErrValidatorNotRegistered, got=%v", got)
	}
}

func TestWithValidator_HandlerCanValidate(t *testing.T) {
	var seen any
	x := New().WithConfig(configWith(quiet)).
		WithValidator(validatorFunc(func(i any) error { seen = i; return errors.New("name is required") })).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/users", func(c echo.Context) error {
				if err := c.Validate(&struct{ Name string }{}); err != nil {
					return echo.NewHTTPError(http.StatusBadRequest, err.Error())
				}
				return c.NoContent(204)
			})
		})

	w := doRequest(t, x.Engine(), "GET", "/users")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "name is required") {
		t.Errorf("the validator was not installed, got=%d %q", w.Code, w.Body.String())
	}
	if _, ok := seen.(*struct{ Name string }); !ok {
		t.Errorf("c.Validate should hand the validator the very value the handler passed, got=%T", seen)
	}
}

func TestWithValidator_CallbackOverrides(t *testing.T) {
	// Same as e.IPExtractor and e.HTTPErrorHandler: what the callback sets wins over what was
	// installed before it.
	x := New().WithConfig(configWith(quiet)).
		WithValidator(validatorFunc(func(any) error { return errors.New("from WithValidator") })).
		WithRoutes(func(e *echo.Echo) {
			e.Validator = validatorFunc(func(any) error { return errors.New("from callback") })
			e.GET("/users", func(c echo.Context) error { return c.String(200, c.Validate(&struct{}{}).Error()) })
		})

	if got := doRequest(t, x.Engine(), "GET", "/users").Body.String(); got != "from callback" {
		t.Errorf("an e.Validator set in the callback should win over WithValidator, got=%q", got)
	}
}

// ---- The config landing on echo ----

func TestEcho_DefaultIPExtractorTrustsEveryone(t *testing.T) {
	// Pins the premise of the tests below (echo v4.16.0): with IPExtractor nil, c.RealIP()
	// believes whoever sends a forwarding header. The day echo's default becomes safe, this test
	// goes red first.
	e := echo.New()
	echoClientIP(e)
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "1.2.3.4" {
		t.Errorf("echo's default believes everyone and accepts a forged X-Forwarded-For from a public peer, got=%q", got)
	}
	req := httptest.NewRequest("GET", "/client-ip", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Real-IP", "5.6.7.8")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Body.String() != "5.6.7.8" {
		t.Errorf("X-Real-IP alone is accepted too, got=%q", w.Body.String())
	}
}

func TestBuild_TrustsOnlyPrivateProxiesByDefault(t *testing.T) {
	// Echo's own default believes everyone, so anyone sending an X-Forwarded-For decides the
	// client_ip in the access log - the log can be forged, and the rate limiting and auditing built
	// on that field fail with it. Only private ranges are trusted by default: what a load balancer
	// or an ingress forwards is accepted, a public peer's is not.
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("a public peer's X-Forwarded-For must not be believed; client_ip should be the peer address itself, got=%q (the header forged 1.2.3.4)", got)
	}
	if !strings.Contains(buf.String(), `"client_ip":"203.0.113.9"`) {
		t.Errorf("the access log client_ip should also be the peer itself:\n%s", buf.String())
	}
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("an X-Forwarded-For from a private-range peer (a load balancer) should be believed, got=%q", got)
	}
	req := httptest.NewRequest("GET", "/client-ip", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Real-IP", "5.6.7.8")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Body.String() != "203.0.113.9" {
		t.Errorf("a public peer's X-Real-IP must not be believed either, got=%q", w.Body.String())
	}
}

func TestBuild_ClientIPWalksForwardedChainLikeXgin(t *testing.T) {
	// Several proxy hops: walk right to left and take the first untrusted address, as xgin does
	// (gin's ClientIP); the two are compared entry by entry in xgin's
	// TestClientIP_SharedRuleMatchesGin.
	e := New().WithConfig(configWith(quiet)).WithRoutes(echoClientIP).Engine()
	for xff, want := range map[string]string{
		"1.2.3.4, 5.6.7.8, 10.0.0.9": "5.6.7.8",  // 10.0.0.9 is our own proxy, 5.6.7.8 is the nearest outsider
		"10.0.0.7, 10.0.0.8":         "10.0.0.7", // all trusted, so take the leftmost
		"1.2.3.4, junk":              "10.0.0.5", // after an unparsable entry, stop and fall back to the peer
	} {
		req := httptest.NewRequest("GET", "/client-ip", nil)
		req.RemoteAddr = "10.0.0.5:1"
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Body.String() != want {
			t.Errorf("X-Forwarded-For: %s -> client_ip should be %s, got=%q", xff, want, w.Body.String())
		}
	}
}

func TestBuild_TrustedProxiesEmptyListTrustsNone(t *testing.T) {
	// The default is [private], so trusting nobody has to be expressible.
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).
		WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("with [] nobody should be trusted, got=%q", got)
	}
}

func TestBuild_ForwardedHeadersOnlyWithProxiesConfigured(t *testing.T) {
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{"203.0.113.0/24"} })).
		WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "1.2.3.4" {
		t.Errorf("with the peer inside a trusted range, the forwarded address should be believed, got=%q", got)
	}
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("the list replaces the default wholesale, so without private the private ranges are not trusted, got=%q", got)
	}
}

func TestBuild_CallbackEngineSettingsOverrideConfig(t *testing.T) {
	// The callback runs after the config has landed on echo: what the caller set explicitly in
	// code wins. Checked again with the server running: Start must not apply the config a second
	// time.
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port))).
		WithRoutes(echoClientIP, func(e *echo.Echo) { e.IPExtractor = echo.ExtractIPDirect() })
	x.Engine()
	serving(t, x, port)

	if got := clientIPOf(t, x.Engine(), "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("the callback switched to ExtractIPDirect, so client_ip should be the direct peer, got=%q", got)
	}
}

func TestBuild_CallbackHTTPErrorHandlerStillLoggedAsSent(t *testing.T) {
	// The callback replaced the error handler: that is what the middleware calls through c.Error,
	// and the access log records the status it wrote.
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).
		WithRoutes(func(e *echo.Echo) {
			e.HTTPErrorHandler = func(err error, c echo.Context) { _ = c.String(503, "custom error") }
			e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
		}).Engine()
	w := doRequest(t, e, "GET", "/fail")
	if w.Code != 503 || w.Body.String() != "custom error" || !strings.Contains(buf.String(), `"status":503`) {
		t.Errorf("the callback's error handler should be used and the access log should record 503, got=%d %q\n%s", w.Code, w.Body.String(), buf.String())
	}
}

func TestEcho_TrailingSlashAndHEADNotRouted(t *testing.T) {
	// Pins two echo defaults written in the README under "Behavior (measured)" (v4.16.0), which
	// the framework does not change: trailing slashes match strictly (/a/ is a 404, while gin
	// redirects 301 to /a), and HEAD is not routed to GET automatically (405).
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) { e.GET("/a", ok) }).Engine()
	if w := doRequest(t, e, "GET", "/a/"); w.Code != 404 {
		t.Errorf("/a/ should be a 404, got=%d", w.Code)
	}
	if w := doRequest(t, e, "HEAD", "/a"); w.Code != 405 || w.Header().Get("Allow") != "OPTIONS, GET" {
		t.Errorf("HEAD should be 405 with Allow: OPTIONS, GET, got=%d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := doRequest(t, e, "OPTIONS", "/a"); w.Code != 204 || w.Header().Get("Allow") != "OPTIONS, GET" {
		t.Errorf("OPTIONS is answered by echo itself with 204, got=%d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestEcho_TrailingParamMatchesSlashes(t *testing.T) {
	// Pins an echo default written in the README under "Behavior (measured)": a path parameter at
	// the end of a route swallows the slashes after it, so /users/:id matches /users/1/z with id
	// being 1/z.
	e := echo.New()
	e.GET("/users/:id", func(c echo.Context) error { return c.String(200, c.Param("id")) })
	if w := doRequest(t, e, "GET", "/users/1/z"); w.Code != 200 || w.Body.String() != "1/z" {
		t.Errorf("got=%d %q", w.Code, w.Body.String())
	}
}

func TestDebug_OffByDefaultKeepsErrorDetailOutOfResponse(t *testing.T) {
	// Error text often holds a DSN, SQL or an internal address: those are masked on their way into
	// the access log and not on their way into the response. By default (e.Debug false, as in
	// echo.New) only echo's generic phrase comes back.
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
		e.GET("/fail", func(echo.Context) error { return errors.New("dial 10.0.0.9:5432: password=hunter2") })
	}).Engine()

	w := doRequest(t, e, "GET", "/fail")
	if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"message":"Internal Server Error"}` {
		t.Errorf("the error text must not reach the response by default, got=%d %q", w.Code, w.Body.String())
	}
}

func TestDebug_PutsErrorDetailInResponse(t *testing.T) {
	// Debug: true lands on e.Debug, and echo's default error handler puts the error text into the
	// response (v4.16.0).
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.Debug = true })).WithRoutes(func(e *echo.Echo) {
		e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
	}).Engine()

	w := doRequest(t, e, "GET", "/fail")
	if w.Code != 500 || w.Body.String() != "{\n  \"error\": \"boom\",\n  \"message\": \"Internal Server Error\"\n}\n" {
		t.Errorf("with Debug on the error text should reach the response, and indented, got=%d %q", w.Code, w.Body.String())
	}
}

func TestDebug_PrettyPrintsJSON(t *testing.T) {
	// The other thing e.Debug does: c.JSON / c.JSONP / c.XML indent by two spaces
	// (c.echo.Debug || pretty in context.go), the same as every request carrying ?pretty. The body
	// grows and bytes_out grows with it.
	route := func(e *echo.Echo) {
		e.GET("/j", func(c echo.Context) error { return c.JSON(200, map[string]string{"a": "b"}) })
	}
	plain := New().WithConfig(configWith(quiet)).WithRoutes(route).Engine()
	debug := New().WithConfig(configWith(quiet, func(c *Config) { c.Debug = true })).WithRoutes(route).Engine()

	if got := doRequest(t, plain, "GET", "/j").Body.String(); got != "{\"a\":\"b\"}\n" {
		t.Errorf("with Debug off c.JSON does not indent (10 bytes), got=%q", got)
	}
	if got := doRequest(t, debug, "GET", "/j").Body.String(); got != "{\n  \"a\": \"b\"\n}\n" {
		t.Errorf("with Debug on c.JSON indents by two spaces (15 bytes), got=%q", got)
	}

	// The access log records the larger byte count: bytes_out counts the body written through
	// c.Response().
	buf := captureLog(t)
	logged := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace, c.Debug = false, false, true })).
		WithRoutes(route).Engine()
	doRequest(t, logged, "GET", "/j")
	if !strings.Contains(buf.String(), `"bytes_out":15`) {
		t.Errorf("with Debug on the access log should record 15 bytes, got=%s", buf.String())
	}
}

func TestDebug_RaisesEchoLoggerLevel(t *testing.T) {
	// Echo raises gommon's level from e.Debug only inside e.Start, which xecho does not use, so it
	// sets the level itself (see applyConfig). Echo's one log line below ERROR: writing again to a
	// response that is already committed (response.go, WARN).
	twice := func(e *echo.Echo) {
		e.GET("/twice", func(c echo.Context) error {
			_ = c.String(200, "a")
			return c.String(200, "b")
		})
	}
	for _, debug := range []bool{false, true} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.Debug = debug })).WithRoutes(twice).Engine()
		doRequest(t, e, "GET", "/twice")

		if got := strings.Contains(buf.String(), "response already committed"); got != debug {
			t.Errorf("with Debug=%v echo's WARN should be present=%v, got=%v\n%s", debug, debug, got, buf.String())
		}
	}
}

// ---- Echo's own log ----

// failWriter fails every response body write, as if the client had already disconnected.
type failWriter struct{ h http.Header }

func (w *failWriter) Header() http.Header       { return w.h }
func (w *failWriter) WriteHeader(int)           {}
func (w *failWriter) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

func TestEchoLogger_DefaultWritesToStdout(t *testing.T) {
	// Pins the premise of that part of applyConfig (echo v4.16.0 / gommon v0.5.0): echo's own log
	// writes to os.Stdout in its own JSON format, bypassing slog.
	if out := echo.New().Logger.Output(); out != os.Stdout {
		t.Errorf("echo's log writes to os.Stdout by default, got=%T", out)
	}
}

func TestEchoLogger_RoutedToSlog(t *testing.T) {
	// When writing the error response fails, echo's default error handler calls e.Logger.Error:
	// routed into slog, no longer to stdout.
	buf := captureLog(t)
	e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
		e.GET("/fail", func(echo.Context) error { return errors.New("boom") })
	}).Engine()
	if out := e.Logger.Output(); out == os.Stdout || out == os.Stderr {
		t.Fatalf("echo's log should no longer write to standard output directly, got=%T", out)
	}
	e.ServeHTTP(&failWriter{h: http.Header{}}, httptest.NewRequest("GET", "/fail", nil))

	out := buf.String()
	if !strings.Contains(out, `"level":"ERROR","msg":"echo internal log","message":"write: broken pipe"`) {
		t.Errorf("echo's log should turn into one slog ERROR line:\n%s", out)
	}

	// The level stays echo's default (ERROR and above), and the levels map across.
	buf.Reset()
	e.Logger.Warn("dropped")
	e.Logger.Print("printed")
	if strings.Contains(buf.String(), "dropped") || !strings.Contains(buf.String(), `"level":"INFO","msg":"echo internal log","message":"printed"`) {
		t.Errorf("WARN is filtered out by echo's level and Print is recorded as INFO:\n%s", buf.String())
	}
}

func TestEchoLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARN": slog.LevelWarn,
		"ERROR": slog.LevelError, "PANIC": slog.LevelError, "FATAL": slog.LevelError, "-": slog.LevelInfo,
	} {
		if got := echoLevel(in); got != want {
			t.Errorf("echoLevel(%q)=%v want %v", in, got, want)
		}
	}
}

// ---- Start and stop ----

func TestStartStop_GracefulShutdown(t *testing.T) {
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", func(c echo.Context) error { return c.String(200, "pong") })
	})

	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/ping", port)
	waitServing(t, url)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pong" {
		t.Errorf("the response is wrong, got=%s", body)
	}

	if err := x.Stop(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a graceful shutdown must not return an error, got=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the shutdown")
	}
}

func TestStart_ServerTimeoutsFromConfig(t *testing.T) {
	// All four timeouts on echo's own e.Server are 0; this module does not use it and builds one
	// from the config through web.Server. A connection sending half a request header should be
	// dropped at ReadHeaderTimeout.
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.ReadHeaderTimeout = 200 * time.Millisecond }))
	serving(t, x, port)
	if e := echo.New(); e.Server.ReadHeaderTimeout != 0 || e.Server.IdleTimeout != 0 {
		t.Fatalf("precondition: echo's e.Server timeouts are 0, got=%+v", e.Server)
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // only half the headers
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = io.ReadAll(conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the server should drop it when ReadHeaderTimeout expires; 3s later it still has not")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("dropped too late: %v", d)
	}
}

func TestStart_DoesNotListenOnInvalidConfig(t *testing.T) {
	err := startErr(t, New().WithConfig(configWith(func(c *Config) { c.TLS.CertFile = "half configured" })))

	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xecho" || xe.Op != "config" {
		t.Fatalf("this should be an xecho config error, got=%v", err)
	}
	// A process may hold two servers: the error has to say it is the WithConfig one, and which
	// field it is.
	if !strings.Contains(err.Error(), "WithConfig") || !strings.Contains(err.Error(), "TLS.CertFile") {
		t.Errorf("the error should say which config and which field, got=%v", err)
	}
}

func TestStart_UsesConfigFromBuildTime(t *testing.T) {
	// The listen address, the timeouts and the middleware and trusted proxies on echo must come
	// from one config. "WithConfig after assembly" has no effect, so the port must not change either.
	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(portA)))
	x.Engine()
	x.WithConfig(configWith(quiet, on(portB)))

	serving(t, x, portA)
}

func TestStart_SignalArrivesBeforeStart(t *testing.T) {
	// Listening as usual would bring the server up after the stop signal has already arrived, and
	// then keep it running until the framework times out.
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	if err := x.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start must not fail: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("it should return immediately, got=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start after Stop must not actually listen")
	}

	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("the server must not come up")
	}
}

func TestStart_RepeatedStartFails(t *testing.T) {
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port)))
	serving(t, x, port)

	err := x.Start(context.Background())
	if err == nil || xerror.Module(err) != "xecho" {
		t.Errorf("starting twice should report an xecho error, got=%v", err)
	}
}

func TestStop_SafeWithoutStart(t *testing.T) {
	if err := New().Stop(context.Background()); err != nil {
		t.Errorf("Stop without a Start must not fail: %v", err)
	}
}

// servingWithHungRequestDone starts a server and leaves one request hanging inside the handler.
// It returns a channel closed when that hanging request finishes - the only way to see whether
// the forced disconnect took effect.
func servingWithHungRequestDone(t *testing.T) (*XEcho, <-chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", ok)
		e.GET("/hang", func(c echo.Context) error { <-release; return c.NoContent(200) })
	})
	go x.Start(context.Background())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")

	hung := make(chan struct{})
	go func() {
		defer close(hung)
		if resp, err := http.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	// Wait until the request really reached the handler, or Shutdown may finish before it does.
	time.Sleep(100 * time.Millisecond)
	return x, hung
}

// stopWithin calls Stop with a 200ms deadline and fails if it has not returned within 3s.
func stopWithin(t *testing.T, x *XEcho) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- x.Stop(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Stop should give up at the caller's deadline; 3s later it still has not returned")
		return nil
	}
}

func TestStop_HonorsCallerDeadline(t *testing.T) {
	// How long it waits comes from the caller's ctx alone; xone.Run passes the server's share of
	// the stop budget.
	x, _ := servingWithHungRequestDone(t)
	if err := stopWithin(t, x); err == nil {
		t.Error("in-flight requests outlived the deadline, which should be reported faithfully")
	}
}

func TestStop_ForceClosesInFlightConnsAfterTimeout(t *testing.T) {
	// A Shutdown timeout only returns an error and touches no connection; what matters is whether
	// that in-flight request was actually dropped.
	x, hung := servingWithHungRequestDone(t)
	if err := stopWithin(t, x); err == nil {
		t.Fatal("in-flight requests outlived the deadline and that should be an error")
	}

	select {
	case <-hung:
	case <-time.After(2 * time.Second):
		t.Error("an in-flight request is still running past the timeout - the framework is about to close the database and will find a closed pool")
	}
}

func TestStop_WaitsForHandlersAfterForceClose(t *testing.T) {
	// Close only drops connections and cancels the request ctx; the handler's goroutine keeps
	// running, so Stop has to wait for a handler that is finishing up to really return.
	port := testkit.FreePort(t)
	var returned atomic.Bool
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", ok)
		e.GET("/hang", func(c echo.Context) error {
			<-c.Request().Context().Done()
			time.Sleep(50 * time.Millisecond) // finishing up: rolling back, writing an audit entry...
			returned.Store(true)
			return nil
		})
	})
	go x.Start(context.Background())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	go func() {
		if resp, err := http.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // wait until the request really reached the handler

	// 20% of the budget is reserved for the cleanup after the disconnect (shutdownCtx): a 2s
	// budget leaves 400ms, eight times the 50ms of cleanup here.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := x.Stop(ctx)
	if !returned.Load() {
		t.Fatal("the handler had not returned when Stop did - the framework is about to close the database")
	}
	if err == nil || strings.Contains(err.Error(), "still running") {
		t.Errorf("every handler returned, so only the timeout disconnect should be reported, got=%v", err)
	}
}

func TestStop_HandlerIgnoringCtxReportsRemainingCount(t *testing.T) {
	// Go has no way to terminate a goroutine from outside, so a handler ignoring the ctx keeps
	// running after the disconnect. The framework cannot stop it, but it has to say so plainly
	// rather than let anyone believe everything stopped cleanly.
	x, _ := servingWithHungRequestDone(t)
	err := stopWithin(t, x)
	if err == nil || !strings.Contains(err.Error(), "1 handler(s) still running") || xerror.Module(err) != "xecho" {
		t.Errorf("this should be xecho reporting 1 handler still running, got=%v", err)
	}
}

// ---- Reading the config file ----

func TestEngine_UsesFinalConfigEvenBeforeRun(t *testing.T) {
	// The caller builds an XEcho at the top of main and calls Engine() before xone.Run: the config
	// is loaded the first time it is read, so with New first and the config file in place later,
	// assembly still reads the final values.
	x := New().WithRoutes(echoClientIP)
	testkit.UseConfigEnv(t, "XEcho:\n  TrustedProxies: [203.0.113.0/24]\n  Metric: false\n")

	if got := clientIPOf(t, x.Engine(), "203.0.113.9:1234"); got != "1.2.3.4" {
		t.Errorf("TrustedProxies from the config did not take effect, client_ip=%q", got)
	}
	if w := doRequest(t, x.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("the config turned metrics off, so there should be no /metrics")
	}
}

func TestEngine_InvalidConfigFallsBackToSafeDefaults(t *testing.T) {
	// An echo is still returned (the caller may have taken it before Run) but assembled from the
	// defaults: a half-decoded invalid config may well have TrustedProxies as 0.0.0.0/0. The error
	// is reported by Start, which does not listen.
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	x := New().WithRoutes(echoClientIP)
	if got := clientIPOf(t, x.Engine(), "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("an invalid config should fall back to the defaults (private ranges only), so a public peer's forwarding header must not be believed, client_ip=%q", got)
	}

	err := startErr(t, x)
	if xerror.Module(err) != "xecho" || !xerror.Is(err, "xconfig") || !strings.Contains(err.Error(), "Port") {
		t.Errorf("this should be xecho reporting, wrapping the error of that config block, got=%v", err)
	}
}

func TestCurrentConfig_AlwaysReadsConfigSection(t *testing.T) {
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 18080\n  Log: false\n")
	c := CurrentConfig()
	if c.Port != 18080 || c.Log {
		t.Errorf("the values from the config file should be read, got=%+v", c)
	}
	if c.Host != "0.0.0.0" || !c.Metric {
		t.Errorf("fields left out should keep their defaults, got=%+v", c)
	}
}

func TestCurrentConfig_ReturnsDefaultsWhenSectionInvalid(t *testing.T) {
	// What this returns gets handed to WithConfig, so it has to be safe: a half-decoded invalid
	// config does not qualify.
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	if got := CurrentConfig(); !reflect.DeepEqual(got, DefaultConfig()) {
		t.Errorf("it should fall back to the defaults, got=%+v", got)
	}
}

func TestLoadConfig_StartupFailsOnFieldTypo(t *testing.T) {
	testkit.UseConfigEnv(t, "XEcho:\n  Prot: 8080\n")
	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("a misspelled field should fail startup")
	}
}

func TestLoadConfig_StartupFailsOnInvalidValue(t *testing.T) {
	// Config implements Validate, which runs during decoding, so this StageServer hook fails
	// startup on the spot.
	for name, yml := range map[string]string{
		"port out of range":                "XEcho:\n  Port: 70000\n",
		"half-configured TLS":              "XEcho:\n  TLS:\n    CertFile: cert.pem\n",
		"metrics path without a leading /": "XEcho:\n  MetricPath: metrics\n",
	} {
		testkit.UseConfigEnv(t, yml)
		if err := loadConfig(context.Background()); err == nil {
			t.Errorf("%s should fail startup", name)
		}
	}
}

func TestLoadConfig_EmptyTrustedProxiesInFileTrustsNone(t *testing.T) {
	// A [] in the config file has to become a genuinely empty list rather than being decoded as
	// "not written" and left at the private default.
	testkit.UseConfigEnv(t, "XEcho:\n  TrustedProxies: []\n")
	if got := clientIPOf(t, New().WithRoutes(echoClientIP).Engine(), "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("a [] in the config file should trust nobody, got=%q", got)
	}
}

func TestWithConfig_TwoInstancesListenOnOwnPorts(t *testing.T) {
	// The block in the config file stays on the default port, which shows neither instance
	// listened according to it.
	testkit.UseConfigEnv(t, "XEcho:\n  Port: 8080\n")

	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	for _, s := range []struct {
		port int
		body string
	}{{portA, "A"}, {portB, "B"}} {
		c := CurrentConfig()
		c.Host, c.Port = "127.0.0.1", s.port
		quiet(&c)
		body := s.body
		x := New().WithConfig(c).WithRoutes(func(e *echo.Echo) {
			e.GET("/who", func(c echo.Context) error { return c.String(200, body) })
		})
		serving(t, x, s.port)
	}

	for _, c := range []struct {
		port int
		want string
	}{{portA, "A"}, {portB, "B"}} {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/who", c.port))
		if err != nil {
			t.Fatalf("port %d is unreachable: %v", c.port, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != c.want {
			t.Errorf("port %d should be instance %s, got=%q", c.port, c.want, got)
		}
	}
}

func TestWithConfig_InstancesKeepTheirOwnTrustedProxies(t *testing.T) {
	// IPExtractor belongs to each echo instance (unlike gin.SetMode, which is process-wide): the
	// two configs govern their own.
	a := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).WithRoutes(echoClientIP).Engine()
	b := New().WithConfig(configWith(quiet)).WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, a, "10.0.0.5:1"); got != "10.0.0.5" {
		t.Errorf("instance A trusts nobody, got=%q", got)
	}
	if got := clientIPOf(t, b, "10.0.0.5:1"); got != "1.2.3.4" {
		t.Errorf("instance B trusts the private ranges, got=%q", got)
	}
}

func TestWithConfig_FollowsConfigFileWhenOmitted(t *testing.T) {
	port := testkit.FreePort(t)
	testkit.UseConfigEnv(t, fmt.Sprintf("XEcho:\n  Host: 127.0.0.1\n  Port: %d\n  Log: false\n  Metric: false\n", port))

	x := New().WithRoutes(func(e *echo.Echo) { e.GET("/ping", ok) })
	serving(t, x, port)
}

// ---- Registration ----

func TestRegister_OnlyReadsConfigNoStopHook(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xecho" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("without a start hook the XEcho block is unclaimed and a misconfiguration waits until Start")
	}
	if got.Stage != hook.StageServer {
		t.Errorf("the server belongs in the last stage, got=%v", got.Stage)
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xecho" {
			t.Error("the server is started and stopped by the caller through xone.Run, so no stop hook belongs here")
		}
	}
}

func TestXEcho_ImplementsRunnable(t *testing.T) {
	// Structural satisfaction is enough, without importing the root package - "an integration does
	// not depend on the framework" has to hold at compile time.
	var _ interface {
		Start(context.Context) error
		Stop(context.Context) error
	} = New()
}

// doRequest sends one request to an echo.
func doRequest(t *testing.T, e *echo.Echo, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// waitServing waits until the server really listens.
func waitServing(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the server never came up: %s", url)
}

func TestBuild_MetricsKeptWhenEngineFetchedFirst(t *testing.T) {
	// Grabbing xmetric's registry at assembly time, before xmetric is initialized, registers the
	// metrics onto a fallback registry that is never exported.
	x := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) { e.GET("/x", ok) })
	e := x.Engine() // taking the echo before Run is a natural thing for a caller to do

	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, "demo_http_requests_total") {
		t.Errorf("metrics should be recorded on the registry that exists after initialization\ngot=\n%s", out)
	}
}

func TestBuild_MetricsEndpointOKWhenEngineFetchedFirst(t *testing.T) {
	// The /metrics handler is the same story: fixed at assembly time it would export that empty
	// fallback registry forever.
	e := New().WithConfig(DefaultConfig()).Engine()

	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))
	w := doRequest(t, e, "GET", "/metrics")

	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("/metrics should export the registry currently in effect\ngot=\n%s", w.Body.String())
	}
}

func TestBuild_MetricsEndpointRunsUserMiddleware(t *testing.T) {
	// The shared auth mounted with WithMiddleware applies to business routes, to the metrics
	// endpoint and to unmatched paths alike: an endpoint believed to be protected must not be open.
	auth := func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { return echo.ErrUnauthorized }
	}

	e := New().WithConfig(configWith(func(c *Config) { c.Log = false })).
		WithMiddleware(auth).
		WithRoutes(func(e *echo.Echo) { e.GET("/biz", ok) }).Engine()

	for _, path := range []string{"/biz", "/metrics", "/nope"} {
		if w := doRequest(t, e, "GET", path); w.Code != http.StatusUnauthorized {
			t.Errorf("%s should be stopped by the auth middleware, got=%d", path, w.Code)
		}
	}
}

// ---- The trust boundary for forwarded headers ----

// trustProbe is a Propagator that only records whether the carrier handed to Extract declared the
// peer trusted. The declaration is the TrustedPeer() bool that xtrace agreed on, see
// xtrace.HeaderPropagator.Extract.
type trustProbe struct{ got *atomic.Bool }

func (p trustProbe) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	t, ok := c.(interface{ TrustedPeer() bool })
	p.got.Store(ok && t.TrustedPeer())
	return ctx
}
func (trustProbe) Inject(context.Context, propagation.TextMapCarrier) {}
func (trustProbe) Fields() []string                                   { return nil }

// probeTrust installs trustProbe and reports whether this request's peer was taken as trusted.
func probeTrust(t *testing.T) func(e *echo.Echo, remote string) bool {
	t.Helper()
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got atomic.Bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	return func(e *echo.Echo, remote string) bool {
		got.Store(false)
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		// Trust depends on the direct peer alone, not on c.RealIP(): a public address forwarded by a
		// trusted peer must not make it untrusted, and a public peer must not pass as one of us
		// through a forwarding header.
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		e.ServeHTTP(httptest.NewRecorder(), req)
		return got.Load()
	}
}

func TestBuild_PassthroughHeadersOnlyFromTrustedProxies(t *testing.T) {
	// Accepting forwarded headers from everyone means a public client can send an X-Tenant-Id or
	// X-Internal-Token and have it treated as coming from one of us, carried into every internal
	// call. Who counts as one of us comes from the trusted-proxy table.
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet, func(c *Config) {
		c.TrustedProxies = []string{"10.0.0.0/8", "192.168.1.7"}
	})).Engine()

	for remote, want := range map[string]bool{
		"10.0.0.5:1234":         true,  // inside the range
		"192.168.1.7:80":        true,  // a single IP
		"[::ffff:10.0.0.5]:443": true,  // the IPv4-mapped IPv6 spelling
		"192.168.1.8:80":        false, // the neighbour of that single IP
		"1.2.3.4:1234":          false, // public
		"[2001:db8::1]:443":     false,
	} {
		if got := trusted(e, remote); got != want {
			t.Errorf("peer %s trusted=%v, want %v", remote, got, want)
		}
	}
}

func TestBuild_PassthroughHeadersOnlyFromPrivateByDefault(t *testing.T) {
	// Pod IPs in Kubernetes are random but all inside the private ranges: trusted by default,
	// with nothing to list.
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet)).Engine()

	for remote, want := range map[string]bool{
		"127.0.0.1:1234":        true, // sidecar
		"10.0.0.5:1234":         true, // a pod or a load balancer
		"172.20.1.2:1234":       true,
		"192.168.1.7:80":        true,
		"100.64.3.4:80":         true, // some CNIs use this as the pod range
		"[::1]:443":             true,
		"[fd00::5]:443":         true,  // IPv6 ULA
		"[::ffff:10.0.0.5]:443": true,  // the IPv4-mapped IPv6 spelling
		"203.0.113.9:1234":      false, // public
		"172.32.0.1:1234":       false, // the neighbour of 172.16.0.0/12
		"[2001:db8::1]:443":     false,
	} {
		if got := trusted(e, remote); got != want {
			t.Errorf("with the default config, peer %s trusted=%v, want %v", remote, got, want)
		}
	}
}

func TestBuild_TrustedProxiesMixesPrivateWithOtherCIDRs(t *testing.T) {
	// The list replaces the default wholesale: to add a range on top of the private ones, write
	// private alongside it.
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet, func(c *Config) {
		c.TrustedProxies = []string{"private", "203.0.113.0/24"}
	})).WithRoutes(echoClientIP).Engine()
	for remote, want := range map[string]bool{"10.0.0.5:1": true, "203.0.113.9:1": true, "198.51.100.1:1": false} {
		if got := trusted(e, remote); got != want {
			t.Errorf("peer %s trusted=%v, want %v", remote, got, want)
		}
	}
	// The client_ip side uses the same table.
	if got := clientIPOf(t, e, "203.0.113.9:1"); got != "1.2.3.4" {
		t.Errorf("what a peer inside 203.0.113.0/24 forwards should be believed, got=%q", got)
	}
	// With [] nobody is trusted, forwarded headers included.
	none := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).Engine()
	if trusted(none, "10.0.0.5:1") {
		t.Error("with [] nobody should be trusted")
	}
}

func TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates(t *testing.T) {
	// XEcho.Trace: false governs the span only: an X-Request-Id from a trusted peer and an upstream
	// traceparent are still accepted.
	trusted := probeTrust(t)
	off := func(c *Config) { c.Trace = false }
	e := New().WithConfig(configWith(quiet, off, func(c *Config) {
		c.TrustedProxies = []string{"10.0.0.0/8"}
	})).Engine()
	for remote, want := range map[string]bool{"10.0.0.5:1234": true, "1.2.3.4:1234": false} {
		if got := trusted(e, remote); got != want {
			t.Errorf("with Trace off, peer %s trusted=%v, want %v: the trust rules have to match those with it on", remote, got, want)
		}
	}

	// The upstream trace identifiers reach the request ctx, while this hop opens no span and
	// echoes no X-Trace-Id.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	oldTP := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(oldTP) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const upstream = "4bf92f3577b34da6a3ce929d0e0e4736"
	var seen string
	e = New().WithConfig(configWith(quiet, off)).WithRoutes(func(e *echo.Echo) {
		e.GET("/x", func(c echo.Context) error {
			seen = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
			return nil
		})
	}).Engine()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("traceparent", "00-"+upstream+"-00f067aa0ba902b7-01")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if seen != upstream {
		t.Errorf("with Trace off the upstream trace identifiers should still be joined; the handler saw TraceID=%q", seen)
	}
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Errorf("with Trace off no span should be opened, got=%d", len(spans))
	}
	if w.Header().Get(middleware.TraceIDHeader) != "" {
		t.Error("with Trace off no X-Trace-Id is echoed back")
	}
}

// ---- h2c ----

// h2cClient speaks cleartext HTTP/2 only: it sends the HTTP/2 preface on connect and never goes
// through the HTTP/1.1 upgrade.
func h2cClient() *http.Client {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: tr}
}

// servingH2C starts a server with h2c on, whose /slow sleeps d inside the handler.
func servingH2C(t *testing.T, d time.Duration) (base string, x *XEcho, entered, finished chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	entered, finished = make(chan struct{}), make(chan struct{})
	x = New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.UseH2C = true })).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/ping", ok)
			e.GET("/slow", func(c echo.Context) error {
				close(entered)
				time.Sleep(d)
				close(finished)
				return c.String(200, "done")
			})
		})
	go x.Start(context.Background())
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	return base, x, entered, finished
}

type h2cResult struct {
	proto int
	body  string
	err   error
}

// h2cGet sends one h2c request in the background.
func h2cGet(url string) <-chan h2cResult {
	got := make(chan h2cResult, 1)
	go func() {
		resp, err := h2cClient().Get(url)
		if err != nil {
			got <- h2cResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- h2cResult{proto: resp.ProtoMajor, body: string(b)}
	}()
	return got
}

func TestStop_H2CWaitsForInFlightRequests(t *testing.T) {
	// h2c connections are Shutdown's business too: x/net's h2c.NewHandler hijacks the connection,
	// and Shutdown would return nil in about 60µs while in-flight requests keep running.
	base, x, entered, finished := servingH2C(t, time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := x.Stop(context.Background()); err != nil {
		t.Fatalf("in-flight requests fit inside the budget, so Stop must not fail: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("an h2c request was still in flight when Stop returned - the framework is about to close the database")
	}
	r := <-got
	if r.err != nil || r.body != "done" {
		t.Fatalf("an in-flight request should receive its complete response, got=%+v", r)
	}
	if r.proto != 2 {
		t.Errorf("it should go over HTTP/2, got=HTTP/%d", r.proto)
	}
}

func TestStop_H2CForceClosesAfterTimeout(t *testing.T) {
	base, x, entered, _ := servingH2C(t, 5*time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := stopWithin(t, x); err == nil {
		t.Fatal("in-flight requests outlived the deadline and that should be an error")
	}
	select {
	case r := <-got:
		if r.err == nil {
			t.Errorf("the connection should have been dropped, got=%+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Error("an in-flight h2c request is still running past the timeout")
	}
}

func TestStart_CleartextHTTP2RejectedWithoutH2C(t *testing.T) {
	// A switch has to be a real switch: without UseH2C, a cleartext HTTP/2 preface must not be
	// accepted.
	port := testkit.FreePort(t)
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) { e.GET("/ping", ok) })
	base := serving(t, x, port)

	if r := <-h2cGet(base + "/ping"); r.err == nil {
		t.Errorf("without UseH2C, cleartext HTTP/2 must not succeed, got=HTTP/%d", r.proto)
	}
}

// ---- The zero value ----

func TestZeroValue_ReportsUnderModuleName(t *testing.T) {
	// &XEcho{} has to work without New: errors and logs are still recorded under xecho, so a
	// caller's xerror.Is(err, "xecho") holds and the log never shows a headless " listening".
	buf := captureLog(t)
	cert := filepath.Join(t.TempDir(), "missing.pem")
	err := startErr(t, (&XEcho{}).WithConfig(configWith(quiet, on(testkit.FreePort(t)), func(c *Config) { c.TLS.CertFile, c.TLS.KeyFile = cert, cert })))
	if !xerror.Is(err, "xecho") || xerror.Module(err) != "xecho" {
		t.Fatalf("the error from a zero value Start should belong to xecho, got=%v", err)
	}

	z := &XEcho{}
	if err := z.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start should do nothing, got=%v", err)
	}
	if err := z.Start(context.Background()); err != nil {
		t.Fatalf("Start after Stop should return nil immediately, got=%v", err)
	}
	if !strings.Contains(buf.String(), `"msg":"xecho received the shutdown signal before starting`) {
		t.Errorf("the log message should start with xecho, got=%s", buf.String())
	}
}

func TestStart_NoListeningLogWhenListenFails(t *testing.T) {
	// "listening" is only printed once it really listens: saying listening first and reporting
	// listen failed afterwards (an unreadable certificate, a taken port) makes whoever is
	// debugging believe the server was up.
	cert := filepath.Join(t.TempDir(), "missing.pem")
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for name, mutate := range map[string]func(*Config){
		"missing cert": func(c *Config) { c.Port = testkit.FreePort(t); c.TLS.CertFile, c.TLS.KeyFile = cert, cert },
		"port in use":  func(c *Config) { c.Port = busy.Addr().(*net.TCPAddr).Port },
	} {
		buf := captureLog(t)
		err := startErr(t, New().WithConfig(configWith(quiet, on(0), mutate)))
		if err == nil || !strings.Contains(err.Error(), "listen on") {
			t.Fatalf("%s: listen failed is expected, got=%v", name, err)
		}
		if strings.Contains(buf.String(), "xecho listening") {
			t.Errorf("%s: listening must not be printed when nothing is listening, got=%s", name, buf.String())
		}
	}
}

func TestLoadConfig_IPv4MappedProxyFailsStartup(t *testing.T) {
	// ::ffff:10.0.0.1 means one thing to gin and another to xtrace's trust check, so it can only
	// be rejected, with a message saying how to write it.
	err := loadErr(t, "XEcho:\n  TrustedProxies: [private, \"::ffff:10.0.0.1\"]\n")
	if err == nil || !strings.Contains(err.Error(), "IPv4-mapped") || !strings.Contains(err.Error(), "write it as 10.0.0.1") {
		t.Fatalf("the IPv4-mapped spelling should fail startup and give the IPv4 spelling, got=%v", err)
	}
}

// ---- e.Pre ----

// preEcho assembles a server with an e.Pre, recording traces into the returned exporter and
// metrics into the returned Metrics.
func preEcho(t *testing.T, pre ...echo.MiddlewareFunc) (*echo.Echo, *tracetest.InMemoryExporter, *xmetric.Metrics) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	m.Install()

	e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.Pre(pre...)
		e.GET("/a", func(c echo.Context) error { return c.String(200, "a") })
	}).Engine()
	return e, exp, m
}

func TestBuild_PreRejectionLoggedMeasuredAndTraced(t *testing.T) {
	// Regression: the built-in middleware used to sit on e.Use, so a request rejected in e.Pre
	// (where auth and rate limiting tend to live) reached neither the access log, the metrics nor
	// the trace, and the response carried no X-Trace-Id. The router has not run, so the route is
	// recorded as unmatched.
	buf := captureLog(t)
	e, exp, m := preEcho(t, func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { return echo.ErrUnauthorized }
	})
	w := doRequest(t, e, "GET", "/a")
	if w.Code != 401 || w.Header().Get(middleware.TraceIDHeader) == "" {
		t.Errorf("it should answer 401 and carry X-Trace-Id, got=%d %v", w.Code, w.Header())
	}
	if out := buf.String(); !strings.Contains(out, `"msg":"request completed"`) || !strings.Contains(out, `"route":"unmatched"`) || !strings.Contains(out, `"status":401`) {
		t.Errorf("the access log should record 401 and unmatched:\n%s", out)
	}
	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `http_requests_total{method="GET",route="unmatched",status="401"} 1`) {
		t.Errorf("the metrics should record 401\n%s", out)
	}
	if spans := exp.GetSpans(); len(spans) != 1 || spans[0].Name != "GET unmatched" {
		t.Errorf("one GET unmatched span is expected, got=%v", spans)
	}
}

func TestBuild_PreRedirectLogged(t *testing.T) {
	// The spelling from echo's own documentation: a trailing slash 301 in Pre. That 301 is a
	// request too.
	buf := captureLog(t)
	e, _, _ := preEcho(t, echomw.RemoveTrailingSlashWithConfig(echomw.TrailingSlashConfig{RedirectCode: http.StatusMovedPermanently}))
	w := doRequest(t, e, "GET", "/a/")
	if w.Code != 301 || !strings.Contains(buf.String(), `"status":301`) {
		t.Errorf("a 301 from Pre should reach the access log, got=%d\n%s", w.Code, buf.String())
	}
}

func TestBuild_PrePanicRecovered(t *testing.T) {
	// Echo does not catch panics: a panic in Pre used to be caught by nobody, the client read EOF
	// (an empty reply) and the stack went to stderr through net/http.
	buf := captureLog(t)
	e, _, _ := preEcho(t, func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { panic("pre boom") }
	})
	w := doRequest(t, e, "GET", "/a")
	out := buf.String()
	if w.Code != 500 || !strings.Contains(out, `"msg":"panic while handling request"`) || !strings.Contains(out, `"status":500`) {
		t.Errorf("a panic in Pre should be caught, answer 500 and record an ERROR log and an access log, got=%d\n%s", w.Code, out)
	}
}

func TestBuild_PreMethodOverrideStillRoutes(t *testing.T) {
	// Echo's router matches on the *http.Request as it arrived (captured in the closure inside
	// Echo.ServeHTTP), not on c.Request(). If the built-in middleware swapped in an r.WithContext
	// copy in Pre, a MethodOverride placed after it would change the Method on the copy and the
	// router would still match POST. With Trace off it is Propagate that swaps the ctx, and it has
	// to swap in place just the same.
	// traceOn, not trace: the name trace is the imported otel package here.
	for _, traceOn := range []bool{true, false} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(c *Config) { c.Trace = traceOn })).WithRoutes(func(e *echo.Echo) {
			e.Pre(echomw.MethodOverride())
			e.POST("/a", func(c echo.Context) error { return c.String(200, "post") })
			e.PUT("/a", func(c echo.Context) error { return c.String(200, "put") })
		}).Engine()
		req := httptest.NewRequest("POST", "/a", nil)
		req.Header.Set(echo.HeaderXHTTPMethodOverride, "PUT")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Code != 200 || w.Body.String() != "put" {
			t.Errorf("Trace=%v: MethodOverride should route the request to PUT, got=%d %q", traceOn, w.Code, w.Body.String())
		}
		if out := buf.String(); !strings.Contains(out, `"method":"PUT","route":"/a"`) {
			t.Errorf("Trace=%v: the access log should record the rewritten method and route:\n%s", traceOn, out)
		}
	}
}

func TestBuild_RouteTemplateKnownWithBuiltinsInPre(t *testing.T) {
	// The built-in middleware sits on Pre with the router inside it: c.Path() is set by the time
	// next(c) returns.
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	buf := captureLog(t)
	e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.GET("/users/:id", ok)
	}).Engine()
	doRequest(t, e, "GET", "/users/1")
	if !strings.Contains(buf.String(), `"route":"/users/:id"`) {
		t.Errorf("the access log should record the route template:\n%s", buf.String())
	}
	if spans := exp.GetSpans(); len(spans) != 1 || spans[0].Name != "GET /users/:id" {
		t.Errorf("the span name should carry the route template, got=%v", spans)
	}
}

func TestEchoStdLogger_DefaultWritesToStdout(t *testing.T) {
	// Pins the premise (echo v4.16.0): echo.New binds e.StdLogger to e.Logger's output at that
	// moment, os.Stdout, and a later e.Logger.SetOutput does not reach it.
	if out := echo.New().StdLogger.Writer(); out != os.Stdout {
		t.Errorf("e.StdLogger writes to os.Stdout by default, got=%T", out)
	}
}

func TestEchoStdLogger_RoutedToSlog(t *testing.T) {
	// e.StdLogger is what echo prepares for http.Server.ErrorLog (e.Start uses it that way): route
	// it where the server's ErrorLog goes.
	buf := captureLog(t)
	e := New().WithConfig(configWith(quiet)).Engine()
	e.StdLogger.Print("http: TLS handshake error from 203.0.113.9:1234: EOF")
	if want := `"level":"WARN","msg":"xecho http server error","error":"http: TLS handshake error from 203.0.113.9:1234: EOF"`; !strings.Contains(buf.String(), want) {
		t.Errorf("e.StdLogger should turn into %s, got=%s", want, buf.String())
	}
}

func TestBuild_ContextTimeoutRecordedAsSent(t *testing.T) {
	// The timeout spelling the README recommends: echomw.ContextTimeout puts a deadline on the
	// request, the handler returns on the ctx and it turns that into a 503. It cancels that ctx in
	// a defer when it returns, so outside its layer the request ctx reads as Canceled, which must
	// not be taken for a client that left (499). Echo's deprecated middleware.Timeout records it
	// wrongly, see the README. With metrics on the innermost layer rendering the error is Metric
	// and with them off it is Log: both layers have to be right.
	for _, metric := range []bool{true, false} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(c *Config) { c.Metric = metric })).
			WithMiddleware(echomw.ContextTimeout(20 * time.Millisecond)).
			WithRoutes(func(e *echo.Echo) {
				e.GET("/slow", func(c echo.Context) error {
					<-c.Request().Context().Done()
					return c.Request().Context().Err()
				})
			}).Engine()
		w := doRequest(t, e, "GET", "/slow")
		if w.Code != 503 || !strings.Contains(buf.String(), `"status":503`) || !strings.Contains(buf.String(), fmt.Sprintf(`"bytes_out":%d`, w.Body.Len())) {
			t.Errorf("Metric=%v: the client received %d and the access log should record the same status and byte count:\n%s", metric, w.Code, buf.String())
		}
	}
}

// xlogCapture replaces the default logger with a real xlog (trace_id and the xlog.AddKV fields
// are added by its handler) and returns a function yielding the log lines.
func xlogCapture(t *testing.T) func() string {
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
	t.Cleanup(func() { xlog.SetTraceExtractor(nil); slog.SetDefault(old); closer.Close() })
	return func() string {
		closer.Close()
		files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
		if len(files) == 0 {
			return ""
		}
		b, _ := os.ReadFile(files[0])
		return string(b)
	}
}

func TestBuild_PreRejectionAccessLogCarriesTraceID(t *testing.T) {
	// For a request that ended inside Pre, the log scope and the span have not been swapped onto
	// the request yet (that happens after routing): the access log uses the ctx stored on
	// echo.Context, so it still carries trace_id, matching X-Trace-Id.
	logs := xlogCapture(t)
	e, _, _ := preEcho(t, func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { return echo.ErrUnauthorized }
	})
	w := doRequest(t, e, "GET", "/a")
	id := w.Header().Get(middleware.TraceIDHeader)
	if out := logs(); id == "" || !strings.Contains(out, `"trace_id":"`+id+`"`) || !strings.Contains(out, `"status":401`) {
		t.Errorf("the access log of the 401 should carry trace_id=%q:\n%s", id, out)
	}
}

func TestBuild_HandlerSeesSpanAndLogScope(t *testing.T) {
	// After routing, xecho swaps the ctx built in Pre onto the request: the handler and
	// WithMiddleware see a ctx carrying the server span, and fields written with xlog.AddKV appear
	// in the access log.
	logs := xlogCapture(t)
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	var inMiddleware, inHandler string
	e := New().WithConfig(configWith(func(c *Config) { c.Metric = false })).
		WithMiddleware(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				inMiddleware = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
				return next(c)
			}
		}).
		WithRoutes(func(e *echo.Echo) {
			e.GET("/a", func(c echo.Context) error {
				inHandler = trace.SpanContextFromContext(c.Request().Context()).TraceID().String()
				xlog.AddKV(c.Request().Context(), "user_id", "u9")
				return c.NoContent(200)
			})
		}).Engine()
	w := doRequest(t, e, "GET", "/a")
	id := w.Header().Get(middleware.TraceIDHeader)
	if id == "" || inMiddleware != id || inHandler != id {
		t.Errorf("WithMiddleware and the handler should both see the server span, X-Trace-Id=%q middleware=%q handler=%q", id, inMiddleware, inHandler)
	}
	if out := logs(); !strings.Contains(out, `"user_id":"u9"`) {
		t.Errorf("the xlog.AddKV fields should reach the access log:\n%s", out)
	}
}

func TestBuild_PreRewritingMiddlewaresStillRoute(t *testing.T) {
	// The caller's e.Pre changes the request echo matches on (Method, URL.Path): the built-in
	// middleware must not replace it.
	for name, c := range map[string]struct {
		pre    echo.MiddlewareFunc
		method string
		path   string
		header string
	}{
		"MethodOverride":      {echomw.MethodOverride(), "POST", "/a", "PUT"},
		"RemoveTrailingSlash": {echomw.RemoveTrailingSlash(), "PUT", "/a/", ""},
		"Rewrite":             {echomw.Rewrite(map[string]string{"/old": "/a"}), "PUT", "/old", ""},
	} {
		e := New().WithConfig(configWith(quiet)).WithRoutes(func(e *echo.Echo) {
			e.Pre(c.pre)
			e.PUT("/a", func(c echo.Context) error { return c.String(200, "put") })
		}).Engine()
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.header != "" {
			req.Header.Set(echo.HeaderXHTTPMethodOverride, c.header)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Code != 200 || w.Body.String() != "put" {
			t.Errorf("%s: it should route to PUT /a, got=%d %q", name, w.Code, w.Body.String())
		}
	}
}

func TestHandler_RequestUsedAfterReturnIsRaceFree(t *testing.T) {
	// Regression (-race): handing the request to a goroutine that outlives the handler (async
	// logging, instrumentation) is a common thing to do. If the built-in middleware mutated the
	// *http.Request handed in, this would be a data race.
	var wg sync.WaitGroup
	e := New().WithConfig(DefaultConfig()).WithRoutes(func(e *echo.Echo) {
		e.GET("/async", func(c echo.Context) error {
			r := c.Request()
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(5 * time.Millisecond)
				_ = r.Context().Value("k")
				_ = r.Method
			}()
			return c.String(200, "ok")
		})
	}).Engine()
	for range 20 {
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/async", nil))
	}
	wg.Wait()
}

// servingHijacked starts a server whose /ws hijacks the connection and then waits on the request
// ctx, which is what a WebSocket read loop does.
func servingHijacked(t *testing.T) *XEcho {
	t.Helper()
	port := testkit.FreePort(t)
	entered, release := make(chan struct{}), make(chan struct{})
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) {
		e.GET("/ping", ok)
		e.GET("/ws", func(c echo.Context) error {
			conn, _, err := c.Response().Hijack()
			if err != nil {
				return err
			}
			defer conn.Close()
			close(entered)
			select {
			case <-c.Request().Context().Done():
			case <-release: // also lets the Stop in Cleanup return when the test fails
			}
			return nil
		})
	})
	base := serving(t, x, port)
	t.Cleanup(func() { close(release) }) // runs before serving's Stop
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: x\r\n\r\n")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the request never reached the handler")
	}
	return x
}

func TestStop_HijackedConnGetsCancelled(t *testing.T) {
	// Regression. Shutdown does not wait for a hijacked connection (a WebSocket) and Close does
	// not drop it either, so the request ctx used to never be cancelled: the handler waiting on it
	// never returned and Stop burned the whole budget before reporting 1 handler(s) still running.
	x := servingHijacked(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := x.Stop(ctx); err != nil {
		t.Fatalf("the hijacked handler returned once it saw the ctx cancelled, so Stop must not fail: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop should cancel the hijacked connection's ctx right after Shutdown returns, but took %v", d)
	}
}

func TestStart_RetryAfterListenFailure(t *testing.T) {
	// Regression. After a failed listen (a taken port) the server used to still consider itself
	// running, and every later Start said already running.
	port := testkit.FreePort(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	x := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *echo.Echo) { e.GET("/ping", ok) })
	if err := startErr(t, x); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("with the port taken, a listen failure is expected, got=%v", err)
	}
	ln.Close()

	done := make(chan error, 1)
	go func() { done <- x.Start(context.Background()) }()
	t.Cleanup(func() { _ = x.Stop(context.Background()) })
	select {
	case err := <-done:
		t.Fatalf("once the port is free it should start again, got=%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/ping", port))
}

func TestTrace_MetricsScrapeNotTraced(t *testing.T) {
	// Regression. The scraper polls /metrics every few seconds; the access log had long skipped it
	// while a span was still opened every time.
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	e := New().WithConfig(configWith(func(c *Config) { c.Log = false; c.MetricPath = "/internal/metrics" })).Engine()
	if w := doRequest(t, e, "GET", "/internal/metrics"); w.Code != 200 {
		t.Fatalf("the metrics endpoint should answer 200, got=%d", w.Code)
	}
	if n := len(exp.GetSpans()); n != 0 {
		t.Errorf("one metrics scrape should open no span, got %d", n)
	}
	doRequest(t, e, "GET", "/work")
	if n := len(exp.GetSpans()); n != 1 {
		t.Errorf("other paths still open spans, got %d", n)
	}
}
