package xgin

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
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xgin/middleware"
	"github.com/jupiter-eng/xone/xgin/trans"
	"github.com/jupiter-eng/xone/xlog"
	"github.com/jupiter-eng/xone/xmetric"
)

func init() { gin.SetMode(gin.TestMode) }

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
func serving(t *testing.T, g *XGin, port int) string {
	t.Helper()
	go func() { _ = g.Start(context.Background()) }()
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/nothing")
	return base
}

// startErr calls Start and takes the error it returns. With an invalid config it should return
// immediately and never listen - after 2s without returning it is taken to be listening, which
// stops it and fails the test.
func startErr(t *testing.T, g *XGin) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = g.Stop(context.Background())
		t.Fatal("with an invalid config Start should return an error, but it started listening")
		return nil
	}
}

// echoClientIP registers GET /client-ip, whose body is the ClientIP the handler sees.
func echoClientIP(e *gin.Engine) {
	e.GET("/client-ip", func(c *gin.Context) { c.String(200, c.ClientIP()) })
}

// clientIPOf requests /client-ip once from remote with a forged X-Forwarded-For: 1.2.3.4 (see
// echoClientIP) and returns the ClientIP the handler saw.
func clientIPOf(t *testing.T, e *gin.Engine, remote string) string {
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
	if c.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("reading headers must have a timeout, got=%v", c.ReadHeaderTimeout)
	}
	// Forgetting the environment variable in production costs far more than one missing hint locally.
	if c.Mode != "release" {
		t.Errorf("the default should be release, got=%q", c.Mode)
	}
	if !c.Log || !c.Trace || !c.Metric {
		t.Errorf("the three built-in middlewares should all be on by default, got=%+v", c)
	}
	if c.LogRequestBody || c.LogResponseBody || c.LogQuery || c.LogRequestHeaders || c.LogResponseHeaders || c.ZHTranslations {
		t.Errorf("recording bodies, query strings, request and response headers, and Chinese translation should all be off by default (each costs and risks enough), got=%+v", c)
	}
	if c.MetricPath != "/metrics" {
		t.Errorf("the metrics path should default to /metrics, got=%q", c.MetricPath)
	}
	if len(c.LogSkipPaths) != 0 {
		t.Errorf("no path is skipped by default, got=%v", c.LogSkipPaths)
	}
}

func TestConfig_LoadFromFile(t *testing.T) {
	c := load(t, "XGin:\n  Port: 9090\n  ReadTimeout: 30s\n  UseH2C: true\n  Log: false\n  LogSkipPaths: [/healthz, /static/]\n")
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
	if err := loadErr(t, "XGin:\n  Prot: 9090\n"); err == nil {
		t.Fatal("a misspelled field should fail startup")
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
		"unknown Mode":                   func(c *Config) { c.Mode = "prod" },
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
	// default": a ReadHeaderTimeout of 0 falls back to ReadTimeout, whose default is also 0, so the
	// main defense against slow-connection attacks disappears entirely while the config file merely
	// reads 0.
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

func TestNetHTTP_ReadHeaderTimeoutZeroLetsSlowClientHoldConn(t *testing.T) {
	// Pins the premise of the validation above: the standard library treats 0 as "no limit". The
	// day an upgrade turns it into "use some default", this goes red first and the validation can
	// be relaxed accordingly.
	for _, rht := range []time.Duration{0, -time.Second} {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Config.ReadHeaderTimeout = rht
		srv.Start()

		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // only half the headers
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, err = conn.Read(make([]byte, 1))
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("with ReadHeaderTimeout=%v the server should keep waiting (our side times out reading), got=%v", rht, err)
		}
		conn.Close()
		srv.Close()
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
	// gin's SetTrustedProxies parses until the first error and keeps whatever it already parsed, so
	// the first half of the proxies is trusted while the second is quietly dropped — client_ip in
	// the log is then half true and half false, which is far harder to track down than not starting.
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

func TestBuild_BuiltinMiddlewareOrder(t *testing.T) {
	// Recover has to be the innermost of the built-ins: whichever layer catches a panic, the code
	// after c.Next() in every layer inside it never runs.
	g := New().WithConfig(DefaultConfig()).WithRoutes(func(e *gin.Engine) {
		e.GET("/boom", func(c *gin.Context) { panic("boom") })
	})

	w := doRequest(t, g.Engine(), "GET", "/boom")
	if w.Code != 500 {
		t.Errorf("the panic should be caught and a 500 returned, got=%d", w.Code)
	}
}

func TestBuild_MetricsEndpointAutoRegistered(t *testing.T) {
	g := New().WithConfig(DefaultConfig())
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code != 200 {
		t.Errorf("with metrics on, /metrics should be registered automatically, got=%d", w.Code)
	}
	if !strings.Contains(doRequest(t, g.Engine(), "GET", "/metrics").Body.String(), "http_requests_total") {
		t.Error("the metrics endpoint should export the request count metric")
	}
}

func TestBuild_MetricsPathConfigurable(t *testing.T) {
	g := New().WithConfig(configWith(func(c *Config) { c.MetricPath = "/internal/metrics" }))
	if w := doRequest(t, g.Engine(), "GET", "/internal/metrics"); w.Code != 200 {
		t.Errorf("it should be registered on the configured path, got=%d", w.Code)
	}
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("nothing should remain on the default path")
	}
}

func TestBuild_NoMetricsEndpointWhenDisabled(t *testing.T) {
	g := New().WithConfig(configWith(func(c *Config) { c.Metric = false }))
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("with metrics off there should be no /metrics")
	}
}

func TestBuild_MetricPathTakenByTheApplicationFailsStartup(t *testing.T) {
	// Registered before the caller's routes, the framework's endpoint made gin panic on the
	// duplicate path -- out of the caller's own routing code, with nothing in the message pointing
	// at MetricPath. It is a startup error now, the sentence xecho and xbeego report too.
	buf := captureLog(t)
	g := New().WithConfig(configWith(func(c *Config) { c.Log = false })).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/metrics", func(c *gin.Context) { c.String(200, "mine") })
		})

	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Body.String() != "mine" {
		t.Errorf("the caller's own route should be the one left standing, got=%q", w.Body.String())
	}
	err := startErr(t, g)
	if !xerror.Is(err, "xgin") || !strings.Contains(err.Error(), "MetricPath") {
		t.Fatalf("Start should report the collision and name MetricPath, got=%v", err)
	}
	// A caller who only ever uses Engine() never sees that error, so the warning has to stand on
	// its own.
	if !strings.Contains(buf.String(), "MetricPath") {
		t.Errorf("assembly should warn about the collision, got=%s", buf.String())
	}
}

func TestBuild_MetricPathTakenCountsAnyMethod(t *testing.T) {
	// The access log and the tracing leave MetricPath out by path alone, so a caller's POST on it
	// is just as invisible as a GET -- and the scraper's GET would answer 405 besides.
	_ = captureLog(t)
	g := New().WithConfig(configWith(func(c *Config) { c.Log = false })).
		WithRoutes(func(e *gin.Engine) {
			e.POST("/metrics", func(c *gin.Context) { c.Status(204) })
		})
	g.Engine()
	if g.confErr == nil || !strings.Contains(g.confErr.Error(), "MetricPath") {
		t.Fatalf("a route on MetricPath under another method is a collision too, got=%v", g.confErr)
	}
}

func TestBuild_405InsteadOf404(t *testing.T) {
	// Without HandleMethodNotAllowed, a wrong method yields a 404 and the caller believes the path
	// was misspelled.
	g := New().WithConfig(DefaultConfig()).WithRoutes(func(e *gin.Engine) {
		e.GET("/only-get", func(c *gin.Context) { c.Status(200) })
	})
	if w := doRequest(t, g.Engine(), "POST", "/only-get"); w.Code != 405 {
		t.Errorf("a wrong method should answer 405, got=%d", w.Code)
	}
}

func TestBuild_Idempotent(t *testing.T) {
	// Assembling twice registers the middleware twice, which shows up as two log lines per request
	// and doubled metrics.
	g := New().WithConfig(DefaultConfig())
	if g.Engine() != g.Engine() {
		t.Error("assembling again should return the same engine")
	}
}

func TestBuild_UserMiddlewareAfterBuiltin(t *testing.T) {
	var order []string
	g := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithMiddleware(func(c *gin.Context) { order = append(order, "user"); c.Next() }).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/x", func(c *gin.Context) { order = append(order, "handler"); c.Status(200) })
		})

	doRequest(t, g.Engine(), "GET", "/x")
	if len(order) != 2 || order[0] != "user" || order[1] != "handler" {
		t.Errorf("user middleware should run before the handler, got=%v", order)
	}
}

func TestBuild_ModeSetFromConfigBeforeEngine(t *testing.T) {
	var out bytes.Buffer
	oldW := gin.DefaultWriter
	gin.DefaultWriter = &out
	t.Cleanup(func() { gin.DefaultWriter = oldW; gin.SetMode(gin.TestMode) })

	New().WithConfig(configWith(quiet, func(c *Config) { c.Mode = "debug" })).Engine()
	if gin.Mode() != gin.DebugMode {
		t.Errorf("assembly should set the Mode from the config, got=%q", gin.Mode())
	}

	// Without GIN_MODE set in the caller's binary, gin defaults to debug. The Mode has to be set
	// before gin.New, or a server configured as release still prints gin's "running in debug mode"
	// warning.
	gin.SetMode(gin.DebugMode)
	out.Reset()
	New().WithConfig(DefaultConfig()).Engine()
	if out.Len() > 0 {
		t.Errorf("a server configured as release should produce no gin debug output:\n%s", out.String())
	}
}

// ---- Switches ----

func TestLog_NoAccessLogWhenDisabled(t *testing.T) {
	buf := captureLog(t)
	route := func(e *gin.Engine) { e.GET("/work", func(c *gin.Context) { c.String(200, "ok") }) }

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

func TestLogSkipPaths_ReallySkipsThesePaths(t *testing.T) {
	// Writing it in the config is not enough: it has to travel all the way into middleware.Log.
	buf := captureLog(t)
	e := New().WithConfig(configWith(func(c *Config) {
		c.Metric, c.Trace = false, false
		c.LogSkipPaths = []string{"/healthz", "/static/"}
	})).WithRoutes(func(e *gin.Engine) {
		e.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
		e.GET("/static/*file", func(c *gin.Context) { c.String(200, "ok") })
		e.GET("/work", func(c *gin.Context) { c.String(200, "ok") })
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
		})).WithRoutes(func(e *gin.Engine) {
			e.POST("/echo", func(c *gin.Context) { c.JSON(200, gin.H{"greeting": "hi-bob"}) })
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
		})).WithRoutes(func(e *gin.Engine) {
			e.GET("/search", func(c *gin.Context) {
				c.Header("X-Page-Total", "7")
				c.Header("Set-Cookie", "sid=abc123")
				c.Status(200)
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

	for _, trace := range []bool{true, false} {
		exp.Reset()
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.Trace = trace })).Engine()
		w := doRequest(t, e, "GET", "/nope")

		if got := len(exp.GetSpans()) > 0; got != trace {
			t.Errorf("with Trace=%v a span was opened=%v", trace, got)
		}
		if got := w.Header().Get(middleware.TraceIDHeader) != ""; got != trace {
			t.Errorf("with Trace=%v the response carried %s=%v", trace, middleware.TraceIDHeader, got)
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
			WithRoutes(func(e *gin.Engine) { e.GET(route, func(c *gin.Context) { c.Status(200) }) }).
			Engine()
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", route, nil))

		w := httptest.NewRecorder()
		m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if got := strings.Contains(w.Body.String(), `route="`+route+`"`); got != metric {
			t.Errorf("with Metric=%v request metrics were recorded=%v\n%s", metric, got, w.Body.String())
		}
	}
}

func TestZHTranslations_InstallsChineseTranslations(t *testing.T) {
	New().WithConfig(configWith(quiet, func(c *Config) { c.Trace, c.ZHTranslations = false, true })).Engine()

	var req struct {
		Name string `binding:"required"`
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	err := c.ShouldBindJSON(&req)
	if err == nil {
		t.Fatal("a missing required field should fail validation")
	}
	if got := trans.Msg(err); !containsHan(got) {
		t.Errorf("it was not translated into Chinese, got=%q", got)
	}
}

func TestWithRecoverFunc_ReplacesPanicResponse(t *testing.T) {
	g := New().WithConfig(configWith(func(c *Config) { c.Log, c.Trace, c.Metric = false, false, false })).
		WithRecoverFunc(func(c *gin.Context, _ any) { c.String(503, "custom") }).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/boom", func(*gin.Context) { panic("boom") })
		})

	w := httptest.NewRecorder()
	g.Engine().ServeHTTP(w, httptest.NewRequest("GET", "/boom", nil))
	if w.Code != 503 || w.Body.String() != "custom" {
		t.Errorf("the custom recover did not take effect, got=%d %q", w.Code, w.Body.String())
	}
}

// ---- The config landing on the engine ----

func TestBuild_TrustsOnlyPrivateProxiesByDefault(t *testing.T) {
	// gin's own default is trustedProxies = 0.0.0.0/0 + ::/0, that is, trust everyone. That means
	// anyone sending an X-Forwarded-For decides what client_ip reads in the access log — the log
	// can be forged, and the rate limiting and auditing built on that field fail with it. Only
	// private ranges are trusted by default: what a load balancer or an ingress forwards is
	// accepted, a public peer's is not.
	e := New().WithConfig(configWith(quiet)).WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("a public peer's X-Forwarded-For must not be believed; client_ip should be the peer address itself, got=%q (the header forged 1.2.3.4)", got)
	}
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("an X-Forwarded-For from a private-range peer (a load balancer) should be believed, got=%q", got)
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
	e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8"} })).
		WithRoutes(echoClientIP).Engine()
	if got := clientIPOf(t, e, "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("with the peer inside a trusted range, the forwarded address should be believed, got=%q", got)
	}
}

func TestBuild_MultipartMemoryThresholdFromConfig(t *testing.T) {
	// gin defaults to 32MB, and that number is not a request body limit but the threshold above
	// which data spills to disk; the real cost is about three times it: a 60MB upload at 32MB makes
	// the heap grow by 96MB in that parsing step, which is two gigabytes across twenty concurrent
	// uploads.
	g := New().WithConfig(configWith(quiet, func(c *Config) { c.MaxMultipartMemory = 2 << 20 }))
	if got := g.Engine().MaxMultipartMemory; got != 2<<20 {
		t.Errorf("the threshold from the config should be used, got=%d want=%d", got, 2<<20)
	}
}

func TestBuild_CallbackEngineSettingsOverrideConfig(t *testing.T) {
	// The callback runs after the config has landed on the engine: what the caller set explicitly
	// in code wins. It used to be the other way round — Start applied the config again and quietly
	// overwrote what the callback had set. So this checks again with the server running: Start must
	// not apply the config a second time.
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.MaxMultipartMemory = 2 << 20 })).
		WithRoutes(echoClientIP, func(e *gin.Engine) {
			e.MaxMultipartMemory = 64 << 20
			if err := e.SetTrustedProxies([]string{"10.0.0.0/8"}); err != nil {
				t.Error(err)
			}
		})
	g.Engine() // assemble on the test goroutine: a t.Error inside the callback must not run on another one
	serving(t, g, port)

	if got := g.Engine().MaxMultipartMemory; got != 64<<20 {
		t.Errorf("what the callback set should win, got=%d", got)
	}
	if got := clientIPOf(t, g.Engine(), "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("the callback trusted 10.0.0.0/8, so client_ip should follow the forwarding header, got=%q", got)
	}
}

// ---- Start and stop ----

func TestStartStop_GracefulShutdown(t *testing.T) {
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.String(200, "pong") })
	})

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/ping", port)
	waitServing(t, url)

	resp, err := oneShot.Get(url)
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pong" {
		t.Errorf("the response is wrong, got=%s", body)
	}

	if err := g.Stop(context.Background()); err != nil {
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

func TestStart_DoesNotListenOnInvalidConfig(t *testing.T) {
	err := startErr(t, New().WithConfig(configWith(func(c *Config) { c.TLS.CertFile = "half configured" })))

	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xgin" || xe.Op != "config" {
		t.Fatalf("this should be an xgin config error, got=%v", err)
	}
	// A process may hold two servers: the error has to say it is the WithConfig one, and which
	// field it is.
	if !strings.Contains(err.Error(), "WithConfig") || !strings.Contains(err.Error(), "TLS.CertFile") {
		t.Errorf("the error should say which config and which field, got=%v", err)
	}
}

func TestStart_UsesConfigFromBuildTime(t *testing.T) {
	// The listen address, the timeouts and the middleware and trusted proxies on the engine must
	// come from one config. Start reading it again could disagree with assembly — this creates that
	// situation with a "WithConfig after assembly", which has no effect, so the port must not
	// change either.
	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(portA)))
	g.Engine()
	g.WithConfig(configWith(quiet, on(portB)))

	serving(t, g, portA)
}

func TestStart_SignalArrivesBeforeStart(t *testing.T) {
	// Listening as usual would bring the server up after the stop signal has already arrived, and
	// then keep it running until the framework times out.
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port)))
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start must not fail: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("it should return immediately, got=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start after Stop must not actually listen")
	}

	// Confirm nothing is really listening on the port.
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("the server must not come up")
	}
}

func TestStart_RepeatedStartFails(t *testing.T) {
	port := testkit.FreePort(t)
	g := New().WithConfig(configWith(quiet, on(port)))
	serving(t, g, port)

	if err := g.Start(context.Background()); err == nil {
		t.Error("starting twice should fail")
	}
}

func TestStop_SafeWithoutStart(t *testing.T) {
	if err := New().Stop(context.Background()); err != nil {
		t.Errorf("Stop without a Start must not fail: %v", err)
	}
}

// servingWithHungRequest starts a server and leaves one request hanging inside the handler, so
// that Shutdown has to wait for it — which is what makes the waiting measurable.
func servingWithHungRequest(t *testing.T) *XGin {
	g, _ := servingWithHungRequestDone(t)
	return g
}

// servingWithHungRequestDone does the same and additionally returns a channel closed when that
// hanging request finishes — the only way to see whether the forced disconnect took effect.
func servingWithHungRequestDone(t *testing.T) (*XGin, <-chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
		e.GET("/hang", func(c *gin.Context) { <-release; c.Status(200) })
	})
	go g.Start(context.Background())
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
	return g, hung
}

// stopWithin calls Stop with a 200ms deadline and fails if it has not returned within 3s.
// It runs on another goroutine: if Stop ignored the deadline it would hang on that request, and
// the test must not hang with it.
func stopWithin(t *testing.T, g *XGin) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- g.Stop(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Stop should give up at the caller's deadline; 3s later it still has not returned")
		return nil
	}
}

func TestStop_HonorsCallerDeadline(t *testing.T) {
	// Regression. How long it waits comes from the caller's ctx alone, and xone.Run passes the
	// server's share of the stop budget. This used to be replaced by context.WithoutCancel, which
	// made it wait out its own limit in full — and "every component shares one budget" became an
	// empty phrase.
	g := servingWithHungRequest(t)
	if err := stopWithin(t, g); err == nil {
		t.Error("in-flight requests outlived the deadline, which should be reported faithfully")
	}
}

func TestStop_ForceClosesInFlightConnsAfterTimeout(t *testing.T) {
	// A Shutdown timeout only returns an error and touches no connection. Left at that, the
	// handlers keep running while the framework goes on to close the database and the cache — those
	// requests then find closed pools.
	//
	// Checking whether the port still accepts connections proves nothing: Shutdown closes the
	// listener on entry, so an unreachable port looks the same either way. What matters is whether
	// that in-flight request was actually dropped.
	g, hung := servingWithHungRequestDone(t)
	if err := stopWithin(t, g); err == nil {
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
	// running. It used to return right after Close, with a handler that saw the cancel and was
	// finishing up still running, and the framework went on to close the database. The deadline
	// must not be consumed entirely by Shutdown either: the cleanup after the disconnect needs time.
	port := testkit.FreePort(t)
	var returned atomic.Bool
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
		e.GET("/hang", func(c *gin.Context) {
			<-c.Request.Context().Done()
			time.Sleep(50 * time.Millisecond) // finishing up: rolling back, writing an audit entry...
			returned.Store(true)
		})
	})
	go g.Start(context.Background())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	go func() {
		if resp, err := http.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // wait until the request really reached the handler

	// 20% of the budget is reserved for the cleanup after the disconnect (shutdownCtx): a 2s budget
	// leaves 400ms, eight times the 50ms of cleanup here. It used to be 500ms with only 100ms left:
	// under -race, with other packages running in parallel, one slow scheduling moment was enough,
	// and -count=3 failed twice out of three. Measured under normal load, Stop returns about 50ms
	// after Shutdown's deadline; the rest is headroom for scheduling jitter.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := g.Stop(ctx)
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
	g := servingWithHungRequest(t)
	err := stopWithin(t, g)
	if err == nil || !strings.Contains(err.Error(), "1 handler(s) still running") {
		t.Errorf("it should report that 1 handler has not returned, got=%v", err)
	}
}

// ---- Reading the config file ----

func TestEngine_UsesFinalConfigEvenBeforeRun(t *testing.T) {
	// The caller builds an XGin at the top of main and calls Engine() (to mount Swagger, say)
	// before xone.Run. The config used not to be loaded by then, so TrustedProxies and
	// MaxMultipartMemory had to wait until Start to land on the engine, and anyone using Engine()
	// on its own got the defaults. The config is now loaded the first time it is read: with New
	// first and the config file in place later, assembly still reads the final values.
	g := New().WithRoutes(echoClientIP)
	testkit.UseConfigEnv(t, "XGin:\n  TrustedProxies: [10.0.0.0/8]\n  MaxMultipartMemory: 1048576\n  Metric: false\n")

	if got := clientIPOf(t, g.Engine(), "10.0.0.5:1234"); got != "1.2.3.4" {
		t.Errorf("TrustedProxies from the config did not take effect, client_ip=%q", got)
	}
	if got := g.Engine().MaxMultipartMemory; got != 1<<20 {
		t.Errorf("MaxMultipartMemory from the config did not take effect, got=%d", got)
	}
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("the config turned metrics off, so there should be no /metrics")
	}
}

func TestEngine_InvalidConfigFallsBackToSafeDefaults(t *testing.T) {
	// An engine is still returned (the caller may have taken it before Run) but assembled from the
	// defaults: a half-decoded invalid config may well have TrustedProxies as 0.0.0.0/0. The error
	// is reported by Start, which does not listen.
	testkit.UseConfigEnv(t, "XGin:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	g := New().WithRoutes(echoClientIP)
	if got := clientIPOf(t, g.Engine(), "203.0.113.9:1234"); got != "203.0.113.9" {
		t.Errorf("an invalid config should fall back to the defaults (private ranges only), so a public peer's forwarding header must not be believed, client_ip=%q", got)
	}

	err := startErr(t, g)
	if xerror.Module(err) != "xgin" || !xerror.Is(err, "xconfig") || !strings.Contains(err.Error(), "Port") {
		t.Errorf("this should be xgin reporting, wrapping the error of that config block, got=%v", err)
	}
}

func TestCurrentConfig_AlwaysReadsConfigSection(t *testing.T) {
	// No need to wait for xone.Run: the config file is loaded the first time it is read, and what
	// comes back is the final values.
	testkit.UseConfigEnv(t, "XGin:\n  Port: 18080\n  Mode: debug\n  Log: false\n")
	c := CurrentConfig()
	if c.Port != 18080 || c.Mode != "debug" || c.Log {
		t.Errorf("the values from the config file should be read, got=%+v", c)
	}
	if c.Host != "0.0.0.0" || !c.Metric {
		t.Errorf("fields left out should keep their defaults, got=%+v", c)
	}
}

func TestCurrentConfig_ReturnsDefaultsWhenSectionInvalid(t *testing.T) {
	// The error is reported by xone.Run at startup (see TestLoadConfig_StartupFailsOnInvalidValue).
	// What this returns gets handed to WithConfig, so it has to be safe: a half-decoded invalid
	// config does not qualify.
	testkit.UseConfigEnv(t, "XGin:\n  Port: 0\n  TrustedProxies: [0.0.0.0/0]\n")
	if got := CurrentConfig(); !reflect.DeepEqual(got, DefaultConfig()) {
		t.Errorf("it should fall back to the defaults, got=%+v", got)
	}
}

func TestLoadConfig_StartupFailsOnFieldTypo(t *testing.T) {
	// A misspelled field has to be caught during startup, or the caller goes on believing it was
	// applied.
	testkit.UseConfigEnv(t, "XGin:\n  Prot: 8080\n")
	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("a misspelled field should fail startup")
	}
}

func TestLoadConfig_StartupFailsOnInvalidValue(t *testing.T) {
	// A port out of range, half-configured TLS and a wrong metrics path used to wait until the
	// server started. Config implements Validate, which runs during decoding, so this StageServer
	// hook fails startup on the spot.
	for name, yml := range map[string]string{
		"port out of range":                "XGin:\n  Port: 70000\n",
		"half-configured TLS":              "XGin:\n  TLS:\n    CertFile: cert.pem\n",
		"metrics path without a leading /": "XGin:\n  MetricPath: metrics\n",
	} {
		testkit.UseConfigEnv(t, yml)
		if err := loadConfig(context.Background()); err == nil {
			t.Errorf("%s should fail startup", name)
		}
	}
}

func TestWithConfig_TwoInstancesListenOnOwnPorts(t *testing.T) {
	// Without WithConfig both instances read the same block and can only listen on the same port —
	// which would make "there is a way out when two configs are needed" false for xgin. The block in
	// the config file stays on the default port, which shows neither instance listened according to
	// it.
	testkit.UseConfigEnv(t, "XGin:\n  Port: 8080\n")

	portA, portB := testkit.FreePort(t), testkit.FreePort(t)
	for _, s := range []struct {
		port int
		body string
	}{{portA, "A"}, {portB, "B"}} {
		c := CurrentConfig()
		c.Host, c.Port = "127.0.0.1", s.port
		quiet(&c)
		body := s.body
		g := New().WithConfig(c).WithRoutes(func(e *gin.Engine) {
			e.GET("/who", func(c *gin.Context) { c.String(200, body) })
		})
		serving(t, g, s.port)
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

func TestWithConfig_FollowsConfigFileWhenOmitted(t *testing.T) {
	// The default path: the port and the switches all come from the XGin block of the config file.
	port := testkit.FreePort(t)
	testkit.UseConfigEnv(t, fmt.Sprintf("XGin:\n  Host: 127.0.0.1\n  Port: %d\n  Log: false\n  Metric: false\n", port))

	g := New().WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	serving(t, g, port)
}

// ---- Registration ----

func TestRegister_OnlyReadsConfigNoStopHook(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgin" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("without a start hook the XGin block is unclaimed and a misconfiguration waits until Start")
	}
	if got.Stage != hook.StageServer {
		t.Errorf("the server belongs in the last stage, got=%v", got.Stage)
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgin" {
			t.Error("the server is started and stopped by the caller through xone.Run, so no stop hook belongs here")
		}
	}
}

func TestXGin_ImplementsRunnable(t *testing.T) {
	// Structural satisfaction is enough, without importing the root package - "an integration does
	// not depend on the framework" has to hold at compile time.
	var _ interface {
		Start(context.Context) error
		Stop(context.Context) error
	} = New()
}

func TestSensitiveFieldsAPI(t *testing.T) {
	// Custom sensitive fields have to be addable before body logging is turned on.
	middleware.AddSensitiveFields("x_custom")
	middleware.AddSensitiveHeaders("X-Custom")
}

// doRequest sends one request to an engine.
func doRequest(t *testing.T, e *gin.Engine, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// waitServing waits until the server really listens.
// oneShot keeps no connection alive, so a request through it leaves nothing pooled.
//
// Sharing http.DefaultTransport's pool makes a request race the pooled connection against a fresh
// dial (queueForIdleConn against queueForDial) and both can win: the dial that lost leaves a
// connection the server has accepted and will never read a request header from. net/http counts
// such a StateNew connection as idle only once it is five seconds old (closeIdleConns, go#22682,
// and the comparison is in whole seconds so it is up to six), and every Shutdown in the meantime
// waits it out. Measured on TestStartStop_GracefulShutdown, whose Stop has no deadline of its own:
// 6.27s on the rounds where the race happened, 0.00s on the rest.
var oneShot = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func waitServing(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		resp, err := oneShot.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the server never came up: %s", url)
}

func TestBuild_MetricsKeptWhenEngineFetchedFirst(t *testing.T) {
	// Regression. Grabbing xmetric's registry at assembly time, before xmetric is initialized,
	// registers the metrics onto a fallback registry that is never exported: requests are handled
	// normally, metrics are recorded normally, /metrics shows nothing, and there is no sign of it.
	g := New().WithConfig(DefaultConfig()).WithRoutes(func(e *gin.Engine) {
		e.GET("/x", func(c *gin.Context) { c.Status(200) })
	})
	e := g.Engine() // taking the engine before Run is a natural thing for a caller to do

	// Only afterwards does the framework initialize xmetric (StageTelemetry comes before StageServer)
	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("metrics should be recorded on the registry that exists after initialization\ngot=\n%s", w.Body.String())
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

	// Send a request first to produce a metric, then scrape /metrics.
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))

	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("/metrics should export the registry currently in effect\ngot=\n%s", w.Body.String())
	}
}

func TestBuild_MetricsEndpointRunsUserMiddleware(t *testing.T) {
	// Gin fixes the handler chain at the moment a route is registered. The metrics endpoint used to
	// be registered before e.Use(g.extra...), so the shared auth mounted with WithMiddleware applied
	// to business routes and not to /metrics — an endpoint believed to be protected was open.
	auth := func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }

	e := New().WithConfig(configWith(func(c *Config) { c.Log = false })).
		WithMiddleware(auth).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/biz", func(c *gin.Context) { c.Status(200) })
		}).Engine()

	for _, path := range []string{"/biz", "/metrics"} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s should be stopped by the auth middleware, got=%d", path, w.Code)
		}
	}
}

func TestBuild_NotFoundRunsUserMiddleware(t *testing.T) {
	// The framework's 404 and 405 handlers are registered like any other route, and gin fixes a
	// route's chain when it goes on: mounted after e.Use(g.extra...), they are covered by the
	// shared auth a caller mounted with WithMiddleware. The other way round, an unmatched path
	// would walk past the auth every real route sits behind.
	auth := func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }

	e := New().WithConfig(configWith(func(c *Config) { c.Log, c.Metric = false, false })).
		WithMiddleware(auth).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/biz", func(c *gin.Context) { c.Status(200) })
		}).Engine()

	for _, r := range []struct{ method, path string }{
		{"GET", "/nope"}, // NoRoute
		{"POST", "/biz"}, // NoMethod
	} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(r.method, r.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s should be stopped by the auth middleware, got=%d", r.method, r.path, w.Code)
		}
	}
}

// ---- applyConfig ----

func TestApplyConfig_FallsBackToTrustNoneOnProxyError(t *testing.T) {
	// gin's default is to trust everyone, so anyone sending an X-Forwarded-For decides client_ip in
	// the access log. It parses until the first error and keeps whatever it already parsed, so the
	// first half of the list stays trusted — the whole thing has to fall back to the safe side.
	//
	// A valid range has to come before the invalid entry: with the invalid one alone gin keeps an
	// empty list, the two implementations look identical, and this test would be checking nothing.
	e := gin.New()
	e.GET("/", func(c *gin.Context) { c.String(200, c.ClientIP()) })

	c := DefaultConfig()
	c.TrustedProxies = []string{"10.0.0.0/8", "not-an-ip"} // Validate would reject this, so it is bypassed here
	trusted := applyConfig(e, c)

	// Both sides of "who counts as one of us" fall back together: gin's client_ip below, and the
	// ranges the forwarded headers are judged against, which are what applyConfig hands back.
	if trusted.Trusts("10.0.0.1") {
		t.Error("when the setup fails, the forwarded headers of that first range must not be believed either")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1234" // inside that first range
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Body.String() != "10.0.0.1" {
		t.Errorf("when the setup fails, nobody should be trusted and the direct address used, got=%q", w.Body.String())
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
func probeTrust(t *testing.T) func(e *gin.Engine, remote string) bool {
	t.Helper()
	old := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(old) })
	var got atomic.Bool
	otel.SetTextMapPropagator(trustProbe{got: &got})

	return func(e *gin.Engine, remote string) bool {
		got.Store(false)
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		e.ServeHTTP(httptest.NewRecorder(), req)
		return got.Load()
	}
}

func TestBuild_PassthroughHeadersOnlyFromTrustedProxies(t *testing.T) {
	// Regression. Forwarded headers used to be accepted from any inbound request: a public client
	// sending X-Tenant-Id / X-Internal-Token had it treated as coming from one of us, carried into
	// every internal call. Who counts as one of us comes from the trusted-proxy table, with no
	// switch of its own.
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
	// Pod IPs in Kubernetes are random but all inside the private ranges: trusted by default, with
	// nothing to list.
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

func TestLoadConfig_EmptyTrustedProxiesInFileTrustsNone(t *testing.T) {
	// Defaults are pre-filled in the struct; a [] in the config file has to become a genuinely
	// empty list rather than being decoded as "not written" and left at the private default.
	testkit.UseConfigEnv(t, "XGin:\n  TrustedProxies: []\n")
	if got := clientIPOf(t, New().WithRoutes(echoClientIP).Engine(), "10.0.0.5:1234"); got != "10.0.0.5" {
		t.Errorf("a [] in the config file should trust nobody, got=%q", got)
	}
}

func TestBuild_TrustedProxiesMixesPrivateWithOtherCIDRs(t *testing.T) {
	// The list replaces the default wholesale: to add a range on top of the private ones, write
	// private alongside it.
	trusted := probeTrust(t)
	e := New().WithConfig(configWith(quiet, func(c *Config) {
		c.TrustedProxies = []string{"private", "203.0.113.0/24"}
	})).Engine()
	for remote, want := range map[string]bool{"10.0.0.5:1": true, "203.0.113.9:1": true, "198.51.100.1:1": false} {
		if got := trusted(e, remote); got != want {
			t.Errorf("peer %s trusted=%v, want %v", remote, got, want)
		}
	}
	// With [] nobody is trusted, forwarded headers included.
	none := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = []string{} })).Engine()
	if trusted(none, "10.0.0.5:1") {
		t.Error("with [] nobody should be trusted")
	}
}

func TestBuild_TraceDisabledOnlySkipsSpan_StillPropagates(t *testing.T) {
	// Regression. XGin.Trace: false used to drop Extract as well: an X-Request-Id from a trusted
	// peer and an upstream traceparent both stopped at this hop, while it should govern the span
	// only.
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

	// The upstream trace identifiers reach the request ctx, while this hop opens no span and echoes
	// no X-Trace-Id.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	oldTP := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(oldTP) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const upstream = "4bf92f3577b34da6a3ce929d0e0e4736"
	var seen string
	e = New().WithConfig(configWith(quiet, off)).WithRoutes(func(e *gin.Engine) {
		e.GET("/x", func(c *gin.Context) {
			seen = trace.SpanContextFromContext(c.Request.Context()).TraceID().String()
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
func servingH2C(t *testing.T, d time.Duration) (base string, g *XGin, entered, finished chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)

	entered, finished = make(chan struct{}), make(chan struct{})
	g = New().WithConfig(configWith(quiet, on(port), func(c *Config) { c.UseH2C = true })).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/ping", func(c *gin.Context) { c.Status(200) })
			e.GET("/slow", func(c *gin.Context) {
				close(entered)
				time.Sleep(d)
				close(finished)
				c.String(200, "done")
			})
		})
	go g.Start(context.Background())
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	return base, g, entered, finished
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
	// Regression. h2c used to rely on x/net's h2c.NewHandler: it hijacked the connection and
	// http.Server no longer knew about it — measured, Shutdown returned nil in about 60µs while a
	// 2s request was still running, and Close() could not reach them either. The framework then
	// went on to close the database and that request found a closed pool.
	base, g, entered, finished := servingH2C(t, time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := g.Stop(context.Background()); err != nil {
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
	// The other half: an h2c request still running at the deadline has to be dropped by Close()
	// rather than keep going after the framework closed the database.
	base, g, entered, _ := servingH2C(t, 5*time.Second)
	got := h2cGet(base + "/slow")
	<-entered

	if err := stopWithin(t, g); err == nil {
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
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	base := serving(t, g, port)

	if r := <-h2cGet(base + "/ping"); r.err == nil {
		t.Errorf("without UseH2C, cleartext HTTP/2 must not succeed, got=HTTP/%d", r.proto)
	}
}

func TestDefault404And405_SameAsGinAndBytesOutCounted(t *testing.T) {
	// gin writes its default 404 / 405 body only after the middleware chain has run, so the access
	// log's bytes_out used to be 0; now that the same content is written inside the chain, the
	// response has to stay identical to gin's own, byte for byte.
	bare := gin.New()
	bare.HandleMethodNotAllowed = true
	bare.GET("/only-get", func(c *gin.Context) {})

	for _, c := range []struct{ name, method, path string }{
		{"404", "GET", "/nope"},
		{"405", "POST", "/only-get"},
	} {
		buf := captureLog(t)
		e := New().WithConfig(configWith(func(cfg *Config) { cfg.Metric, cfg.Trace = false, false })).
			WithRoutes(func(e *gin.Engine) { e.GET("/only-get", func(c *gin.Context) {}) }).Engine()
		got, want := httptest.NewRecorder(), httptest.NewRecorder()
		e.ServeHTTP(got, httptest.NewRequest(c.method, c.path, nil))
		bare.ServeHTTP(want, httptest.NewRequest(c.method, c.path, nil))

		if got.Code != want.Code || got.Body.String() != want.Body.String() ||
			got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
			t.Errorf("%s: the response should match gin's default, got=%d %q %q want=%d %q %q", c.name,
				got.Code, got.Header().Get("Content-Type"), got.Body, want.Code, want.Header().Get("Content-Type"), want.Body)
		}
		if w := fmt.Sprintf(`"bytes_out":%d`, want.Body.Len()); !strings.Contains(buf.String(), w) {
			t.Errorf("%s: the access log bytes_out should be the body length %d\n%s", c.name, want.Body.Len(), buf.String())
		}
	}
}

func TestNoRoute_UserHandlerStillWins(t *testing.T) {
	// The framework's NoRoute is registered before the caller's routes, so one set in WithRoutes
	// has to win over it.
	e := New().WithConfig(configWith(func(cfg *Config) { cfg.Metric, cfg.Trace, cfg.Log = false, false, false })).
		WithRoutes(func(e *gin.Engine) { e.NoRoute(func(c *gin.Context) { c.String(404, "custom") }) }).Engine()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if w.Body.String() != "custom" {
		t.Errorf("the caller's own NoRoute should take effect, got=%q", w.Body)
	}
}

func TestClientIP_SharedRuleMatchesGin(t *testing.T) {
	// xecho has no gin available and computes client_ip through internal/web's Proxies.ClientIP,
	// whose rules are copied from gin. The same service moved to another framework must not see
	// client_ip change, so this compares entry by entry against what gin itself computes.
	for _, proxies := range [][]string{{web.TrustPrivate}, {}, {"10.0.0.0/8", "203.0.113.7"}, {web.TrustPrivate, "203.0.113.0/24"}} {
		e := New().WithConfig(configWith(quiet, func(c *Config) { c.TrustedProxies = proxies })).WithRoutes(echoClientIP).Engine()
		shared := web.ParseProxies(proxies)
		for _, remote := range []string{"203.0.113.9:1", "203.0.113.7:1", "10.0.0.5:1", "[::ffff:10.0.0.5]:1", "[::1]:1", "[2001:db8::1]:1", "junk"} {
			for _, h := range []map[string][]string{
				{},
				{"X-Forwarded-For": {"1.2.3.4"}},
				{"X-Forwarded-For": {"1.2.3.4, 10.0.0.9"}},
				{"X-Forwarded-For": {"1.2.3.4, 5.6.7.8, 10.0.0.9"}},
				{"X-Forwarded-For": {"10.0.0.7, 10.0.0.8"}},
				{"X-Forwarded-For": {"1.2.3.4", "5.6.7.8"}},
				{"X-Forwarded-For": {"junk, 10.0.0.9"}},
				{"X-Forwarded-For": {"1.2.3.4, junk"}},
				{"X-Forwarded-For": {"junk"}, "X-Real-Ip": {"9.9.9.9"}},
				{"X-Real-Ip": {"9.9.9.9"}},
				{"X-Forwarded-For": {" 1.2.3.4 ,10.0.0.9 "}},
			} {
				req := httptest.NewRequest("GET", "/client-ip", nil)
				req.RemoteAddr = remote
				for k, vs := range h {
					for _, v := range vs {
						req.Header.Add(k, v)
					}
				}
				w := httptest.NewRecorder()
				e.ServeHTTP(w, req)
				if got := shared.ClientIP(req); got != w.Body.String() {
					t.Errorf("proxies=%v remote=%s headers=%v: gin=%q web=%q", proxies, remote, h, w.Body.String(), got)
				}
			}
		}
	}
}

// ---- The zero value ----

func TestZeroValue_ReportsUnderModuleName(t *testing.T) {
	// &XGin{} has to work without New: errors and logs are still recorded under xgin, so a caller's
	// xerror.Is(err, "xgin") holds and the log never shows a headless " listening".
	buf := captureLog(t)
	cert := filepath.Join(t.TempDir(), "missing.pem")
	err := startErr(t, (&XGin{}).WithConfig(configWith(quiet, on(testkit.FreePort(t)), func(c *Config) { c.TLS.CertFile, c.TLS.KeyFile = cert, cert })))
	if !xerror.Is(err, "xgin") || xerror.Module(err) != "xgin" {
		t.Fatalf("the error from a zero value Start should belong to xgin, got=%v", err)
	}

	z := &XGin{}
	if err := z.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start should do nothing, got=%v", err)
	}
	if err := z.Start(context.Background()); err != nil {
		t.Fatalf("Start after Stop should return nil immediately, got=%v", err)
	}
	if !strings.Contains(buf.String(), `"msg":"xgin received the shutdown signal before starting`) {
		t.Errorf("the log message should start with xgin, got=%s", buf.String())
	}
}

func TestStart_NoListeningLogWhenListenFails(t *testing.T) {
	// "listening" is only printed once it really listens: saying listening first and reporting
	// listen failed afterwards (an unreadable certificate, a taken port) makes whoever is debugging
	// believe the server was up.
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
		if strings.Contains(buf.String(), "xgin listening") {
			t.Errorf("%s: listening must not be printed when nothing is listening, got=%s", name, buf.String())
		}
	}
}

func TestLoadConfig_IPv4MappedProxyFailsStartup(t *testing.T) {
	// ::ffff:10.0.0.1 means one thing to gin and another to xtrace's trust check, so it can only be
	// rejected, with a message saying how to write it.
	err := loadErr(t, "XGin:\n  TrustedProxies: [private, \"::ffff:10.0.0.1\"]\n")
	if err == nil || !strings.Contains(err.Error(), "IPv4-mapped") || !strings.Contains(err.Error(), "write it as 10.0.0.1") {
		t.Fatalf("the IPv4-mapped spelling should fail startup and give the IPv4 spelling, got=%v", err)
	}
}

// servingHijacked starts a server whose /ws hijacks the connection and then waits on the request
// ctx (which is what a WebSocket read loop does), and returns the server plus a signal that the
// handler has hijacked and is waiting.
func servingHijacked(t *testing.T) (*XGin, <-chan struct{}) {
	t.Helper()
	port := testkit.FreePort(t)
	entered, release := make(chan struct{}), make(chan struct{})
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
		e.GET("/ws", func(c *gin.Context) {
			conn, _, err := c.Writer.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			close(entered)
			select {
			case <-c.Request.Context().Done():
			case <-release: // also lets the Stop in Cleanup return when the test fails
			}
		})
	})
	base := serving(t, g, port)
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
	return g, entered
}

func TestStop_HijackedConnGetsCancelled(t *testing.T) {
	// Regression. Shutdown does not wait for a hijacked connection (a WebSocket) and Close does not
	// drop it either, so the request ctx used to never be cancelled: the handler waiting on it never
	// returned and Stop burned the whole budget before reporting 1 handler(s) still running.
	g, _ := servingHijacked(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := g.Stop(ctx)
	if err != nil {
		t.Fatalf("the hijacked handler returned once it saw the ctx cancelled, so Stop must not fail: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop should cancel the hijacked connection's ctx right after Shutdown returns, but took %v", d)
	}
}

func TestStart_RetryAfterListenFailure(t *testing.T) {
	// Regression. After a failed listen (a taken port) s.srv used to be kept, and every later Start
	// said already running.
	port := testkit.FreePort(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	if err := startErr(t, g); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("with the port taken, a listen failure is expected, got=%v", err)
	}
	ln.Close()

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	select {
	case err := <-done:
		t.Fatalf("once the port is free it should start again, got=%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/ping", port))
}

func TestBuild_TrailingSlashGoesThroughMiddlewareAs404(t *testing.T) {
	// Regression. gin enables RedirectTrailingSlash by default (handleHTTPRequest in gin.go,
	// v1.12.0): when /users/ does not match /users it answers 301 outright without running the
	// middleware chain — no access log, no metrics, no span. With it off, it goes to NoRoute and is
	// recorded as a 404 unmatched.
	buf := captureLog(t)
	route := func(e *gin.Engine) { e.GET("/users", func(c *gin.Context) { c.String(200, "ok") }) }
	e := New().WithConfig(configWith(func(c *Config) { c.Metric, c.Trace = false, false })).WithRoutes(route).Engine()

	if w := doRequest(t, e, "GET", "/users/"); w.Code != 404 {
		t.Errorf("one extra trailing slash should be a 404 rather than a redirect, got=%d Location=%q", w.Code, w.Header().Get("Location"))
	}
	if out := buf.String(); !strings.Contains(out, `"route":"unmatched","path":"/users/","status":404`) {
		t.Errorf("a 404 unmatched access log line is expected: %s", out)
	}

	// A caller who wants the redirect turns it on inside WithRoutes, which wins over the config.
	e = New().WithConfig(configWith(quiet)).WithRoutes(func(e *gin.Engine) { e.RedirectTrailingSlash = true }, route).Engine()
	if w := doRequest(t, e, "GET", "/users/"); w.Code != http.StatusMovedPermanently {
		t.Errorf("RedirectTrailingSlash turned on in WithRoutes should redirect as gin does by default, got=%d", w.Code)
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
	t.Cleanup(func() { slog.SetDefault(old); closer.Close() })
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

func TestBuild_GinContextAsContextKeepsSpanLogScopeAndCancel(t *testing.T) {
	// Regression. A *gin.Context is itself a context.Context and business code often passes it
	// directly: xlog.AddKV(c, ...), db.WithContext(c). Without ContextWithFallback its Value only
	// looks at c.Keys and Done returns nil: the log scope and the parent span are both lost, and
	// cancellation does not propagate.
	logs := xlogCapture(t)
	exp := tracetest.NewInMemoryExporter()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	var done <-chan struct{}
	e := New().WithConfig(configWith(func(c *Config) { c.Metric = false })).WithRoutes(func(e *gin.Engine) {
		e.GET("/a", func(c *gin.Context) {
			xlog.AddKV(c, "user_id", "u9")
			_, span := otel.Tracer("test").Start(c, "child")
			span.End()
			done = c.Done()
			c.Status(200)
		})
	}).Engine()
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/a", nil).WithContext(reqCtx))

	var server, child sdktrace.ReadOnlySpan
	for _, s := range exp.GetSpans().Snapshots() {
		if s.Name() == "child" {
			child = s
		} else {
			server = s
		}
	}
	if server == nil || child == nil || child.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("a span opened from a *gin.Context should hang under the server span")
	}
	if done == nil || done != reqCtx.Done() {
		t.Error("c.Done() should be the request ctx's Done, which is what propagates a client leaving")
	}
	if out := logs(); !strings.Contains(out, `"user_id":"u9"`) {
		t.Errorf("the xlog.AddKV(c, ...) fields should reach the access log:\n%s", out)
	}
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

func TestStart_GinUseH2CDoesNotBypassShutdown(t *testing.T) {
	// Regression. gin's engine.Handler() wraps x/net's h2c.NewHandler when UseH2C is on: the
	// connection is hijacked and Shutdown does not wait for in-flight requests. What is handed to
	// net/http should be the engine itself, with h2c going through the standard library via
	// XGin.UseH2C. Here gin's own UseH2C is turned on inside the callback, and the request the
	// server sees must not be one h2c produced.
	port := testkit.FreePort(t)
	var proto int
	g := New().WithConfig(configWith(quiet, on(port))).WithRoutes(func(e *gin.Engine) {
		e.UseH2C = true
		e.GET("/ping", func(c *gin.Context) { proto = c.Request.ProtoMajor; c.Status(200) })
	})
	go func() { _ = g.Start(context.Background()) }()
	t.Cleanup(func() {
		// With a deadline: a connection wrapped into h2c is hijacked, and without one this would wait
		// on that goroutine forever.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = g.Stop(ctx)
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")
	if r := <-h2cGet(base + "/ping"); r.err == nil {
		t.Errorf("with XGin.UseH2C off, gin's own UseH2C must not let cleartext HTTP/2 bypass net/http, got=HTTP/%d handler=HTTP/%d", r.proto, proto)
	}
}

// containsHan reports whether s holds at least one Han character, which is how the translation
// test checks a message was translated without spelling any Chinese out.
func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
