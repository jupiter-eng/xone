package middleware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	"github.com/jupiter-eng/xone/xlog"
)

// capture replaces the default logger with a real xlog instance and returns a function that
// yields the parsed lines.
//
// It has to be xlog rather than any slog handler: the fields LogScope puts into the context are
// pulled back out by xlog's handler, and another handler shows none of them — which is exactly
// what "LogScope only works with xlog" means in practice.
func capture(t *testing.T) func() []map[string]any {
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

	return func() []map[string]any {
		closer.Close() // flush before reading
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
				t.Fatalf("the log line is not JSON: %v, content=%q", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

// accessLogs keeps only the access log lines.
func accessLogs(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["msg"] == "request completed" {
			out = append(out, l)
		}
	}
	return out
}

// newEcho is an echo with the middleware installed; /hello and /hello/:id register h for every
// standard method, and a request using another method (CUSTOM1) gets its own registration -
// otherwise that would be a 405.
func newEcho(mws []echo.MiddlewareFunc, h echo.HandlerFunc, extra ...string) *echo.Echo {
	e := echo.New()
	e.Use(mws...)
	e.Any("/hello/:id", h)
	e.Any("/hello", h)
	for _, m := range extra {
		e.Add(m, "/hello/:id", h)
		e.Add(m, "/hello", h)
	}
	return e
}

// serve installs the middleware and runs one request.
func serve(t *testing.T, req *http.Request, mws []echo.MiddlewareFunc, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	newEcho(mws, h, req.Method).ServeHTTP(w, req)
	return w
}

func get(path string) *http.Request { return httptest.NewRequest("GET", path, nil) }

// status makes the handler answer with a status code only.
func statusOnly(code int) echo.HandlerFunc {
	return func(c echo.Context) error { return c.NoContent(code) }
}

// ---- LogScope ----

func TestLogScope_HandlersCanAddFieldsAtAnyDepth(t *testing.T) {
	// Deep in the call stack there is no echo.Context, so there was never a chance to pass a new
	// context back up.
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{LogScope(), Log()}, func(c echo.Context) error {
		deepInBusinessCode(c.Request().Context())
		return c.String(200, "ok")
	})

	got := lines()
	if len(got) == 0 {
		t.Fatal("no access log")
	}
	last := got[len(got)-1]
	if last["user"] != "u9" {
		t.Errorf("a field added by business code should appear in the access log, got=%v", last)
	}
}

func deepInBusinessCode(ctx context.Context) { xlog.AddKV(ctx, "user", "u9") }

// ---- Recover ----

func TestRecover_CatchesPanicAndReturns500(t *testing.T) {
	// Echo does not catch panics itself: the connection is simply broken and the stack goes to
	// stderr rather than the logging platform.
	lines := capture(t)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
		panic("boom")
	})

	if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"message":"Internal Server Error"}` {
		t.Errorf("echo's default 500 response is expected, got=%d %q", w.Code, w.Body.String())
	}
	got := lines()
	if len(got) == 0 || got[0]["level"] != "ERROR" || got[0]["msg"] != "panic while handling request" {
		t.Fatalf("exactly one error log line is expected, got=%v", got)
	}
	if got[0]["stack"] == nil || !strings.Contains(got[0]["stack"].(string), "middleware_test.go") {
		t.Errorf("the stack should be included, got=%v", got[0])
	}
	if got[0]["path"] != "/hello" || got[0]["method"] != "GET" || got[0]["error"] != "boom" {
		t.Errorf("the request path, the method and the panic value should be included, got=%v", got[0])
	}
}

func TestRecover_ErrAbortHandlerRepanicsToNetHTTP(t *testing.T) {
	// http.ErrAbortHandler is the agreed way for a handler to say "drop this connection", and
	// net/http receives it, breaks the connection silently and prints no stack. Catching it and
	// turning it into a 500 would send the response that was meant to be aborted, plus a panic
	// stack that means nothing.
	lines := capture(t)
	var got any
	func() {
		defer func() { got = recover() }()
		serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
			panic(http.ErrAbortHandler)
		})
	}()
	if got != http.ErrAbortHandler {
		t.Fatalf("ErrAbortHandler should be re-panicked as is, got=%v", got)
	}
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			t.Errorf("a deliberate abort is not a failure and should log no panic, got=%v", l)
		}
	}
}

func TestRecover_CustomResponse(t *testing.T) {
	capture(t)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(func(c echo.Context, _ any) error {
		return c.JSON(503, map[string]string{"msg": "try again later"})
	})}, func(c echo.Context) error { panic("boom") })

	if w.Code != 503 || !strings.Contains(w.Body.String(), "try again later") {
		t.Errorf("the custom response should be used, got=%d %s", w.Code, w.Body.String())
	}
}

func TestRecover_CustomFuncErrorGoesThroughHTTPErrorHandler(t *testing.T) {
	// The error it returns is rendered by e.HTTPErrorHandler exactly as a handler's would be: a
	// custom error format applies to panics too.
	capture(t)
	e := newEcho([]echo.MiddlewareFunc{Recover(func(echo.Context, any) error {
		return echo.NewHTTPError(503, "busy")
	})}, func(c echo.Context) error { panic("boom") })
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		var he *echo.HTTPError
		errors.As(err, &he)
		_ = c.String(he.Code, "custom:"+fmt.Sprint(he.Message))
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, get("/hello"))
	if w.Code != 503 || w.Body.String() != "custom:busy" {
		t.Errorf("it should go through HTTPErrorHandler, got=%d %q", w.Code, w.Body.String())
	}
}

func TestRecover_KeepsStatusOnceResponseStarted(t *testing.T) {
	// Once the response has started going out, changing the status code only yields a truncated
	// response.
	capture(t)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
		_ = c.String(200, "part of this already went out")
		panic("boom mid-write")
	})

	if w.Code != 200 {
		t.Errorf("a response already written must not be turned into a 500, got=%d", w.Code)
	}
	if w.Body.String() != "part of this already went out" {
		t.Errorf("what was written should remain, with no error response appended, got=%q", w.Body.String())
	}
}

func TestRecover_CustomFuncNotCalledOnceResponseStarted(t *testing.T) {
	// Once the response has started going out the custom recover function is not called: whatever
	// it writes would only be appended to a truncated response.
	capture(t)
	called := false
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(func(c echo.Context, _ any) error {
		called = true
		return c.String(503, "try again later")
	})}, func(c echo.Context) error {
		_ = c.String(200, "partial")
		panic("boom mid-write")
	})
	if called || w.Body.String() != "partial" {
		t.Errorf("the recover function must not be called once the response started, called=%v body=%q", called, w.Body.String())
	}
}

func TestRecover_NoOpWithoutPanic(t *testing.T) {
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error {
		return c.String(201, "ok")
	})
	if w.Code != 201 {
		t.Errorf("a normal request must not be interfered with, got=%d", w.Code)
	}
}

func TestRecover_BrokenPipeLoggedWithoutStack(t *testing.T) {
	// A client disconnecting early is not a failure: log one connection broken line, no stack, and
	// write no further response.
	lines := capture(t)
	broken := &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: errors.New("broken pipe")}}
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(), Recover(nil)}, func(c echo.Context) error { panic(broken) })

	got := lines()
	var sawBroken bool
	for _, l := range got {
		if l["msg"] == "panic while handling request" {
			t.Errorf("a broken connection must not be logged as a panic with a stack, got=%v", l)
		}
		if l["msg"] == "connection broken" {
			// A client leaving is not the service's failure, so WARN: ERROR would make the alerts
			// follow the client's flaky network.
			sawBroken = l["level"] == "WARN"
		}
	}
	if !sawBroken {
		t.Errorf("exactly one WARN connection broken line is expected, got=%v", got)
	}
	if a := accessLogs(got); len(a) != 1 || !strings.Contains(fmt.Sprint(a[0]["errors"]), "broken pipe") {
		t.Errorf("the access log errors field should carry the reason for the broken connection, got=%v", a)
	}
}

// ---- Log ----

func TestLog_RecordsKeyFields(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello/42?q=1"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		return c.String(201, "ok")
	})

	got := lines()
	if len(got) != 1 {
		t.Fatalf("exactly one line is expected, got=%v", got)
	}
	l := got[0]
	// The route is the template rather than the real path: /hello/42 and /hello/43 are one endpoint.
	if l["route"] != "/hello/:id" {
		t.Errorf("route should be the template, got=%v", l["route"])
	}
	if l["path"] != "/hello/42" {
		t.Errorf("path should be the real path, got=%v", l["path"])
	}
	if l["status"] != float64(201) || l["method"] != "GET" {
		t.Errorf("the status and the method are wrong, got=%v", l)
	}
	if l["elapsed_ms"] == nil || l["client_ip"] != "192.0.2.1" {
		t.Errorf("the duration and the client should be recorded, got=%v", l)
	}
	if _, ok := l["errors"]; ok {
		t.Errorf("without an error there should be no errors field, got=%v", l)
	}
}

func TestLog_RecordsRequestAndResponseSizes(t *testing.T) {
	// With body logging on the request body is buffered and the response is captured, and both
	// byte counts still have to be right.
	for name, opts := range map[string][]LogOption{"defaults": nil, "body recorded": {WithBody(true, true)}} {
		lines := capture(t)
		req := httptest.NewRequest("POST", "http://api.example.com/hello/1", strings.NewReader("12345"))
		req.Header.Set("User-Agent", "probe/1.0")
		serve(t, req, []echo.MiddlewareFunc{Log(opts...)}, func(c echo.Context) error { return c.String(200, "hello!") })
		l := lines()[0]
		for k, want := range map[string]any{
			"host": "api.example.com", "proto": "HTTP/1.1", "user_agent": "probe/1.0",
			"bytes_in": float64(5), "bytes_out": float64(6),
		} {
			if l[k] != want {
				t.Errorf("%s: %s should be %v, got=%v", name, k, want, l[k])
			}
		}
	}
}

func TestLog_BytesOutIsZeroWhenNothingWritten(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, statusOnly(204))
	if l := lines()[0]; l["bytes_out"] != float64(0) || l["status"] != float64(204) {
		t.Errorf("c.NoContent(204): bytes_out should be 0 and status 204, got=%v", l)
	}
}

func TestLog_BytesOutCountsJSONAndStreamedResponses(t *testing.T) {
	// A c.JSON body carries one newline (echo serializes JSON with json.Encoder); writing in
	// several Flush-ed chunks has to be counted in full as well.
	for name, c := range map[string]struct {
		h    echo.HandlerFunc
		want int
	}{
		"c.JSON": {func(c echo.Context) error { return c.JSON(200, map[string]string{"a": "b"}) }, len(`{"a":"b"}` + "\n")},
		"chunked write": {func(c echo.Context) error {
			c.Response().WriteHeader(200)
			for i := 0; i < 3; i++ {
				_, _ = c.Response().Write([]byte("chunk"))
				c.Response().Flush()
			}
			return nil
		}, 15},
	} {
		lines := capture(t)
		w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, c.h)
		if l := lines()[0]; l["bytes_out"] != float64(c.want) || w.Body.Len() != c.want {
			t.Errorf("%s: bytes_out should be %d (the client received %d), got=%v", name, c.want, w.Body.Len(), l["bytes_out"])
		}
	}
}

func TestLog_ElapsedIsMilliseconds(t *testing.T) {
	// slog's JSON writes a Duration as an integer of nanoseconds: a 20ms request is recorded as
	// 20000000, a million times off for an alert threshold configured in "milliseconds".
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		time.Sleep(20 * time.Millisecond)
		return c.NoContent(200)
	})
	ms, ok := lines()[0]["elapsed_ms"].(float64)
	if !ok || ms < 20 || ms > 1000 {
		t.Errorf("elapsed_ms should be milliseconds (about 20), got=%v", lines()[0]["elapsed_ms"])
	}
}

func TestLog_UnmatchedRouteIsUnmatched(t *testing.T) {
	// Consistent with the metrics and the span: filling in the real path would make /nope
	// indistinguishable between a route and a 404 in the log.
	lines := capture(t)
	serve(t, get("/nope/1"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	l := lines()[0]
	if l["route"] != "unmatched" || l["path"] != "/nope/1" || l["status"] != float64(404) {
		t.Errorf("an unmatched request should have route unmatched and path as the real path, got=%v", l)
	}
}

func TestLog_GroupedRouteIsTheWholeTemplate(t *testing.T) {
	// A version prefix is how an API is usually laid out, and the label has to be the whole
	// template: with the part registered inside the group alone, /api/v1/users/:id and
	// /api/v2/users/:id would share one label in the log, the metrics and the span names alike.
	lines := capture(t)
	e := echo.New()
	e.Use(Log())
	e.Group("/api/v1").GET("/users/:id", statusOnly(200))
	e.ServeHTTP(httptest.NewRecorder(), get("/api/v1/users/42"))

	l := accessLogs(lines())[0]
	if l["route"] != "/api/v1/users/:id" || l["path"] != "/api/v1/users/42" {
		t.Errorf("route should be the whole template, the group prefix included, got=%v", l)
	}
}

// errorCases covers requests where the handler returns an error or the router itself fails,
// together with the status the client actually receives and the route label.
var errorCases = []struct {
	name, method, path string
	status             int
	route, errs        string
}{
	{"unmatched route (404)", "GET", "/nope", 404, "unmatched", "code=404, message=Not Found"},
	{"wrong method (405)", "POST", "/only-get", 405, "unmatched", "code=405, message=Method Not Allowed"},
	// The error text carries password=, so that value is masked and the sentence around it stays
	{"plain error (500)", "GET", "/plain", 500, "/plain", "db down: password=" + Redacted},
	{"echo.NewHTTPError(418)", "GET", "/teapot", 418, "/teapot", "code=418, message=I'm a teapot"},
	// A group with middleware: echo quietly registers RouteNotFound fallbacks for it (/api/v1,
	// /api/v1/*), so c.Path() is the fallback's template.
	{"unmatched path under a group", "GET", "/api/v1/nope", 404, "unmatched", "code=404, message=Not Found"},
	{"wrong method under a group (echo answers 404, not 405)", "POST", "/api/v1/users", 404, "unmatched", "code=404, message=Not Found"},
	{"a RouteNotFound the caller registered", "GET", "/custom/x", 404, "unmatched", "code=404, message=custom"},
}

// passThrough is group middleware that does nothing: its presence is what makes echo register
// the RouteNotFound fallbacks for the group.
func passThrough(next echo.HandlerFunc) echo.HandlerFunc { return next }

// errorEcho registers the routes errorCases needs.
func errorEcho(mws ...echo.MiddlewareFunc) *echo.Echo {
	e := echo.New()
	e.Use(mws...)
	e.GET("/only-get", statusOnly(200))
	e.GET("/plain", func(echo.Context) error { return errors.New("db down: password=s3cret") })
	e.GET("/teapot", func(echo.Context) error { return echo.NewHTTPError(http.StatusTeapot) })
	e.Group("/api/v1", passThrough).GET("/users", statusOnly(200))
	e.RouteNotFound("/custom/*", func(echo.Context) error { return echo.NewHTTPError(http.StatusNotFound, "custom") })
	return e
}

func TestLog_FallbackTemplateKeptForMethodsRegisteredOnIt(t *testing.T) {
	// A fallback and a real route can share a template: a group with GET("/*") proxying, plus the
	// /proxy/* fallback echo registered for the group.
	// GET hits the real route and records the template; other methods land on the fallback (404)
	// and record unmatched.
	lines := capture(t)
	e := echo.New()
	e.Use(Log())
	e.Group("/proxy", passThrough).GET("/*", statusOnly(200))
	e.ServeHTTP(httptest.NewRecorder(), get("/proxy/a"))
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/proxy/a", nil))
	got := accessLogs(lines())
	if len(got) != 2 || got[0]["route"] != "/proxy/*" || got[0]["status"] != float64(200) ||
		got[1]["route"] != "unmatched" || got[1]["status"] != float64(404) {
		t.Errorf("GET should record the template /proxy/* and POST should land on the fallback and record unmatched, got=%v", got)
	}
}

func TestLog_StaticMissingFileKeepsRouteTemplate(t *testing.T) {
	// e.Static registers a real GET route (/static*) which answers 404 itself when the file does
	// not exist: recording its template keeps the cardinality bounded.
	lines := capture(t)
	e := echo.New()
	e.Use(Log())
	e.Static("/static", t.TempDir())
	w := httptest.NewRecorder()
	e.ServeHTTP(w, get("/static/missing.txt"))
	if l := lines()[0]; w.Code != 404 || l["route"] != "/static*" || l["status"] != float64(404) {
		t.Errorf("a missing static file should record 404 and /static*, got code=%d log=%v", w.Code, l)
	}
}

func TestLog_ErrorStatusIsTheRenderedResponse(t *testing.T) {
	// Regression (measured while designing this): echo renders the error only after the whole
	// chain returns, so at the moment the middleware receives it the response is unwritten, with
	// status 200 and Size 0. Without rendering in this layer every 404 and 500 is recorded as a
	// 200 with bytes_out 0.
	for _, c := range errorCases {
		lines := capture(t)
		w := httptest.NewRecorder()
		errorEcho(Log()).ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))

		if w.Code != c.status {
			t.Errorf("%s: the client should receive %d, got=%d", c.name, c.status, w.Code)
		}
		l := lines()[0]
		if l["status"] != float64(c.status) || l["bytes_out"] != float64(w.Body.Len()) || w.Body.Len() == 0 {
			t.Errorf("%s: the access log should record the status %d and the %d bytes actually sent, got status=%v bytes_out=%v",
				c.name, c.status, w.Body.Len(), l["status"], l["bytes_out"])
		}
		if l["route"] != c.route || l["errors"] != c.errs {
			t.Errorf("%s: route should be %q and errors should be %q, got=%v", c.name, c.route, c.errs, l)
		}
	}
}

func TestLog_ErrorBodyIsEchoDefaultAndHidesErrorText(t *testing.T) {
	// The response body keeps echo's default: a plain error answers Internal Server Error only,
	// and the error text goes to the log alone.
	capture(t)
	w := httptest.NewRecorder()
	errorEcho(Log()).ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	if got := strings.TrimSpace(w.Body.String()); got != `{"message":"Internal Server Error"}` || strings.Contains(got, "s3cret") {
		t.Errorf("the error text must not reach the response, got=%q", got)
	}
}

func TestLog_ErrorAfterResponseCommittedKeepsSentStatus(t *testing.T) {
	// The handler wrote a 200 and then returned an error: echo's default error handler sees
	// Committed and does nothing, so the client receives that 200. The log records 200 faithfully
	// and the error goes into errors.
	lines := capture(t)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		_ = c.String(200, "ok")
		return errors.New("after commit")
	})
	l := lines()[0]
	if w.Code != 200 || w.Body.String() != "ok" || l["status"] != float64(200) || l["bytes_out"] != float64(2) || l["errors"] != "after commit" {
		t.Errorf("the 200 that was sent and that error should both be recorded, got code=%d body=%q log=%v", w.Code, w.Body.String(), l)
	}
}

func TestFinish_HTTPErrorHandlerRunsOnceAcrossTheChain(t *testing.T) {
	// All three layers render the error in their own layer, and only the innermost actually
	// renders: nil goes outwards, since otherwise echo calls HTTPErrorHandler again and a custom
	// error handler that does not check Committed writes the error response twice.
	capture(t)
	_ = recording(t)
	withMetrics(t)
	calls := 0
	e := errorEcho(Trace(), Log(), Metric())
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		calls++
		_ = c.String(500, "E")
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	if calls != 1 || w.Body.String() != "E" {
		t.Errorf("HTTPErrorHandler should run exactly once, got calls=%d body=%q", calls, w.Body.String())
	}
}

func TestLog_ClientIPFollowsIPExtractor(t *testing.T) {
	// client_ip is c.RealIP(): computed by e.IPExtractor, which xecho set from TrustedProxies.
	lines := capture(t)
	e := newEcho([]echo.MiddlewareFunc{Log()}, statusOnly(200))
	e.IPExtractor = func(*http.Request) string { return "198.51.100.7" }
	e.ServeHTTP(httptest.NewRecorder(), get("/hello"))
	if l := lines()[0]; l["client_ip"] != "198.51.100.7" {
		t.Errorf("client_ip should be computed by the IPExtractor, got=%v", l["client_ip"])
	}
}

func TestLog_QueryIsRedacted(t *testing.T) {
	// Query strings often carry credentials (access_token, signatures), so with this on they are
	// masked field by field as well.
	lines := capture(t)
	serve(t, get("/hello?page=2&access_token=t0p"), []echo.MiddlewareFunc{Log(WithQuery(true))}, statusOnly(200))
	q, _ := lines()[0]["query"].(string)
	if !strings.Contains(q, "page=2") || strings.Contains(q, "t0p") {
		t.Errorf("query should keep page and mask access_token, got=%q", q)
	}
}

func TestLog_QueryOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello?page=2"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	if _, ok := lines()[0]["query"]; ok {
		t.Errorf("query should not be recorded by default, got=%v", lines()[0])
	}
}

func TestLog_ResponseHeadersAreRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithHeaders(false, true))}, func(c echo.Context) error {
		c.Response().Header().Set("X-Page-Total", "7")
		c.Response().Header().Set("Set-Cookie", "sid=abc123")
		return c.NoContent(200)
	})
	h, ok := lines()[0]["response_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Page-Total"]) != "7" || strings.Contains(fmt.Sprint(h), "abc123") {
		t.Errorf("response_headers should keep ordinary headers and mask Set-Cookie, got=%v", lines()[0]["response_headers"])
	}
}

func TestLog_ResponseHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	if _, ok := lines()[0]["response_headers"]; ok {
		t.Errorf("response_headers should not be recorded by default, got=%v", lines()[0])
	}
}

func TestLog_SkipsConfiguredPaths(t *testing.T) {
	lines := capture(t)
	e := echo.New()
	e.Use(Log(WithSkipPaths("/hello", "/internal/")))
	e.GET("/hello", statusOnly(200))
	e.GET("/internal/x", statusOnly(200))
	e.GET("/other", statusOnly(200))

	for _, p := range []string{"/hello", "/internal/x", "/other"} {
		e.ServeHTTP(httptest.NewRecorder(), get(p))
	}

	got := lines()
	if len(got) != 1 || got[0]["path"] != "/other" {
		t.Errorf("only /other should be recorded, got=%v", got)
	}
}

func TestLog_SkippedPathStillRendersErrors(t *testing.T) {
	// A skipped path is not logged, and its error is still rendered into a response by echo.
	capture(t)
	e := errorEcho(Log(WithSkipPaths("/plain")))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))
	if w.Code != 500 {
		t.Errorf("on a path skipped for logging the error response is unchanged, got=%d", w.Code)
	}
}

func TestLog_NoBodyByDefault(t *testing.T) {
	// Recording a body means buffering the whole request body and masking every field, which costs
	// and risks enough to be a decision.
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	serve(t, req, []echo.MiddlewareFunc{Log()}, func(c echo.Context) error { return c.String(200, "ok") })

	got := lines()[0]
	if _, has := got["request_body"]; has {
		t.Errorf("the request body should not be recorded by default, got=%v", got)
	}
}

func TestLog_RecordsRedactedBodyWhenEnabled(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"user":"alice","password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")

	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, true))}, func(c echo.Context) error {
		body, _ := io.ReadAll(c.Request().Body)
		if !strings.Contains(string(body), "alice") {
			t.Errorf("the downstream should still read the complete request body, got=%s", body)
		}
		return c.JSON(200, map[string]any{"token": secret, "ok": true})
	})

	got := lines()[0]
	reqBody, _ := got["request_body"].(string)
	if strings.Contains(reqBody, secret) {
		t.Errorf("the password in the request body should be masked, got=%v", reqBody)
	}
	if !strings.Contains(reqBody, "alice") {
		t.Errorf("non-sensitive fields should be kept, got=%v", reqBody)
	}
	respBody, _ := got["response_body"].(string)
	if respBody == "" || strings.Contains(respBody, secret) {
		t.Errorf("the response body should be recorded with its token masked, got=%v", respBody)
	}
}

func TestLog_RedactsRequestHeaders(t *testing.T) {
	lines := capture(t)
	req := get("/hello")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Visible", "keep-me")
	serve(t, req, []echo.MiddlewareFunc{Log(WithHeaders(true, false))}, statusOnly(200))

	h, ok := lines()[0]["request_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Visible"]) != "keep-me" || strings.Contains(fmt.Sprint(h), secret) {
		t.Errorf("request_headers should keep ordinary headers and mask credentials, got=%v", lines()[0]["request_headers"])
	}
}

func TestLog_RequestHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, statusOnly(200))
	if _, ok := lines()[0]["request_headers"]; ok {
		t.Errorf("request_headers should not be recorded by default, got=%v", lines()[0])
	}
}

func TestLog_SkipsFileUploadBody(t *testing.T) {
	// The content is useless for debugging while reading it costs the whole memory and time.
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("a lot of binary content"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, false))}, statusOnly(200))

	if b, _ := lines()[0]["request_body"].(string); !strings.Contains(b, "omitted") {
		t.Errorf("an uploaded file body must not be read into the log, got=%v", b)
	}
}

func TestLog_DoesNoWorkWhenLevelDisabled(t *testing.T) {
	// Buffering the body, wrapping the writer, masking and serializing - all of it exists to
	// assemble that one log line. Doing it with the level disabled pays the whole cost and throws
	// the result away.

	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf strings.Builder
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))

	read := false
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Body = &trackingBody{ReadCloser: req.Body, read: &read}

	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, true))}, statusOnly(200))

	if buf.Len() != 0 {
		t.Errorf("with the level disabled there should be no log, got=%s", buf.String())
	}
	if read {
		t.Error("with the level disabled the request body should not be read")
	}
}

type trackingBody struct {
	io.ReadCloser
	read *bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	*b.read = true
	return b.ReadCloser.Read(p)
}

func TestLog_PanicStillWritesAccessLog(t *testing.T) {
	// When the custom recover function blows up itself, the panic passes through the Log layer.
	lines := capture(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error { panic("boom") })
	}()

	if got := lines(); len(got) == 0 {
		t.Error("the access log should still be written when a panic passes through")
	}
}

func TestLog_BodyReadErrorDoesNotSwallowRequest(t *testing.T) {
	// Regression. If the fallback returned on a read error halfway through, that half of the
	// request body would be gone and the downstream handler would get an empty body - logging must
	// never be able to change the request itself.

	capture(t)

	const head = "the first half is still readable"
	req := httptest.NewRequest("POST", "/hello", &failingReader{data: head})
	req.Header.Set("Content-Type", "text/plain")
	req.GetBody = nil // force the "read it out and put it back" fallback

	var seen string
	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, false))}, func(c echo.Context) error {
		b, _ := io.ReadAll(c.Request().Body)
		seen = string(b)
		return c.NoContent(200)
	})

	if seen != head {
		t.Errorf("whatever was read should be handed back to the downstream, got=%q want=%q", seen, head)
	}
}

// failingReader yields some data and then fails.
type failingReader struct {
	data string
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("connection broken")
	}
	r.done = true
	return copy(p, r.data), nil
}

func TestLog_NeverLogsQueryString(t *testing.T) {
	// The access log records URL.Path, without the query string - in a request like
	// GET /login?token=hunter2 the credential sits in the URL.

	lines := capture(t)
	serve(t, get("/hello/42?token=hunter2&password=s3cret"), []echo.MiddlewareFunc{Log()},
		func(c echo.Context) error { return c.String(200, "ok") })

	got := lines()
	if len(got) != 1 {
		t.Fatalf("exactly one line is expected, got=%v", got)
	}
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"hunter2", "s3cret", "token=", "password="} {
		if bytes.Contains(raw, []byte(leaked)) {
			t.Errorf("%q from the query string appeared in the log: %s", leaked, raw)
		}
	}
}

func TestLog_StringResponseIsRecorded(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		return c.String(200, "hello alice")
	})

	if got, _ := lines()[0]["response_body"].(string); got != "hello alice" {
		t.Errorf("the response body written by c.String was not captured, got=%q", got)
	}
}

func TestLog_ResponseCaptureKeepsFlushWorking(t *testing.T) {
	// Capturing the response replaces the Writer inside echo.Response. Echo's Flush goes through
	// http.ResponseController: without the wrapper handing back the original writer (Unwrap), one
	// Flush in an SSE handler panics once response body logging is on.

	lines := capture(t)
	var flushed bool
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		c.Response().Header().Set("Content-Type", "text/event-stream")
		c.Response().WriteHeader(200)
		_, _ = c.Response().Write([]byte("data: 1\n\n"))
		c.Response().Flush()
		flushed = true
		return nil
	})
	if !flushed || !w.Flushed {
		t.Fatalf("Flush should reach the real writer, flushed=%v recorder.Flushed=%v", flushed, w.Flushed)
	}
	// Newlines are stripped on the way into the log (web.RedactBody, against log injection).
	if got, _ := lines()[0]["response_body"].(string); got != "data: 1" {
		t.Errorf("a streamed response body should be captured too, got=%q", got)
	}
}

func TestLog_ResponseWriterRestoredAfterRequest(t *testing.T) {
	// The capturing writer comes from a pool and has to be swapped back before the request ends:
	// an outer middleware still uses c.Response().
	capture(t)
	var inner, outer http.ResponseWriter
	outerMW := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			before := c.Response().Writer
			err := next(c)
			outer = c.Response().Writer
			if outer != before {
				t.Error("Response().Writer should be swapped back when the request ends")
			}
			return err
		}
	}
	serve(t, get("/hello"), []echo.MiddlewareFunc{outerMW, Log(WithBody(false, true))}, func(c echo.Context) error {
		inner = c.Response().Writer
		return c.String(200, "x")
	})
	if _, ok := inner.(*captureWriter); !ok {
		t.Errorf("the handler should see the capturing writer, got=%T", inner)
	}
}

func TestLog_StringResponseIsRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))},
		func(c echo.Context) error { return c.String(200, "token="+secret) })

	if got, _ := lines()[0]["response_body"].(string); strings.Contains(got, secret) {
		t.Errorf("a credential leaked into the log, got=%q", got)
	}
}

func TestLog_ResponseBodyTruncatedToPrefixOverLimit(t *testing.T) {
	lines := capture(t)
	big := strings.Repeat("a", maxResponseBody*2)
	w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))},
		func(c echo.Context) error { return c.String(200, big) })

	got, _ := lines()[0]["response_body"].(string)
	if len(got) != maxResponseBody {
		t.Errorf("the response body should be truncated to the first %d bytes, %d were recorded", maxResponseBody, len(got))
	}
	if w.Body.Len() != len(big) {
		t.Errorf("only the copy in the log is truncated; the client should receive all %d bytes, got=%d", len(big), w.Body.Len())
	}
}

func TestSnapshotBody_SkipsBinaryStream(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("\x00\x01\x02"))
	req.Header.Set("Content-Type", "application/octet-stream")
	serve(t, req, []echo.MiddlewareFunc{Log(WithBody(true, false))}, statusOnly(200))

	if b, _ := lines()[0]["request_body"].(string); !strings.Contains(b, "omitted") {
		t.Errorf("a binary stream must not be read into the log, got=%v", b)
	}
}

// serveAborted runs a request that writes a 200 and half a body and then aborts with
// http.ErrAbortHandler. Recover sits innermost, matching the order xecho assembles, and the panic
// it re-raises is caught here.

func serveAborted(t *testing.T, mws ...echo.MiddlewareFunc) {
	t.Helper()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler should travel all the way to net/http, got=%v", got)
		}
	}()
	serve(t, get("/hello"), append(mws, Recover(nil)), func(c echo.Context) error {
		_ = c.String(200, "partial")
		panic(http.ErrAbortHandler)
	})
}

func TestLog_ErrAbortHandlerAbortLoggedAs499(t *testing.T) {
	// An aborted request has usually written a 200 header already. Read from Response().Status
	// as is, a truncated response looks like an ordinary success in the access log.

	lines := capture(t)
	serveAborted(t, Log())

	got := lines()
	if len(got) != 1 {
		t.Fatalf("exactly one access log line is expected, got=%v", got)
	}
	if got[0]["status"] != float64(499) {
		t.Errorf("an aborted request should be recorded as 499, got=%v", got[0]["status"])
	}
	if e, _ := got[0]["errors"].(string); !strings.Contains(e, "net/http: abort Handler") {
		t.Errorf("errors should carry the reason for the abort, got=%v", got[0]["errors"])
	}
}

func TestLog_ErrorTextIsRedacted(t *testing.T) {
	// Returning an error is the normal thing to do in echo, and the error text goes into the
	// errors field: errors from drivers and downstreams often carry credentials. What goes is the
	// value under a sensitive name, while the sentence around it stays, see web.RedactText.
	for msg, want := range map[string]string{
		"db down":                       "db down",
		"token expired":                 "token expired", // names a credential and carries none
		"login failed password=hunter2": "login failed password=" + Redacted,
	} {
		lines := capture(t)
		serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error { return errors.New(msg) })
		if got := accessLogs(lines())[0]["errors"]; got != want {
			t.Errorf("errors=%v, %q should be recorded as %q", got, msg, want)
		}
	}
}

func TestLog_ErrorTheHandlerRendersItselfIsNotInTheErrorsField(t *testing.T) {
	// c.Error(err) hands the error to e.HTTPErrorHandler on the spot, which is what this layer
	// does with a returned one (see finish). A handler doing it itself and returning nil leaves
	// the layers outside nothing to record: the status code and the byte count are still the ones
	// sent, and only the text is missing.
	//
	// The boundary is pinned here because it is stated under "Access log" in xecho/README.md and
	// would have to be corrected there if it ever changed.
	// Both spellings of the same failure, so that what differs is only the one field.
	err := echo.NewHTTPError(http.StatusInternalServerError, "db is down")
	for name, h := range map[string]echo.HandlerFunc{
		"returned":                func(echo.Context) error { return err },
		"rendered by the handler": func(c echo.Context) error { c.Error(err); return nil },
	} {
		lines := capture(t)
		w := serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, h)

		l := accessLogs(lines())[0]
		if w.Code != 500 || w.Body.String() != "{\"message\":\"db is down\"}\n" {
			t.Errorf("%s: the client should receive the same response either way, got=%d %q", name, w.Code, w.Body.String())
		}
		if l["status"] != float64(500) || l["bytes_out"] != float64(25) {
			t.Errorf("%s: what went out should be recorded as it is, got=%v", name, l)
		}
		got, recorded := l["errors"]
		if want := name == "returned"; recorded != want {
			t.Errorf("%s: errors recorded=%v, want=%v, got=%v", name, recorded, want, got)
		}
		if recorded && got != "code=500, message=db is down" {
			t.Errorf("%s: the returned error should be recorded in full, got=%v", name, got)
		}
	}
}

func TestLog_DSNPasswordInHTTPErrorIsMasked(t *testing.T) {
	// Echo's HTTPError folds Internal into Error(): code=502, message=..., internal=<the driver's error>
	lines := capture(t)
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log()}, func(c echo.Context) error {
		return echo.NewHTTPError(502, "upstream").SetInternal(errors.New("dial postgres://app:hunter2@db:5432/prod failed"))
	})
	want := "code=502, message=upstream, internal=dial postgres://app:" + Redacted + "@db:5432/prod failed"
	if got := accessLogs(lines())[0]["errors"]; got != want {
		t.Errorf("errors=%v, want %q", got, want)
	}
}

func TestRecover_PanicValueIsRedacted(t *testing.T) {
	// A panic value carries credentials just as an error returned by a handler does, and putting
	// it into the error field as is writes the password to disk.
	for msg, want := range map[string]string{
		"dial app:hunter2@tcp(db:3306)/prod failed": "dial app:" + Redacted + "@tcp(db:3306)/prod failed",
		"login failed password=hunter2":             "login failed password=" + Redacted,
	} {
		lines := capture(t)
		serve(t, get("/hello"), []echo.MiddlewareFunc{Recover(nil)}, func(c echo.Context) error { panic(errors.New(msg)) })
		var got any = "(nothing logged)"
		for _, l := range lines() {
			if l["msg"] == "panic while handling request" {
				got = l["error"]
			}
		}
		if got != want {
			t.Errorf("the error field for panic(%q) is %v, want %q", msg, got, want)
		}
	}
}

func TestLog_EncodedResponseBodyOmitted(t *testing.T) {
	// Regression: Log sits outside echo's Gzip, so what it captures is compressed bytes: the JSON
	// does not parse, and the whole thing used to be recorded as ***REDACTED***, showing nothing.
	// A compressed body records no content, just an omitted note.

	lines := capture(t)
	req := get("/hello")
	req.Header.Set("Accept-Encoding", "gzip")
	body := strings.Repeat("hello ", 200)
	w := serve(t, req, []echo.MiddlewareFunc{Log(WithBody(false, true)), echomw.Gzip()}, func(c echo.Context) error {
		return c.JSON(200, map[string]string{"greeting": body})
	})
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("precondition: the response should be gzip compressed, got=%v", w.Header())
	}
	l := lines()[0]
	if got := l["response_body"]; got != "[gzip-encoded content omitted]" {
		t.Errorf("a compressed response body should record omitted, got=%q", got)
	}
	// Pins the number in the README: echo's Response.Size counts the bytes handed to Gzip, not
	// the ones on the wire.
	if want := len(`{"greeting":""}`+"\n") + len(body); l["bytes_out"] != float64(want) || w.Body.Len() >= want {
		t.Errorf("bytes_out should be the %d bytes before compression (%d on the wire), got=%v", want, w.Body.Len(), l["bytes_out"])
	}
}

func TestLog_ResponseCaptureSatisfiesFlusherAndHijacker(t *testing.T) {
	// Regression: with response body logging on, c.Response().Writer is the capturing writer. It
	// used to have Unwrap only: http.ResponseController and c.Response().Flush() found the writer
	// below, while code that type-asserts directly (older SSE and WebSocket libraries) failed.

	capture(t)
	var flusher, hijacker bool
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		_, flusher = c.Response().Writer.(http.Flusher)
		_, hijacker = c.Response().Writer.(http.Hijacker)
		return nil
	})
	if !flusher || !hijacker {
		t.Errorf("the capturing writer should satisfy http.Flusher=%v and http.Hijacker=%v", flusher, hijacker)
	}
}

func TestLog_ResponseCaptureFlushReachesClient(t *testing.T) {
	// On a real connection: the part Flush-ed through a type assertion has to reach the client
	// before the handler returns.
	capture(t)
	release := make(chan struct{})
	e := newEcho([]echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		c.Response().Header().Set("Content-Type", "text/event-stream")
		c.Response().WriteHeader(200)
		_, _ = c.Response().Write([]byte("data: 1\n\n"))
		c.Response().Writer.(http.Flusher).Flush()
		<-release
		return nil
	})
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer close(release)
	resp, err := http.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: 1\n" {
		t.Errorf("the client should read the first chunk after Flush, got=%q err=%v", line, err)
	}
}

func TestLog_ResponseCaptureHijack(t *testing.T) {
	// Hijack through a type assertion on a real connection yields the raw connection; when the
	// writer below does not support it, an error comes back, as in net/http.
	capture(t)
	e := newEcho([]echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		conn, buf, err := c.Response().Writer.(http.Hijacker).Hijack()
		if err != nil {
			return err
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\nhijack")
		return buf.Flush()
	})
	srv := httptest.NewServer(e)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hijack" {
		t.Errorf("Hijack should yield the raw connection, got=%q", b)
	}

	var herr error
	serve(t, get("/hello"), []echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		_, _, herr = c.Response().Writer.(http.Hijacker).Hijack() // httptest.ResponseRecorder does not support it
		return nil
	})
	if !errors.Is(herr, http.ErrNotSupported) {
		t.Errorf("when the writer below does not support Hijack, http.ErrNotSupported is expected, got=%v", herr)
	}
}

func TestLog_ResponseCaptureKeepsResponseControllerFeatures(t *testing.T) {
	// Beyond Flush and Hijack, http.ResponseController's read and write deadlines and full duplex
	// have to reach the real connection too: it walks down through Unwrap, and without the
	// capturing writer handing back the original everything returns ErrNotSupported.

	capture(t)
	var werr, rerr error
	e := newEcho([]echo.MiddlewareFunc{Log(WithBody(false, true))}, func(c echo.Context) error {
		rc := http.NewResponseController(c.Response())
		werr = rc.SetWriteDeadline(time.Now().Add(time.Minute))
		rerr = rc.SetReadDeadline(time.Now().Add(time.Minute))
		return c.String(200, "ok")
	})
	srv := httptest.NewServer(e)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if werr != nil || rerr != nil {
		t.Errorf("read and write deadlines should be set on the real connection, got write=%v read=%v", werr, rerr)
	}
}

func TestRequestUnchangedAfterServeHTTP(t *testing.T) {
	// The *http.Request handed in is never modified (net/http's contract): swapping the ctx uses
	// an r.WithContext copy, the caller of e.ServeHTTP gets its request back untouched, and handing
	// the same request in twice does not nest the ctx deeper.

	recording(t) // installs TraceContext: Propagate joins the upstream traceparent, so the ctx really changes
	for name, mw := range map[string]echo.MiddlewareFunc{"LogScope": LogScope(), "Trace": Trace(), "Propagate": Propagate()} {
		req := get("/hello")
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		before := req.Context()
		var inside context.Context
		serve(t, req, []echo.MiddlewareFunc{mw}, func(c echo.Context) error {
			inside = c.Request().Context()
			return nil
		})
		if req.Context() != before {
			t.Errorf("%s: the request handed in must not be modified", name)
		}
		if inside == before {
			t.Errorf("%s: the handler should see the swapped ctx", name)
		}
	}
}
