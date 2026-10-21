package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// redacted is the value after the redaction, xgin/middleware.Redacted
const redacted = "***REDACTED***"

// The access log is one JSON line per request, and its fields are the ones in
// internal/web/accesslog.go (AccessLog.Log): method, route, path, status, elapsed_ms and
// client_ip, plus the trace_id / span_id xtrace injects.
// A business log line (slog.InfoContext) is on the same trace as the access log: the same
// trace_id, which is the span's trace_id
func TestFunctional_AccessLogFieldsComplete_TraceIDSharedWithAppLogAndSpan(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	// What follows checks the framework defaults "the query string, the headers and the body are not recorded by default": service/application.yml writes none of them, so what is read is xgin.DefaultConfig
	p := harness.Start(t, harness.Options{Spans: true})

	start := time.Now()
	r := p.PostJSON(t, "/users", map[string]string{"name": "logan", "email": "logan@example.com"},
		"X-Forwarded-For", "203.0.113.9")
	rtt := time.Since(start)
	var u user
	r.JSON(t, &u)
	tid := traceIDOf(t, r)
	al := accessLog(t, p, tid)

	t.Run("the fields are all there", func(t *testing.T) {
		for k, want := range map[string]string{
			"level": "INFO", "method": "POST", "route": "/users", "path": "/users", "status": "201",
			// xgin/README.md, XGin.TrustedProxies: the private ranges are trusted by default, so the X-Forwarded-For from this host is taken and client_ip is the address inside it
			"client_ip": "203.0.113.9",
			"proto":     "HTTP/1.1",
		} {
			if got := al.Str(k); got != want {
				t.Errorf("the access log's %s should be %q, got %q\n%s", k, want, got, al.Line)
			}
		}
		// xgin/README.md: host and user_agent as they are; bytes_in is the request's Content-Length and bytes_out the response body's size
		if al.Str("host") == "" || al.Str("user_agent") == "" {
			t.Errorf("the access log should carry host and user_agent, got %s", al.Line)
		}
		if in, _ := num(al, "bytes_in"); in <= 0 {
			t.Errorf("POST /users carries a JSON request body, so bytes_in should be > 0, got %s", al.Line)
		}
		if out, _ := num(al, "bytes_out"); int(out) != len(r.Body) {
			t.Errorf("bytes_out should equal the response body's length %d, got %v", len(r.Body), al.Str("bytes_out"))
		}
		// xgin/README.md: elapsed_ms is in milliseconds, down to the microsecond
		el, ok := num(al, "elapsed_ms")
		elapsed := time.Duration(el * float64(time.Millisecond))
		if !ok || el <= 0 || elapsed > rtt {
			t.Errorf("elapsed_ms should be a number of milliseconds in (0, the client round trip %v], got %v", rtt, al.Str("elapsed_ms"))
		}
		if _, err := time.Parse(time.RFC3339Nano, al.Str("time")); err != nil {
			t.Errorf("time should be RFC3339, got %q", al.Str("time"))
		}
		if !hexSpanID.MatchString(al.Str("span_id")) {
			t.Errorf("span_id should be 16 hex digits, got %q", al.Str("span_id"))
		}
		// xgin/README.md: LogQuery / LogRequestHeaders / LogRequestBody / LogResponseHeaders / LogResponseBody are off by default
		for _, k := range []string{"query", "request_headers", "request_body", "response_headers", "response_body"} {
			if _, ok := al.Get(k); ok {
				t.Errorf("the documentation says %s is not recorded by default, and the access log holds it: %s", k, al.Line)
			}
		}
		t.Logf("numbers: POST /users took %v as a client round trip, with elapsed_ms %v in the access log", rtt, elapsed)
	})

	t.Run("the business log and the access log share one trace_id, which is the span's", func(t *testing.T) {
		biz := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "user created" && l.Str("trace_id") == tid })
		if biz.Str("user_id") != fmt.Sprint(u.ID) {
			t.Errorf("the business log line user created should have user_id %d, got %s", u.ID, biz.Line)
		}
		srv := serverSpan(t, p, tid)
		if srv.TraceID != tid || al.Str("trace_id") != tid {
			t.Errorf("X-Trace-Id, the access log and the span should share one trace_id: %s / %s / %s", tid, al.Str("trace_id"), srv.TraceID)
		}
		// Both lines are written inside the server span, so span_id is the server span's
		if al.Str("span_id") != srv.SpanID || biz.Str("span_id") != srv.SpanID {
			t.Errorf("the access log's and the business log's span_id should be the server span's %s, got %s / %s", srv.SpanID, al.Str("span_id"), biz.Str("span_id"))
		}
	})

	t.Run("route is the route template and path carries no query string", func(t *testing.T) {
		r := p.Get(t, fmt.Sprintf("/users/%d?cache=off&token=query-token-in-url", u.ID))
		l := accessLog(t, p, traceIDOf(t, r))
		if l.Str("route") != "/users/:id" || l.Str("path") != fmt.Sprintf("/users/%d", u.ID) {
			t.Errorf("route should be /users/:id and path /users/%d (with no query string), got route=%q path=%q", u.ID, l.Str("route"), l.Str("path"))
		}
		// xgin/README.md: "path carries no query string" -- the ?token= on the link never reaches the log this way
		mustNotContain(t, "the access log", l.Line, "query-token-in-url")
	})

	t.Run("with an upstream traceparent the log holds the upstream's trace_id", func(t *testing.T) {
		up, parent := randomTraceID(), randomSpanID()
		r := p.Get(t, "/ping", "traceparent", "00-"+up+"-"+parent+"-01")
		if got := traceIDOf(t, r); got != up {
			t.Fatalf("joining the upstream trace, X-Trace-Id should be the upstream's %s, got %s", up, got)
		}
		l := accessLog(t, p, up)
		srv := serverSpan(t, p, up)
		if srv.ParentSpanID != parent || !srv.ParentRemote {
			t.Errorf("the server span's parent should be the upstream's %s (remote), got parent=%q remote=%v", parent, srv.ParentSpanID, srv.ParentRemote)
		}
		if l.Str("span_id") != srv.SpanID {
			t.Errorf("the access log's span_id should be the server span's %s, got %s", srv.SpanID, l.Str("span_id"))
		}
	})

	t.Run("the metrics endpoint writes no access log", func(t *testing.T) {
		p.Metrics(t)
		p.Metrics(t)
		// One more request as a landmark: once its access log is out, the /metrics ones before it would be out too
		accessLog(t, p, traceIDOf(t, p.Get(t, "/ping")))
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/metrics" })); n != 0 {
			t.Errorf("xgin/README.md, LogSkipPaths: with Metric on the metrics endpoint writes no access log by itself, and %d lines were written", n)
		}
	})

	t.Run("an unmatched route still gets an access log line", func(t *testing.T) {
		l := accessLog(t, p, traceIDOf(t, p.Get(t, "/no/such/route")))
		// xgin/README.md: with no route matched, route is unmatched (as in the metrics and the spans) and the real path is in path
		if l.Str("status") != "404" || l.Str("route") != "unmatched" || l.Str("path") != "/no/such/route" {
			t.Errorf("an unmatched route should record 404, route=unmatched and the original path, got %s", l.Line)
		}
	})
}

// With everything the access log can record turned on, every promise of xgin/README.md, "The
// access log": a "contains" match on the sensitive words, at any nesting depth, Unicode folding,
// forms, a whole plain-text body masked, the header list and the word list, a header whose value
// is a URL losing its query string, and multipart / octet-stream never read
func TestFunctional_RequestBodyLogMasksAllSecrets(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: payloadLogs})
	var secrets []string // the plaintext each subtest uses, checked once more over the whole output at the end
	secret := func(label string) string {
		s := label + "-" + harness.NewID()
		secrets = append(secrets, s)
		return s
	}
	// bodyOf decodes the request body log back into a map (it is itself a JSON string)
	bodyOf := func(t *testing.T, l harness.Log, field string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(l.Str(field)), &m); err != nil {
			t.Fatalf("%s should be redacted JSON, got %q (%v)", field, l.Str(field), err)
		}
		return m
	}

	t.Run("password and token in login's request body and session_token in its response body", func(t *testing.T) {
		pw, tok := secret("pw"), secret("tok")
		r := p.PostJSON(t, "/login", map[string]string{"username": "carol", "password": pw, "token": tok})
		sess, _ := r.Map(t)["session_token"].(string)
		secrets = append(secrets, sess)
		l := accessLog(t, p, traceIDOf(t, r))
		req := bodyOf(t, l, "request_body")
		if req["password"] != redacted || req["token"] != redacted || req["username"] != "carol" {
			t.Errorf("the documentation says a value whose key holds a sensitive word is masked in the body and the rest stays, got request_body=%s", l.Str("request_body"))
		}
		resp := bodyOf(t, l, "response_body")
		if resp["session_token"] != redacted || resp["username"] != "carol" {
			t.Errorf("the documentation says the response body is redacted as well (session_token holds session and token), got response_body=%s", l.Str("response_body"))
		}
		mustNotContain(t, "the access log", l.Line, pw, tok, sess)
	})

	t.Run("the request headers: the ones on the list and the ones whose name holds a sensitive word are masked, the rest stay", func(t *testing.T) {
		h := map[string]string{
			"Authorization":       "Bearer " + secret("auth"),
			"Cookie":              "sid=" + secret("cookie"),
			"Proxy-Authorization": "Basic " + secret("proxyauth"),
			"X-Api-Key":           secret("apikey"),
			"X-Auth-Token":        secret("authtoken"),
			"X-Csrf-Token":        secret("csrf"),    // not on the list, and its name holds token
			"X-Session-Id":        secret("session"), // not on the list, and its name holds session
			// A name holding no sensitive word that is on the list alone (added in service/main.go with AddSensitiveHeaders):
			// the names of the listed headers above all carry a sensitive word, so they would be masked by the word list even if the list rule did not work; this one is the only one that shows it
			"X-Tenant-Id": secret("tenant"),
			"X-Visible":   "keep-me",
		}
		var kv []string
		for k, v := range h {
			kv = append(kv, k, v)
		}
		r := p.PostJSON(t, "/login", map[string]string{"username": "hdr"}, kv...)
		l := accessLog(t, p, traceIDOf(t, r))
		for k := range h {
			want := redacted
			if k == "X-Visible" {
				want = "keep-me"
			}
			if got := l.Str("request_headers." + k); got != want {
				t.Errorf("the request header %s should be recorded as %q, got %q", k, want, got)
			}
		}
		mustNotContain(t, "the access log", l.Line, secrets...)
	})

	t.Run("a request header whose value is a URL loses its query string and fragment", func(t *testing.T) {
		code, tok, x := secret("oauthcode"), secret("urltok"), secret("fwd")
		r := p.PostJSON(t, "/login", map[string]string{"username": "ref"},
			"Referer", "https://example.com/cb?code="+code+"#frag",
			"X-Original-URL", "/internal/path?token="+tok,
			"X-Forwarded-URI", "/a/b#"+x)
		l := accessLog(t, p, traceIDOf(t, r))
		// The header names in the log are Go's canonical spelling: X-Original-URL is recorded as X-Original-Url
		for k, want := range map[string]string{
			"Referer": "https://example.com/cb", "X-Original-URL": "/internal/path", "X-Forwarded-URI": "/a/b",
		} {
			if got := l.Str("request_headers." + textproto.CanonicalMIMEHeaderKey(k)); got != want {
				t.Errorf("the documentation says %s keeps what is before ? and #, so it should record %q, got %q", k, want, got)
			}
		}
		mustNotContain(t, "the access log", l.Line, code, tok, x)
	})

	t.Run("a multipart upload with an upper-case Content-Type is not read either", func(t *testing.T) {
		content := []byte("file content with password=" + secret("upload") + "\n")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="file"; filename="notes.txt"`},
			"Content-Type":        {"text/plain"},
		})
		part.Write(content)
		mw.Close()
		// xgin/README.md: "the Content-Type is matched case-insensitively, and Multipart/Form-Data is not read either"
		ct := "Multipart/Form-Data; boundary=" + mw.Boundary()

		r := p.Do(t, http.MethodPost, "/upload", buf.Bytes(), "Content-Type", ct)
		sum := sha256.Sum256(content)
		m := r.Map(t)
		if r.Status != http.StatusOK || m["size"] != float64(len(content)) || m["sha256"] != hex.EncodeToString(sum[:]) {
			t.Errorf("the handler should receive the whole file (logging does not touch the request body), got %v", r)
		}
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); got != "[multipart/form-data omitted]" {
			t.Errorf("the documentation says a multipart request body is not read and records an omitted note alone, got request_body=%q", got)
		}
		mustNotContain(t, "the access log", l.Line, secrets[len(secrets)-1])
	})

	t.Run("an octet-stream with an upper-case Content-Type is not read", func(t *testing.T) {
		b := secret("binary")
		r := p.Do(t, http.MethodPost, "/login", b, "Content-Type", "Application/Octet-Stream")
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); got != "[binary content omitted]" {
			t.Errorf("the documentation says an application/octet-stream request body is not read, got request_body=%q", got)
		}
		mustNotContain(t, "the access log", l.Line, b)
	})

	t.Run("the Unicode-folded field names \u017Fecret and toKen are masked as well", func(t *testing.T) {
		s1, s2 := secret("longs"), secret("kelvin")
		// \u017F is U+017F (the long s) and K is U+212A (the Kelvin sign): encoding/json binds them to Secret / Token
		r := p.PostJSON(t, "/login", map[string]string{"username": "fold", "ſecret": s1, "toKen": s2})
		l := accessLog(t, p, traceIDOf(t, r))
		req := bodyOf(t, l, "request_body")
		if req["ſecret"] != redacted || req["toKen"] != redacted {
			t.Errorf("the documentation says a key equal to a sensitive word after Unicode folding is masked as well, got request_body=%s", l.Str("request_body"))
		}
		mustNotContain(t, "the access log", l.Line, s1, s2)
	})

	t.Run("sensitive fields inside nested objects and arrays", func(t *testing.T) {
		s1, s2, s3 := secret("newpw"), secret("oldpw"), secret("userpw")
		body := fmt.Sprintf(`{"username":"nest","profile":{"new_password":%q},"history":[{"old-password":%q}],"user.password":%q,"note":"keep"}`, s1, s2, s3)
		r := p.Do(t, http.MethodPost, "/login", body, "Content-Type", "application/json")
		l := accessLog(t, p, traceIDOf(t, r))
		req := bodyOf(t, l, "request_body")
		prof, _ := req["profile"].(map[string]any)
		hist, _ := req["history"].([]any)
		var old any
		if len(hist) == 1 {
			old = hist[0].(map[string]any)["old-password"]
		}
		if prof["new_password"] != redacted || old != redacted || req["user.password"] != redacted || req["note"] != "keep" {
			t.Errorf("the documentation says new_password, user.password and the old-password inside the array are all recognized, got request_body=%s", l.Str("request_body"))
		}
		mustNotContain(t, "the access log", l.Line, s1, s2, s3)
	})

	t.Run("a field name written with a backslash escape", func(t *testing.T) {
		s := secret("escaped")
		// The key is password with the a written as a JSON escape (backslash, u, 0061): the raw bytes hold no password substring,
		// which the literal pre-check cannot recognize, and only "hand anything holding a backslash to the parser" stops it.
		// The escape is written with \\ in an interpreted string and not inside backticks: the escape here used to be turned back into an a on the way to disk,
		// so the body held a plaintext password and what was really tested was the literal pre-check, and breaking that rule would have gone unnoticed
		body := "{\"username\":\"esc\",\"p\\u0061ssword\":\"" + s + "\"}"
		if strings.Contains(body, "password") || !strings.Contains(body, "\\u0061") {
			t.Fatalf("the test itself is wrong: the body should hold the escape alone and no password substring, got %s", body)
		}
		r := p.Do(t, http.MethodPost, "/login", body, "Content-Type", "application/json")
		l := accessLog(t, p, traceIDOf(t, r))
		if req := bodyOf(t, l, "request_body"); req["password"] != redacted {
			t.Errorf("the key after the escape is password and should be masked, got request_body=%s", l.Str("request_body"))
		}
		mustNotContain(t, "the access log", l.Line, s)
	})

	t.Run("a percent-encoded field name in a form", func(t *testing.T) {
		s := secret("formpw")
		r := p.Do(t, http.MethodPost, "/login", "username=form&p%61ssword="+s, "Content-Type", "application/x-www-form-urlencoded")
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); !strings.Contains(got, "password="+redacted) || !strings.Contains(got, "username=form") {
			t.Errorf("the documentation says a form is redacted by key (p%%61ssword being password), got request_body=%q", got)
		}
		mustNotContain(t, "the access log", l.Line, s)
	})

	t.Run("a plain-text body holding a sensitive word is masked whole", func(t *testing.T) {
		s := secret("plain")
		r := p.Do(t, http.MethodPost, "/login", "my api token is "+s, "Content-Type", "text/plain")
		l := accessLog(t, p, traceIDOf(t, r))
		if got := l.Str("request_body"); got != redacted {
			t.Errorf("the documentation says plain text cannot be located by field and is masked whole once a sensitive word turns up, got request_body=%q", got)
		}
		mustNotContain(t, "the access log", l.Line, s)
	})

	t.Run("a value that is not sensitive is written back as it is: a large integer keeps its precision and < > & are not escaped", func(t *testing.T) {
		s := secret("bigint")
		body := `{"username":"a<b>&c","n":12345678901234567890,"password":"` + s + `"}`
		r := p.Do(t, http.MethodPost, "/login", body, "Content-Type", "application/json")
		l := accessLog(t, p, traceIDOf(t, r))
		got := l.Str("request_body")
		if !strings.Contains(got, `"n":12345678901234567890`) || !strings.Contains(got, `"username":"a<b>&c"`) {
			t.Errorf("the documentation says the other values are written back as they are (a large integer keeping its precision, < > & not escaped), got request_body=%s", got)
		}
		mustNotContain(t, "the access log", l.Line, s)
	})

	t.Run("the query string is redacted field by field", func(t *testing.T) {
		tok := secret("qtok")
		l := accessLog(t, p, traceIDOf(t, p.Get(t, "/users/1?page=2&access_token="+tok)))
		q, err := url.ParseQuery(l.Str("query"))
		if err != nil || q.Get("page") != "2" || q.Get("access_token") != redacted {
			t.Errorf("query should keep page and mask access_token, got %q", l.Str("query"))
		}
		mustNotContain(t, "the access log", l.Line, tok)
	})

	t.Run("not one piece of plaintext in the whole process output", func(t *testing.T) {
		mustNotContain(t, "the process's stdout / stderr", p.Output(), secrets...)
		t.Logf("numbers: checked %d pieces of plaintext", len(secrets))
	})
}

// PG's password sits in the DSN (e2e-secret-pw by default in the harness). Running normally,
// failing, failing to start and a malformed DSN: none of the logs, the response bodies, the spans
// or /metrics may hold it.
//
// xgorm/README.md, XGorm: a failed DSN pre-check "does not hand back" pgx's raw error (which holds
// the whole DSN); with Log: true what is recorded is the SQL with its placeholders
func TestFunctional_PGPasswordNeverInLogsOrErrors(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	dsn := harness.PGDSN(harness.PGAddr())

	t.Run("one normal round with the SQL log, debug, the request body log and the spans on, breaking PG in the middle", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		pg := harness.NewProxy(t, harness.PGAddr())
		p := harness.Start(t, harness.Options{
			PGAddr: pg.Addr(), Spans: true, Downstream: stub.URL,
			Overlay: sqlLog("default") + debugLogs + payloadLogs,
		})
		var bodies []string
		keep := func(r harness.Response) { bodies = append(bodies, string(r.Body)) }

		u := createUser(t, p, "pw-check", "pw@example.com")
		for _, path := range []string{
			fmt.Sprintf("/users/%d", u.ID), fmt.Sprintf("/users/%d", u.ID), fmt.Sprintf("/users/%d?cache=off", u.ID),
			"/users/999999999", "/users/abc", "/boom", "/proxy?token=t", "/stuck?ms=1&db=1",
		} {
			keep(p.Get(t, path))
		}
		keep(p.PostJSON(t, "/login", map[string]string{"username": "u", "password": "p"}))
		keep(p.PostJSON(t, "/orders", map[string]any{"user_id": u.ID, "amount": 1}))
		keep(p.PostJSON(t, "/orders", map[string]any{"user_id": u.ID, "amount": 1, "fail_at": 3, "rollback_fail_at": 1}))

		// Break PG: the connections already open are cut and new ones are refused. An error here is the likeliest to carry the connection string out
		pg.Cut()
		start := time.Now()
		r := p.Get(t, fmt.Sprintf("/users/%d?cache=off", u.ID))
		cutElapsed := time.Since(start)
		keep(r)
		if r.Status != http.StatusInternalServerError {
			t.Errorf("a ?cache=off read should answer 500 while PG is cut off, got %v", r)
		}
		// The write has to take the failing branch as well: without the cut it answers 201, and this round would miss one kind of error that can carry the connection string out
		r = p.PostJSON(t, "/users", map[string]string{"name": "while-cut"})
		keep(r)
		if r.Status != http.StatusInternalServerError {
			t.Errorf("POST /users should answer 500 while PG is cut off, got %v", r)
		}
		pg.Restore()
		// A broken connection in the pool is dropped when it fails, so a read succeeds soon after the recovery; how fast it recovers is not asserted here,
		// this only brings the process back to health before the graceful exit. 50 attempts at most, 50ms apart
		start = time.Now()
		attempts := 0
		for recovered := false; !recovered; {
			if attempts++; attempts > 50 {
				t.Fatalf("PG recovered and a read still fails %v later: %v", time.Since(start), r)
			}
			r = p.Get(t, fmt.Sprintf("/users/%d?cache=off", u.ID))
			keep(r)
			if recovered = r.Status == http.StatusOK; !recovered {
				time.Sleep(50 * time.Millisecond)
			}
		}
		t.Logf("numbers: one read took %v to answer 500 while PG was cut off; after the recovery read %d answered 200, %v in", cutElapsed, attempts, time.Since(start))

		metrics, err := http.Get(p.URL("/metrics"))
		if err == nil {
			var b bytes.Buffer
			b.ReadFrom(metrics.Body)
			metrics.Body.Close()
			mustNotContain(t, "/metrics", b.String(), pw)
		}
		exit := p.Terminate(t, 20*time.Second)
		if exit.Code != 0 {
			t.Errorf("it should exit with 0 after the SIGTERM, got %v", exit)
		}

		out := p.Output()
		spans, _ := os.ReadFile(p.SpanFile)
		mustNotContain(t, "the process's stdout / stderr", out, pw, dsn)
		mustNotContain(t, "the span file", string(spans), pw)
		mustNotContain(t, "the response bodies", strings.Join(bodies, "\n"), pw)
		// And the other way around: make sure this round really took the paths that fail and record SQL, rather than running empty
		for _, msg := range []string{"SQL", "read user failed", "panic while handling request", "xflow rollback did not complete, resources may be left dangling"} {
			if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == msg })) == 0 {
				t.Errorf("this round should have produced a %q log line and there is not one: the check does not cover enough", msg)
			}
		}
		t.Logf("numbers: checked %d bytes of output, %d bytes of spans and %d response bodies", len(out), len(spans), len(bodies))
	})

	// On a failed start the error is written to stderr by MustRun, while the logs after xlog is installed go to stdout
	startFails := func(t *testing.T, o harness.Options, secrets ...string) (harness.Exit, *harness.Process) {
		t.Helper()
		o.NoWait = true
		p := harness.Start(t, o)
		exit, ok := p.Wait(30 * time.Second)
		if !ok {
			t.Fatalf("the start should fail and exit, and it is still running after 30s\n%s", p.Output())
		}
		if exit.Code == 0 {
			t.Fatalf("the start should fail (a non-zero exit), got %v\n%s", exit, p.Output())
		}
		if !strings.Contains(p.Stderr(), "xgorm") {
			t.Errorf("stderr should hold the error xgorm reports, got:\n%s", p.Stderr())
		}
		mustNotContain(t, "the process's stdout / stderr", p.Output(), secrets...)
		return exit, p
	}

	t.Run("a failed start: PG refuses connections", func(t *testing.T) {
		t.Parallel()
		addr := fmt.Sprintf("127.0.0.1:%d", harness.FreePort(t)) // nothing is listening
		exit, p := startFails(t, harness.Options{PGAddr: addr}, pw)
		if !strings.Contains(p.Stderr(), "connection refused") {
			t.Errorf("the error should say it cannot connect (connection refused), got:\n%s", p.Stderr())
		}
		// docs/behavior.md, "The connect probe at start": it retries 3 times when it cannot connect
		t.Logf("numbers: with PG refusing connections the start failed and exited after %v (%v)", exit.Uptime, exit)
	})

	t.Run("a failed start: a wrong password", func(t *testing.T) {
		t.Parallel()
		wrong := "wrong-pw-" + harness.NewID()
		bad := strings.Replace(dsn, ":"+pw+"@", ":"+wrong+"@", 1)
		exit, p := startFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": bad}}, pw, wrong)
		if !strings.Contains(p.Stderr(), "password authentication failed") || !strings.Contains(p.Stderr(), "authentication to ") {
			t.Errorf("the error should say it is an authentication failure, got:\n%s", p.Stderr())
		}
		if strings.Contains(p.Stderr(), "cannot reach") {
			t.Errorf("an authentication failure is not a failure to connect and should not report cannot reach:\n%s", p.Stderr())
		}
		t.Logf("numbers: with a wrong password the start failed and exited after %v", exit.Uptime)
	})

	for _, c := range []struct{ name, dsn string }{
		{"a malformed sslmode in the URL", strings.Replace(dsn, "sslmode=disable", "sslmode=bogus", 1)},
		{"a malformed pgx-only parameter in the URL", dsn + "&default_query_exec_mode=bogus"},
		{"a port in the URL that is not a number", strings.Replace(dsn, harness.PGAddr(), "127.0.0.1:notaport", 1)},
		// xgorm/README.md: pgx's raw error "masks the tidy password=x spelling alone, and carries password = hunter2 out as it is"
		{"the key=value spelling with spaces around the equals sign", "host=127.0.0.1 user=xone password = " + pw + " dbname=xone_e2e sslmode=bogus"},
		// parsePostgres in xgorm/dsn.go: the pre-check has to use pgx.ParseConfig -- with pgconn alone a malformed pgx-only parameter goes through,
		// and the error turns up only in gorm.Open, reported by pgx with the whole DSN in its text. In the URL spelling pgx masks the password as xxxxx itself,
		// so the URL cases above cannot tell whether the password leaked; this is the combination xgorm/README.md names, where a leak is plaintext
		{"the key=value spelling with spaces around the equals sign and a malformed pgx-only parameter", "host=127.0.0.1 user=xone password = " + pw + " dbname=xone_e2e sslmode=disable default_query_exec_mode=bogus"},
	} {
		t.Run("a failed start: a malformed DSN ("+c.name+")", func(t *testing.T) {
			t.Parallel()
			exit, p := startFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": c.dsn}}, pw)
			if !strings.Contains(p.Stderr(), "failed to parse DSN") {
				t.Errorf("the error should say the DSN is malformed, got:\n%s", p.Stderr())
			}
			t.Logf("numbers: with %s the start failed and exited after %v", c.name, exit.Uptime)
		})
	}
}

// xgorm/README.md, XGorm.Log: "what is recorded is the SQL with its placeholders and no parameter values"
func TestFunctional_SQLLogHasPlaceholdersNotArgs(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Spans: true, Overlay: sqlLog("default")})
	name, email := "sql-name-"+harness.NewID(), "sql-email-"+harness.NewID()+"@example.com"
	r := p.PostJSON(t, "/users", map[string]string{"name": name, "email": email})
	tid := traceIDOf(t, r)
	l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "SQL" && l.Str("trace_id") == tid })
	logged := l.Str("sql")

	if !strings.HasPrefix(logged, "INSERT INTO") {
		t.Fatalf("POST /users's SQL log should be that INSERT, got %s", l.Line)
	}
	mustNotContain(t, "the SQL log", l.Line, name, email)
	if _, ok := num(l, "elapsed_ms"); !ok || l.Str("rows_affected") != "1" {
		t.Errorf("the SQL log should carry elapsed_ms and rows_affected=1, got %s", l.Line)
	}

	t.Run("the SQL in the log is the statement sent to PG", func(t *testing.T) {
		serverSpan(t, p, tid)
		ins := spansNamed(traceSpans(t, p, tid), "gorm.create")
		if len(ins) != 1 {
			t.Fatalf("this trace should hold one gorm.create span")
		}
		sent := ins[0].Str("db.query.text")
		// ExplainSQL in gorm v1.31.2 logger/sql.go: PG's Dialector first rewrites $1 into $1$ to await the parameter substitution,
		// while ParamsFilter returns an empty parameter list; statement in xgorm/logger.go puts the marker back
		if logged != sent {
			t.Errorf("the SQL log and the span's db.query.text should be the same statement: the log %q, the span %q", logged, sent)
		}
		if !strings.Contains(logged, "$1") || strings.Contains(logged, "$1$") {
			t.Errorf("PG's SQL log should carry $1-style placeholders (as sent to PG), got %q", logged)
		}
	})
}

// xredis/README.md, XRedis.Log: "it records the command name and the first key alone, and no value
// and no further argument" and "a key that does not exist (redis.Nil) is no failure"
//
// GET /users/:id reading through: Redis does not hold it -> the GET gets a nil -> PG is read -> a
// SET writes the whole user (with name and email) back into Redis. The value is that JSON, and the
// whole process output may not hold it
func TestFunctional_RedisLogHasKeysNotValues(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: "XRedis:\n  Log: true\n"})
	name, email := "redis-name-"+harness.NewID()+harness.NewID(), "redis-email-"+harness.NewID()+harness.NewID()+"@example.com"
	var u user
	p.PostJSON(t, "/users", map[string]string{"name": name, "email": email}).JSON(t, &u)
	r := p.Get(t, fmt.Sprintf("/users/%d", u.ID))
	if r.Status != http.StatusOK {
		t.Fatalf("GET /users/:id should answer 200, got %v", r)
	}
	tid := traceIDOf(t, r)
	key := p.KeyPrefix + "user:" + fmt.Sprint(u.ID)

	cmd := func(c string) harness.Log {
		return p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "redis command" && l.Str("cmd") == c && l.Str("trace_id") == tid
		})
	}
	get, set := cmd("get"), cmd("set")
	for _, l := range []harness.Log{get, set} {
		if l.Str("key") != key || l.Str("name") != "default" || l.Level() != "INFO" {
			t.Errorf("a command log line should carry the instance name default and the key %s at INFO, got %s", key, l.Line)
		}
		if _, ok := num(l, "elapsed_ms"); !ok {
			t.Errorf("a command log line should carry elapsed_ms, got %s", l.Line)
		}
	}
	if v, _ := get.Get("nil"); v != true {
		t.Errorf("a GET that missed the cache should carry nil=true (not a failure), got %s", get.Line)
	}
	if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "redis command failed" })); n != 0 {
		t.Errorf("a redis.Nil should not be recorded as redis command failed, got %d lines", n)
	}
	mustNotContain(t, "the process's stdout / stderr", p.Output(), name, email)
}

// xhttp/README.md, XHttp.Log: "one log line per logical request" and "the query string ... is
// never recorded, and the query string of a URL in an error text is dropped as well"
//
// /proxy?token=... puts the token into the outbound URL's query string. Three cases are looked at:
// success, a downstream 5xx (one attempt even with the retries configured) and a transport error
// (the retries used up): the xhttp line holds the path and the number of attempts, and no token
func TestFunctional_HTTPClientLogHasNoQuery(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	const overlay = "XHttp:\n  Timeout: 2s\n  Log: true\n  RetryCount: 2\n  RetryWaitTime: 10ms\n  RetryMaxWaitTime: 10ms\n"
	for _, c := range []struct {
		name     string
		status   int // the status code the downstream answers; 0 means the downstream cannot be reached
		msg      string
		attempts float64
	}{
		{"the downstream answers 200", http.StatusOK, "http request", 1},
		{"the downstream answers 500 and a response means no retry", http.StatusInternalServerError, "http request failed", 1},
		{"the downstream cannot be reached and the retries are used up", 0, "http request failed", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			downstream := fmt.Sprintf("http://127.0.0.1:%d", harness.FreePort(t)) // nothing is listening
			if c.status != 0 {
				stub := harness.NewStub(t)
				stub.SetStatus(c.status)
				downstream = stub.URL
			}
			p := harness.Start(t, harness.Options{Downstream: downstream, Overlay: overlay})
			token := "tok-" + harness.NewID() + harness.NewID()
			r := p.Get(t, "/proxy?token="+token)
			tid := traceIDOf(t, r)

			l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
				return strings.HasPrefix(l.Msg(), "http request") && l.Str("trace_id") == tid
			})
			if l.Msg() != c.msg || l.Str("method") != http.MethodGet || l.Str("path") != "/echo" {
				t.Errorf("it should be one %s line, GET /echo, got %s", c.msg, l.Line)
			}
			if got, _ := num(l, "attempts"); got != c.attempts {
				t.Errorf("attempts should be %v, got %s", c.attempts, l.Line)
			}
			if got, _ := num(l, "status"); int(got) != c.status {
				t.Errorf("status should be %d, got %s", c.status, l.Line)
			}
			if c.status == 0 && !strings.Contains(l.Str("error"), "connection refused") {
				t.Errorf("when it cannot connect, error should say why, got %s", l.Line)
			}
			if n := len(p.FindLogs(func(l harness.Log) bool { return strings.HasPrefix(l.Msg(), "http request") })); n != 1 {
				t.Errorf("one logical request makes one line, got %d", n)
			}
			if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xhttp resty log" })); n != 0 {
				t.Errorf("with Log on, resty's retry log is no longer written, got %d lines", n)
			}
			// No line the framework writes may hold it. The business's own downstream call failed records err as it is (with the whole URL),
			// which is how service/routes.go writes it and is not xhttp's affair, so it is skipped here
			for _, fl := range p.Logs() {
				if fl.Msg() != "downstream call failed" {
					mustNotContain(t, "the log line "+fl.Msg(), fl.Line, token)
				}
			}
			mustNotContain(t, "the process's stderr", p.Stderr(), token)
			t.Logf("numbers: %s: %s", c.name, l.Line)
		})
	}
}
