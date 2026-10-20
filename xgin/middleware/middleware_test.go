package middleware

import (
	"bytes"
	"compress/gzip"
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

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/xlog"
)

func init() { gin.SetMode(gin.TestMode) }

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

// serve installs the middleware and runs one request.
func serve(t *testing.T, req *http.Request, mws []gin.HandlerFunc, h gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	e := gin.New()
	e.Use(mws...)
	e.Any("/hello/:id", h)
	e.Any("/hello", h)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func get(path string) *http.Request { return httptest.NewRequest("GET", path, nil) }

// ---- LogScope ----

func TestLogScope_HandlersCanAddFieldsAtAnyDepth(t *testing.T) {
	// Deep in the call stack there is no *gin.Context, so there was never a chance to pass a new
	// context back up.
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{LogScope(), Log()}, func(c *gin.Context) {
		deepInBusinessCode(c.Request.Context())
		c.String(200, "ok")
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
	lines := capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		panic("boom")
	})

	if w.Code != 500 {
		t.Errorf("a 500 is expected, got=%d", w.Code)
	}
	got := lines()
	if len(got) == 0 || got[0]["level"] != "ERROR" {
		t.Fatalf("exactly one error log line is expected, got=%v", got)
	}
	if got[0]["stack"] == nil || !strings.Contains(got[0]["stack"].(string), "middleware_test.go") {
		t.Errorf("the stack should be included, got=%v", got[0])
	}
	if got[0]["path"] != "/hello" {
		t.Errorf("the request path should be included, got=%v", got[0])
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
		serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
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
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(func(c *gin.Context, _ any) {
		c.JSON(503, gin.H{"msg": "try again later"})
	})}, func(c *gin.Context) { panic("boom") })

	if w.Code != 503 || !strings.Contains(w.Body.String(), "try again later") {
		t.Errorf("the custom response should be used, got=%d %s", w.Code, w.Body.String())
	}
}

func TestRecover_KeepsStatusOnceResponseStarted(t *testing.T) {
	// Once the response has started going out, changing the status code only yields a truncated
	// response.
	capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		c.String(200, "part of this already went out")
		panic("boom mid-write")
	})

	if w.Code != 200 {
		t.Errorf("a response already written must not be turned into a 500, got=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "part of this already went out") {
		t.Errorf("what was written should remain, got=%s", w.Body.String())
	}
}

func TestRecover_NoOpWithoutPanic(t *testing.T) {
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		c.String(201, "ok")
	})
	if w.Code != 201 {
		t.Errorf("a normal request must not be interfered with, got=%d", w.Code)
	}
}

// ---- Log ----

func TestLog_RecordsKeyFields(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello/42?q=1"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		c.String(201, "ok")
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
	if l["elapsed_ms"] == nil || l["client_ip"] == nil {
		t.Errorf("the duration and the client should be recorded, got=%v", l)
	}
}

func TestLog_RecordsRequestAndResponseSizes(t *testing.T) {
	// With body logging on the request body is buffered and the response is captured, and both
	// byte counts still have to be right.
	for name, opts := range map[string][]LogOption{"defaults": nil, "body recorded": {WithBody(true, true)}} {
		lines := capture(t)
		req := httptest.NewRequest("POST", "http://api.example.com/hello/1", strings.NewReader("12345"))
		req.Header.Set("User-Agent", "probe/1.0")
		serve(t, req, []gin.HandlerFunc{Log(opts...)}, func(c *gin.Context) { c.String(200, "hello!") })
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
	// With no response body written, gin's Size() is -1, which recorded as is looks like a failure.
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(204) })
	if l := lines()[0]; l["bytes_out"] != float64(0) {
		t.Errorf("with no response body written, bytes_out should be 0, got=%v", l["bytes_out"])
	}
}

func TestLog_ElapsedIsMilliseconds(t *testing.T) {
	// slog's JSON writes a Duration as an integer of nanoseconds: a 20ms request is recorded as
	// 20000000, a million times off for an alert threshold configured in "milliseconds".
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		time.Sleep(20 * time.Millisecond)
		c.Status(200)
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
	serve(t, get("/nope/1"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {})
	l := lines()[0]
	if l["route"] != "unmatched" || l["path"] != "/nope/1" || l["status"] != float64(404) {
		t.Errorf("an unmatched request should have route unmatched and path as the real path, got=%v", l)
	}
}

func TestLog_QueryIsRedacted(t *testing.T) {
	// Query strings often carry credentials (access_token, signatures), so with this on they are
	// masked field by field as well.
	lines := capture(t)
	serve(t, get("/hello?page=2&access_token=t0p"), []gin.HandlerFunc{Log(WithQuery(true))}, func(c *gin.Context) {
		c.Status(200)
	})
	q, _ := lines()[0]["query"].(string)
	if !strings.Contains(q, "page=2") || strings.Contains(q, "t0p") {
		t.Errorf("query should keep page and mask access_token, got=%q", q)
	}
}

func TestLog_QueryOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello?page=2"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(200) })
	if _, ok := lines()[0]["query"]; ok {
		t.Errorf("query should not be recorded by default, got=%v", lines()[0])
	}
}

func TestLog_ResponseHeadersAreRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log(WithHeaders(false, true))}, func(c *gin.Context) {
		c.Header("X-Page-Total", "7")
		c.Header("Set-Cookie", "sid=abc123")
		c.Status(200)
	})
	h, ok := lines()[0]["response_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Page-Total"]) != "7" || strings.Contains(fmt.Sprint(h), "abc123") {
		t.Errorf("response_headers should keep ordinary headers and mask Set-Cookie, got=%v", lines()[0]["response_headers"])
	}
}

func TestLog_ResponseHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(200) })
	if _, ok := lines()[0]["response_headers"]; ok {
		t.Errorf("response_headers should not be recorded by default, got=%v", lines()[0])
	}
}

func TestLog_SkipsConfiguredPaths(t *testing.T) {
	lines := capture(t)
	mws := []gin.HandlerFunc{Log(WithSkipPaths("/hello", "/internal/"))}
	e := gin.New()
	e.Use(mws...)
	e.GET("/hello", func(c *gin.Context) { c.Status(200) })
	e.GET("/internal/x", func(c *gin.Context) { c.Status(200) })
	e.GET("/other", func(c *gin.Context) { c.Status(200) })

	for _, p := range []string{"/hello", "/internal/x", "/other"} {
		e.ServeHTTP(httptest.NewRecorder(), get(p))
	}

	got := lines()
	if len(got) != 1 || got[0]["path"] != "/other" {
		t.Errorf("only /other should be recorded, got=%v", got)
	}
}

func TestLog_NoBodyByDefault(t *testing.T) {
	// Recording a body means buffering the whole request body and masking every field, which costs
	// and risks enough to be a decision.
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	serve(t, req, []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.String(200, "ok") })

	got := lines()[0]
	if _, has := got["request_body"]; has {
		t.Errorf("the request body should not be recorded by default, got=%v", got)
	}
}

func TestLog_RecordsRedactedBodyWhenEnabled(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"user":"alice","password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")

	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, true))}, func(c *gin.Context) {
		body, _ := c.GetRawData()
		if !strings.Contains(string(body), "alice") {
			t.Errorf("the downstream should still read the complete request body, got=%s", body)
		}
		c.JSON(200, gin.H{"token": secret, "ok": true})
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
	if strings.Contains(respBody, secret) {
		t.Errorf("the token in the response body should be masked too, got=%v", respBody)
	}
}

func TestLog_RedactsRequestHeaders(t *testing.T) {
	lines := capture(t)
	req := get("/hello")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Visible", "keep-me")
	serve(t, req, []gin.HandlerFunc{Log(WithHeaders(true, false))}, func(c *gin.Context) { c.Status(200) })

	h, ok := lines()[0]["request_headers"].(map[string]any)
	if !ok || fmt.Sprint(h["X-Visible"]) != "keep-me" || strings.Contains(fmt.Sprint(h), secret) {
		t.Errorf("request_headers should keep ordinary headers and mask credentials, got=%v", lines()[0]["request_headers"])
	}
}

func TestLog_RequestHeadersOffByDefault(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(200) })
	if _, ok := lines()[0]["request_headers"]; ok {
		t.Errorf("request_headers should not be recorded by default, got=%v", lines()[0])
	}
}

func TestLog_SkipsFileUploadBody(t *testing.T) {
	// The content is useless for debugging while reading it costs the whole memory and time.
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("a lot of binary content"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, false))}, func(c *gin.Context) { c.Status(200) })

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

	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, true))}, func(c *gin.Context) { c.Status(200) })

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
	// When a caller's RecoveryFunc blows up itself, the panic passes through the Log layer.
	lines := capture(t)
	defer func() { recover() }()

	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { panic("boom") })
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
	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, false))}, func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		seen = string(b)
		c.Status(200)
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
	// GET /login?token=hunter2 the credential sits in the URL. Switching to RequestURI or
	// URL.String() both look like "recording a bit more", and both write credentials into the log
	// in the clear with nothing to show for it. Without a test watching this, it gets changed
	// sooner or later.
	lines := capture(t)
	serve(t, get("/hello/42?token=hunter2&password=s3cret"), []gin.HandlerFunc{Log()},
		func(c *gin.Context) { c.String(200, "ok") })

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

func TestLog_c_String_ResponseIsRecorded(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("GET", "/hello", nil)
	serve(t, req, []gin.HandlerFunc{Log(WithBody(false, true))}, func(c *gin.Context) {
		c.String(200, "hello alice")
	})

	if got, _ := lines()[0]["response_body"].(string); got != "hello alice" {
		t.Errorf("the response body written by c.String was not captured, got=%q", got)
	}
}

func TestLog_handler_DirectWriteStringIsRecorded(t *testing.T) {
	// gin's ResponseWriter interface carries WriteString and handlers calling it directly is a
	// common thing to do. With the wrapper capturing Write alone, a response written that way is
	// always empty in the log.
	lines := capture(t)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) {
			c.Header("Content-Type", "text/plain")
			c.Status(200)
			if _, err := c.Writer.WriteString("hello alice"); err != nil {
				t.Error(err)
			}
		})

	if got, _ := lines()[0]["response_body"].(string); got != "hello alice" {
		t.Errorf("the response body written by WriteString was not captured, got=%q", got)
	}
}

func TestLog_WriteString_TruncatesToPrefixOverLimit(t *testing.T) {
	lines := capture(t)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) {
			c.Header("Content-Type", "text/plain")
			c.Status(200)
			_, _ = c.Writer.WriteString(strings.Repeat("a", maxResponseBody*2))
		})

	if got, _ := lines()[0]["response_body"].(string); len(got) > maxResponseBody {
		t.Errorf("the response body was not truncated, %d bytes were recorded", len(got))
	}
}

func TestLog_c_String_ResponseIsRedacted(t *testing.T) {
	lines := capture(t)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) { c.String(200, "token="+secret) })

	if got, _ := lines()[0]["response_body"].(string); strings.Contains(got, secret) {
		t.Errorf("a credential leaked into the log, got=%q", got)
	}
}

func TestLog_ResponseBodyTruncatedToPrefixOverLimit(t *testing.T) {
	lines := capture(t)
	big := strings.Repeat("a", maxResponseBody*2)
	serve(t, httptest.NewRequest("GET", "/hello", nil),
		[]gin.HandlerFunc{Log(WithBody(false, true))},
		func(c *gin.Context) { c.String(200, big) })

	got, _ := lines()[0]["response_body"].(string)
	if len(got) > maxResponseBody {
		t.Errorf("the response body was not truncated, %d bytes were recorded", len(got))
	}
}

func TestSnapshotBody_SkipsBinaryStream(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("\x00\x01\x02"))
	req.Header.Set("Content-Type", "application/octet-stream")
	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, false))}, func(c *gin.Context) { c.Status(200) })

	if b, _ := lines()[0]["request_body"].(string); !strings.Contains(b, "omitted") {
		t.Errorf("a binary stream must not be read into the log, got=%v", b)
	}
}

// serveAborted runs a request that writes a 200 and half a body and then aborts with
// http.ErrAbortHandler. Recover sits innermost, matching the order xgin assembles, and the panic
// it re-raises is caught here.
func serveAborted(t *testing.T, mws ...gin.HandlerFunc) {
	t.Helper()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler should travel all the way to net/http, got=%v", got)
		}
	}()
	serve(t, get("/hello"), append(mws, Recover(nil)), func(c *gin.Context) {
		c.String(200, "partial")
		panic(http.ErrAbortHandler)
	})
}

func TestLog_ErrAbortHandlerAbortLoggedAs499(t *testing.T) {
	// An aborted request has usually written a 200 header already. Read from c.Writer.Status() as
	// is, a truncated response looks like an ordinary success in the access log.
	lines := capture(t)
	serveAborted(t, Log())

	got := lines()
	if len(got) != 1 {
		t.Fatalf("exactly one access log line is expected, got=%v", got)
	}
	if got[0]["status"] != float64(statusAborted) {
		t.Errorf("an aborted request should be recorded as %d, got=%v", statusAborted, got[0]["status"])
	}
	if e, _ := got[0]["errors"].(string); !strings.Contains(e, "net/http: abort Handler") {
		t.Errorf("errors should carry the reason for the abort, got=%v", got[0]["errors"])
	}
}

func TestLog_ErrorTextIsRedacted(t *testing.T) {
	// An error recorded with c.Error reaches the errors field as is; errors from drivers and
	// downstreams often carry credentials.
	for msg, want := range map[string]string{"db down": "db down", "login failed password=hunter2": Redacted} {
		lines := capture(t)
		serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
			_ = c.Error(errors.New(msg))
			c.Status(500)
		})
		got, _ := lines()[0]["errors"].(string)
		if want == Redacted && got != Redacted || want != Redacted && !strings.Contains(got, msg) {
			t.Errorf("errors=%q, %q should be recorded as %q", got, msg, want)
		}
	}
}

func TestLog_DSNPasswordInErrorIsMasked(t *testing.T) {
	// A driver's error often carries the whole DSN without a single sensitive word in it: going by
	// the word list alone would put the password into the errors field as is.
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		_ = c.Error(errors.New("dial postgres://app:hunter2@db:5432/prod failed"))
		c.Status(500)
	})
	got, _ := lines()[0]["errors"].(string)
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "postgres://app:"+Redacted+"@db:5432/prod") {
		t.Errorf("the DSN password in errors should be masked and the rest kept, got=%q", got)
	}
}

func TestLog_MultipleErrorsStaySeparated(t *testing.T) {
	// c.Errors.String() puts one error per line; stripping newlines outright glues two errors into
	// one sentence.
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		_ = c.Error(errors.New("a"))
		_ = c.Error(errors.New("b"))
		c.Status(500)
	})
	if got := lines()[0]["errors"]; got != "Error #01: a; Error #02: b" {
		t.Errorf("two errors should be separated by ;, got=%q", got)
	}
}

func TestRecover_PanicValueIsRedacted(t *testing.T) {
	// A panic value carries credentials just as an error returned by a handler does, and putting it
	// into the error field as is writes the password to disk.
	for msg, want := range map[string]string{
		"dial postgres://app:hunter2@db:5432/prod failed": "dial postgres://app:" + Redacted + "@db:5432/prod failed",
		"login failed password=hunter2":                   Redacted,
	} {
		lines := capture(t)
		serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) { panic(errors.New(msg)) })
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

// gzipWriter is a compression middleware for the tests: like gin-contrib/gzip it replaces
// c.Writer, gzipping what is written before passing it down.
type gzipWriter struct {
	gin.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipWriter) Write(b []byte) (int, error)       { return w.gz.Write(b) }
func (w *gzipWriter) WriteString(s string) (int, error) { return w.gz.Write([]byte(s)) }

func gzipped(c *gin.Context) {
	c.Header("Content-Encoding", "gzip")
	gz := gzip.NewWriter(c.Writer)
	c.Writer = &gzipWriter{ResponseWriter: c.Writer, gz: gz}
	c.Next()
	_ = gz.Close()
}

func TestLog_EncodedResponseBodyOmitted(t *testing.T) {
	// Regression: Log sits outside the compression middleware, so what it captures is compressed
	// bytes, and the whole binary mess used to reach response_body. A compressed body records no
	// content, just an omitted note.
	lines := capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Log(WithBody(false, true)), gzipped}, func(c *gin.Context) {
		c.String(200, strings.Repeat("hello ", 200))
	})
	l := lines()[0]
	if got := l["response_body"]; got != "[gzip-encoded content omitted]" {
		t.Errorf("a compressed response body should record omitted, got=%q", got)
	}
	// On gin's side bytes_out is the on-wire (compressed) count: Log reads the writer below the
	// compression middleware.
	if l["bytes_out"] != float64(w.Body.Len()) {
		t.Errorf("bytes_out should be the compressed %d bytes, got=%v", w.Body.Len(), l["bytes_out"])
	}
}

func TestRecover_BrokenPipeLoggedAsWarnWithoutStack(t *testing.T) {
	// A client disconnecting early is not a failure: one WARN connection broken line, no stack.
	// It used to be ERROR, which made the alerts follow the client's flaky network.
	lines := capture(t)
	broken := &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: errors.New("broken pipe")}}
	serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) { panic(broken) })

	var sawBroken bool
	for _, l := range lines() {
		if l["msg"] == "panic while handling request" {
			t.Errorf("a broken connection must not be logged as a panic with a stack, got=%v", l)
		}
		if l["msg"] == "connection broken" {
			sawBroken = l["level"] == "WARN"
		}
	}
	if !sawBroken {
		t.Error("exactly one WARN connection broken line is expected")
	}
}
