package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/internal/web"
)

func TestLog_Fields(t *testing.T) {
	// The field names are what the dashboards and the alerts on the logging platform go by, and
	// they are the same on all three web integrations because they live in internal/web.
	lines := captureLog(t)
	req := httptest.NewRequest("POST", "/users/42", strings.NewReader("hello"))
	req.RemoteAddr = "10.1.2.3:5000"
	req.Header.Set("User-Agent", "probe/1.0")

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log()},
		routed("/users/:id", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(201)
			_, _ = w.Write([]byte("created"))
		}))

	a := only(t, lines(), "request completed")
	for field, want := range map[string]any{
		"method":     "POST",
		"route":      "/users/:id",
		"path":       "/users/42",
		"status":     float64(201),
		"client_ip":  "10.1.2.3",
		"host":       "example.com",
		"proto":      "HTTP/1.1",
		"user_agent": "probe/1.0",
		"bytes_in":   float64(5),
		"bytes_out":  float64(len("created")),
	} {
		if a[field] != want {
			t.Errorf("%s should be %v, got=%v", field, want, a[field])
		}
	}
	if _, ok := a["elapsed_ms"].(float64); !ok {
		t.Errorf("elapsed_ms should be a number of milliseconds, got=%v", a["elapsed_ms"])
	}
	// Off by default: each of them costs, and a query string is a common place for credentials.
	for _, field := range []string{"query", "request_body", "response_body", "request_headers", "response_headers"} {
		if _, ok := a[field]; ok {
			t.Errorf("%s should not be recorded unless asked for, got=%v", field, a[field])
		}
	}
}

func TestLog_PathNeverCarriesTheQueryString(t *testing.T) {
	// GET /login?token=... would otherwise put the credential into the log in plaintext.
	lines := captureLog(t)
	serve(t, get("/login?token=s3cret"), []func(http.Handler) http.Handler{Capture(nil), Log()},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	a := only(t, lines(), "request completed")
	if strings.Contains(a["path"].(string), "token") {
		t.Errorf("path must be the path alone, got=%v", a["path"])
	}
}

func TestLog_SkipPaths(t *testing.T) {
	lines := captureLog(t)
	mws := []func(http.Handler) http.Handler{Capture(nil), Log(WithSkipPaths("/healthz", "/static/"))}
	for _, p := range []string{"/healthz", "/static/app.js", "/static/", "/api"} {
		serve(t, get(p), mws, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	}

	// Exactly one survives: a path ending in / matches by prefix, the rest match exactly.
	a := only(t, lines(), "request completed")
	if a["path"] != "/api" {
		t.Errorf("only the unskipped path should be logged, got=%v", a["path"])
	}
}

func TestLog_RequestBodyIsRedactedAndStillReachesTheHandler(t *testing.T) {
	lines := captureLog(t)
	var seen string
	req := httptest.NewRequest("POST", "/login", strings.NewReader(`{"user":"a","password":"s3cret"}`))
	req.Header.Set("Content-Type", "application/json")

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log(WithBody(true, false))},
		func(w http.ResponseWriter, r *http.Request) {
			b := make([]byte, 64)
			n, _ := r.Body.Read(b)
			seen = string(b[:n])
			w.WriteHeader(200)
		})

	a := only(t, lines(), "request completed")
	body := a["request_body"].(string)
	if strings.Contains(body, "s3cret") || !strings.Contains(body, Redacted) {
		t.Errorf("the password should be masked, got=%v", body)
	}
	if !strings.Contains(seen, "s3cret") {
		t.Errorf("writing a log must not take the body away from the handler, got=%q", seen)
	}
}

func TestLog_MultipartBodyIsNotRead(t *testing.T) {
	// Reading an upload costs the whole of the memory and the time while helping nothing.
	lines := captureLog(t)
	req := httptest.NewRequest("POST", "/upload", strings.NewReader("....binary...."))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log(WithBody(true, false))},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	if body := only(t, lines(), "request completed")["request_body"].(string); !strings.Contains(body, "omitted") {
		t.Errorf("an upload should be recorded as omitted, got=%v", body)
	}
}

func TestLog_ResponseBodyIsCappedAndRedacted(t *testing.T) {
	lines := captureLog(t)
	big := `{"token":"s3cret","filler":"` + strings.Repeat("x", web.MaxResponseBody) + `"}`

	serve(t, get("/big"), []func(http.Handler) http.Handler{Capture(nil), Log(WithBody(false, true))},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(big))
		})

	a := only(t, lines(), "request completed")
	got := a["response_body"].(string)
	if strings.Contains(got, "s3cret") {
		t.Errorf("a credential in the response should be masked, got=%.80s", got)
	}
	if len(got) > web.MaxResponseBody+len(Redacted) {
		t.Errorf("the recorded response should be capped, got %d bytes", len(got))
	}
	if a["bytes_out"] != float64(len(big)) {
		t.Errorf("capping what is kept must not change what was counted, got=%v", a["bytes_out"])
	}
}

func TestLog_CompressedResponseBodyIsNotRecorded(t *testing.T) {
	// A compression layer sits inside the access log, so what would be kept is binary garbage that
	// JSON cannot even hold.
	lines := captureLog(t)
	serve(t, get("/gz"), []func(http.Handler) http.Handler{Capture(nil), Log(WithBody(false, true))},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte("\x1f\x8b\x08\x00garbage"))
		})

	if got := only(t, lines(), "request completed")["response_body"].(string); !strings.Contains(got, "gzip-encoded") {
		t.Errorf("a compressed body should be recorded as omitted, got=%q", got)
	}
}

func TestLog_BinaryResponseBodyIsLeftOut(t *testing.T) {
	lines := captureLog(t)
	serve(t, get("/png"), []func(http.Handler) http.Handler{Capture(nil), Log(WithBody(false, true))},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG\r\n"))
		})

	if _, ok := only(t, lines(), "request completed")["response_body"]; ok {
		t.Error("a binary response has nothing worth recording")
	}
}

func TestLog_QueryIsRedacted(t *testing.T) {
	lines := captureLog(t)
	serve(t, get("/cb?code=abc&token=s3cret&page=2"),
		[]func(http.Handler) http.Handler{Capture(nil), Log(WithQuery(true))},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	q := only(t, lines(), "request completed")["query"].(string)
	if strings.Contains(q, "s3cret") {
		t.Errorf("a token in the query string should be masked, got=%v", q)
	}
	if !strings.Contains(q, "page=2") {
		t.Errorf("the rest should be kept, got=%v", q)
	}
}

func TestLog_HeadersAreRedacted(t *testing.T) {
	lines := captureLog(t)
	req := get("/ping")
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("X-Request-Id", "r-1")

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log(WithHeaders(true, true))},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Set-Cookie", "session=s3cret")
			w.Header().Set("X-Page", "2")
			w.WriteHeader(200)
		})

	a := only(t, lines(), "request completed")
	in := a["request_headers"].(map[string]any)
	if in["Authorization"] != Redacted {
		t.Errorf("Authorization should be masked, got=%v", in)
	}
	if in["X-Request-Id"] != "r-1" {
		t.Errorf("an ordinary header should be kept, got=%v", in)
	}
	out := a["response_headers"].(map[string]any)
	if out["Set-Cookie"] != Redacted {
		t.Errorf("Set-Cookie should be masked, got=%v", out)
	}
	if out["X-Page"] != "2" {
		t.Errorf("an ordinary response header should be kept, got=%v", out)
	}
}

func TestAddSensitiveFields_AppliesToTheWholeProcess(t *testing.T) {
	// One word list per process: the other web integrations read the same one.
	AddSensitiveFields("tenant_secret")
	t.Cleanup(func() { /* the list is append-only by design; nothing to undo */ })

	lines := captureLog(t)
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"tenantSecret":"s3cret"}`))
	req.Header.Set("Content-Type", "application/json")
	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log(WithBody(true, false))},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	if body := only(t, lines(), "request completed")["request_body"].(string); strings.Contains(body, "s3cret") {
		t.Errorf("an added word should mask the field whatever its separators, got=%v", body)
	}
}

func TestLog_SkippedWhenInfoIsOff(t *testing.T) {
	// Everything the middleware does exists to assemble one Info line; with the level off, the
	// price would be paid in full and the result thrown away.
	var read bool
	req := httptest.NewRequest("POST", "/x", readerFunc(func([]byte) (int, error) {
		read = true
		return 0, nil
	}))
	quietAboveWarn(t)

	serve(t, req, []func(http.Handler) http.Handler{Capture(nil), Log(WithBody(true, false))},
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	if read {
		t.Error("the request body should not even be buffered when the line would be dropped")
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
