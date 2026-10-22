package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xbeego/internal/reqstate"
	"github.com/jupiter-eng/xone/xlog"
)

// ---- Helpers ----

// chain wraps h in mws, the first outermost.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// serve sends one request through mws into h.
func serve(t *testing.T, req *http.Request, mws []func(http.Handler) http.Handler, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	chain(h, mws...).ServeHTTP(w, req)
	return w
}

func get(target string) *http.Request { return httptest.NewRequest("GET", target, nil) }

// routed is a handler that fills in the route template, which is what xbeego's beego filter does
// for real.
func routed(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s := reqstate.From(r.Context()); s != nil {
			s.Route = route
		}
		h(w, r)
	}
}

// captureLog collects slog's records as maps for the length of the test.
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
		buf.WriteString(string(p))
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

// xlogCapture replaces the default logger with a real xlog writing to a file, and returns the
// records written so far.
//
// It has to be xlog rather than any slog handler: the fields LogScope puts into the context are
// pulled back out by xlog's handler, and another handler shows none of them -- which is exactly
// what "LogScope only works with xlog" means in practice.
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
	t.Cleanup(func() { slog.SetDefault(old); _ = closer.Close() })

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

// only is the one record whose msg matches, or a failure.
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

type proxies struct{ trust bool }

func (p proxies) Trusts(string) bool { return p.trust }
func (p proxies) ClientIP(r *http.Request) string {
	if p.trust {
		return r.Header.Get("X-Forwarded-For")
	}
	return reqstate.From(r.Context()).ClientIP(r)
}

// ---- Capture ----

func TestCapture_InstallsTheStateAndTheRecorder(t *testing.T) {
	var st *reqstate.State
	serve(t, get("/ping"), []func(http.Handler) http.Handler{Capture(nil)},
		func(w http.ResponseWriter, r *http.Request) {
			st = reqstate.From(r.Context())
			w.WriteHeader(201)
		})

	if st == nil || st.Rec == nil {
		t.Fatal("Capture should put a state with a recorder on the request")
	}
	if st.Rec.Status() != 201 {
		t.Errorf("the recorder should have seen the status, got=%d", st.Rec.Status())
	}
}

func TestCapture_IsIdempotent(t *testing.T) {
	// Two of them -- xbeego's own plus one a caller mounted -- must not wrap the writer twice, or
	// the bytes would be counted once per layer.
	var seen []*reqstate.State
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, reqstate.From(r.Context()))
		_, _ = w.Write([]byte("hello"))
	}), Capture(nil), Capture(nil))
	h.ServeHTTP(httptest.NewRecorder(), get("/ping"))

	if len(seen) != 1 || seen[0] == nil {
		t.Fatalf("the handler should run once with a state, got=%v", seen)
	}
	if n := seen[0].Rec.Bytes(); n != 5 {
		t.Errorf("the bytes should be counted once, got=%d", n)
	}
}

func TestCapture_TrustAndClientIP(t *testing.T) {
	req := get("/ping")
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	var trusted bool
	var ip string
	serve(t, req, []func(http.Handler) http.Handler{Capture(proxies{trust: true})},
		func(_ http.ResponseWriter, r *http.Request) {
			trusted = reqstate.From(r.Context()).Trusted
			ip = ClientIP(r)
		})
	if !trusted || ip != "1.2.3.4" {
		t.Errorf("a trusted peer's forwarded header should be followed, got trusted=%v ip=%q", trusted, ip)
	}

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil)},
		func(_ http.ResponseWriter, r *http.Request) {
			trusted = reqstate.From(r.Context()).Trusted
			ip = ClientIP(r)
		})
	if trusted || ip != "10.0.0.1" {
		t.Errorf("without a proxy list nothing is trusted, got trusted=%v ip=%q", trusted, ip)
	}
}

func TestClientIP_OutsideARequest(t *testing.T) {
	req := get("/ping")
	req.RemoteAddr = "203.0.113.9:1"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(req); got != "203.0.113.9" {
		t.Errorf("without the middleware the peer alone is used, got=%q", got)
	}
}

// ---- The middleware works on its own ----

func TestMiddleware_WorkOnTheirOwn(t *testing.T) {
	// Mounted with web.RunWithMiddleWares and no Capture in front, whichever runs outermost has to
	// install the state the others read.
	lines := captureLog(t)
	serve(t, get("/ping"), []func(http.Handler) http.Handler{Log()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })

	a := only(t, lines(), "request completed")
	if a["status"] != float64(204) {
		t.Errorf("the status should be recorded without Capture in front, got=%v", a["status"])
	}
	// Nobody filled the route in, so it is the fixed value rather than a guess from the path.
	if a["route"] != "unmatched" {
		t.Errorf("without beego's filter the route is unmatched, got=%v", a["route"])
	}
}

// ---- LogScope ----

func TestLogScope_FieldsReachTheAccessLog(t *testing.T) {
	// Business code deep in the call stack has no beego context to pass back up, which is what the
	// scope is for.
	lines := xlogCapture(t)
	serve(t, get("/ping"), []func(http.Handler) http.Handler{Capture(nil), LogScope(), Log()},
		func(w http.ResponseWriter, r *http.Request) {
			deepInBusinessCode(r.Context())
			w.WriteHeader(200)
		})

	got := lines()
	if len(got) == 0 {
		t.Fatal("no access log")
	}
	if last := got[len(got)-1]; last["tenant"] != "acme" {
		t.Errorf("a field added during the request should be on the access log, got=%v", last)
	}
}

// deepInBusinessCode is the shape this is for: no beego context in sight, only the ctx.
func deepInBusinessCode(ctx context.Context) { xlog.AddKV(ctx, "tenant", "acme") }

// ---- status ----

func TestStatus_ClientGoneIsRecordedAs499(t *testing.T) {
	// A handler that returned without writing anything after the client left would otherwise be
	// recorded as a 200 nobody will ever receive.
	lines := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := get("/ping").WithContext(ctx)
	cancel()

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log()},
		func(http.ResponseWriter, *http.Request) {})

	if a := only(t, lines(), "request completed"); a["status"] != float64(499) {
		t.Errorf("a request whose client left should be recorded as 499, got=%v", a["status"])
	}
}

func TestStatus_ClientGoneButAnswered(t *testing.T) {
	// Having written a response, the status that went out is the real one.
	lines := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := get("/ping").WithContext(ctx)
	cancel()

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	if a := only(t, lines(), "request completed"); a["status"] != float64(200) {
		t.Errorf("an answered request keeps its status, got=%v", a["status"])
	}
}

// ---- Recover ----

func TestRecover_LogsAndAnswers500(t *testing.T) {
	lines := captureLog(t)
	w := serve(t, get("/boom"), []func(http.Handler) http.Handler{Capture(nil), Recover(nil)},
		func(http.ResponseWriter, *http.Request) { panic("dsn=postgres://u:p@host/db") })

	if w.Code != 500 {
		t.Errorf("a panic should answer 500, got=%d", w.Code)
	}
	l := only(t, lines(), "panic while handling request")
	if msg, _ := l["error"].(string); strings.Contains(msg, "p@host") {
		t.Errorf("a credential inside the panic value should be masked, got=%v", msg)
	}
	if stack, _ := l["stack"].(string); stack == "" {
		t.Error("the stack should be recorded")
	}
	if l["path"] != "/boom" || l["method"] != "GET" {
		t.Errorf("the request should be identifiable, got=%v", l)
	}
}

func TestRecover_CustomResponse(t *testing.T) {
	captureLog(t)
	w := serve(t, get("/boom"), []func(http.Handler) http.Handler{
		Capture(nil),
		Recover(func(w http.ResponseWriter, _ *http.Request, _ any) { w.WriteHeader(503) }),
	}, func(http.ResponseWriter, *http.Request) { panic("boom") })

	if w.Code != 503 {
		t.Errorf("the custom response should be used, got=%d", w.Code)
	}
}

func TestRecover_ResponseAlreadyStarted(t *testing.T) {
	// Once the response is going out, the status code can no longer be changed and anything more
	// written is appended to a half-sent body -- a truncated page with an error message glued to
	// the end of it. The answer is not attempted at all.
	captureLog(t)
	var called bool
	w := serve(t, get("/boom"), []func(http.Handler) http.Handler{
		Capture(nil),
		Recover(func(w http.ResponseWriter, _ *http.Request, _ any) {
			called = true
			w.WriteHeader(500)
			_, _ = w.Write([]byte("error"))
		}),
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("half "))
		panic("boom")
	})

	if called {
		t.Error("the recovery response must not be attempted once the response has started")
	}
	if w.Code != 200 || w.Body.String() != "half " {
		t.Errorf("what went out should stay as it was, got=%d %q", w.Code, w.Body.String())
	}
}

func TestRecover_ErrAbortHandlerTravelsOn(t *testing.T) {
	// net/http breaks the connection for it; answering a 500 would send a response the handler
	// deliberately abandoned.
	captureLog(t)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		serve(t, get("/boom"), []func(http.Handler) http.Handler{Capture(nil), Recover(nil)},
			func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	}()

	if recovered != http.ErrAbortHandler {
		t.Errorf("http.ErrAbortHandler should be re-panicked, got=%v", recovered)
	}
}

func TestRecover_AbortIsRecordedAs499(t *testing.T) {
	lines := captureLog(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/boom"), []func(http.Handler) http.Handler{Capture(nil), Log(), Recover(nil)},
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(200) // a response has already started going out
				panic(http.ErrAbortHandler)
			})
	}()

	a := only(t, lines(), "request completed")
	if a["status"] != float64(499) {
		t.Errorf("an aborted request should not be recorded as the 200 that was truncated, got=%v", a["status"])
	}
	if errs, _ := a["errors"].(string); !strings.Contains(errs, "aborted") {
		t.Errorf("the abort should be visible in the errors field, got=%v", errs)
	}
}

func TestRecover_BrokenPipeIsAWarning(t *testing.T) {
	// A client's flaky network should not fire the service's alerts.
	lines := captureLog(t)
	broken := &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}}
	w := serve(t, get("/ping"), []func(http.Handler) http.Handler{Capture(nil), Recover(nil)},
		func(http.ResponseWriter, *http.Request) { panic(broken) })

	l := only(t, lines(), "connection broken")
	if l["level"] != "WARN" {
		t.Errorf("a broken connection is not an error, got=%v", l["level"])
	}
	if _, ok := l["stack"]; ok {
		t.Error("a broken connection needs no stack")
	}
	// Nothing can be written to a connection that is gone, so no status is forced onto it.
	if w.Code != 200 {
		t.Errorf("no response should be attempted, got=%d", w.Code)
	}
}

func TestOnPanic_UsesTheAnswerOnTheRequest(t *testing.T) {
	// This is how beego's single global recovery slot answers with the right instance's response.
	captureLog(t)
	req := get("/boom")
	rec := reqstate.NewRecorder(httptest.NewRecorder())
	ctx, st := reqstate.New(req.Context(), req, nil, rec)
	st.Recover = func(w http.ResponseWriter, _ *http.Request, _ any) { w.WriteHeader(418) }
	req = req.WithContext(ctx)

	func() {
		defer func() {
			if r := recover(); r != nil {
				OnPanic(rec, req, r, nil)
			}
		}()
		panic("boom")
	}()

	if rec.Status() != 418 {
		t.Errorf("the answer carried by the request should be used, got=%d", rec.Status())
	}
}

func TestOnPanic_WithoutAStateStillAnswers(t *testing.T) {
	captureLog(t)
	req := get("/boom")
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				OnPanic(w, req, r, nil)
			}
		}()
		panic(errors.New("boom"))
	}()

	if w.Code != 500 {
		t.Errorf("a bare 500 is the fallback, got=%d", w.Code)
	}
}

// quietAboveWarn raises slog's level above Info for the length of the test.
func quietAboveWarn(t *testing.T) {
	t.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
}
