package xhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

// secret is a long random string: the "must not appear in the log" checks use it, since a short number would collide with a timestamp or a duration.
func secret(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// captureLogs swaps the slog default logger for one writing JSON into a buffer and returns functions for "the whole text" and "each parsed line".
func captureLogs(t *testing.T) (raw func() string, parsed func() []map[string]any) {
	t.Helper()
	var buf strings.Builder
	w := &lockedWriter{w: &buf}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	raw = func() string {
		w.mu.Lock()
		defer w.mu.Unlock()
		return buf.String()
	}
	parsed = func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(raw()), "\n") {
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
	return raw, parsed
}

// requestLines picks out the logs with these messages.
func requestLines(all []map[string]any, msgs ...string) []map[string]any {
	var out []map[string]any
	for _, l := range all {
		for _, m := range msgs {
			if l["msg"] == m {
				out = append(out, l)
			}
		}
	}
	return out
}

var allRequestMsgs = []string{"http request", "http request failed", "slow http request"}

// logClient is a client with Log on and tracing and metrics off; newQuiet's discardLogger is not installed, so resty's logs still reach slog.
func logClient(t *testing.T, mutate ...func(*Config)) *resty.Client {
	t.Helper()
	c := DefaultConfig()
	c.Log, c.Trace, c.Metric = true, false, false
	c.RetryWaitTime, c.RetryMaxWaitTime = time.Millisecond, time.Millisecond
	for _, m := range mutate {
		m(&c)
	}
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	return client
}

// cutServer cuts the connection on every request: a transport-layer error, which resty retries.
func cutServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLog_SuccessOneInfoLineWithoutQuery(t *testing.T) {
	token := secret(t, "token")
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	raw, lines := captureLogs(t)
	client := logClient(t)
	if _, err := client.R().SetHeader("Authorization", "Bearer "+token).SetBody(token).
		Post(srv.URL + "/api/items?token=" + token + "#frag"); err != nil {
		t.Fatal(err)
	}

	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("one line per request, got=%v", lines())
	}
	l := got[0]
	host := strings.TrimPrefix(srv.URL, "http://")
	if l["msg"] != "http request" || l["level"] != "INFO" || l["method"] != "POST" || l["host"] != host ||
		l["path"] != "/api/items" || l["status"] != float64(200) || l["attempts"] != float64(1) {
		t.Errorf("the fields are wrong, got=%v", l)
	}
	if _, ok := l["elapsed_ms"].(float64); !ok {
		t.Errorf("elapsed_ms must be carried, got=%v", l)
	}
	if _, ok := l["error"]; ok {
		t.Errorf("a successful request carries no error, got=%v", l)
	}
	if strings.Contains(raw(), token) {
		t.Errorf("a token from the query string, a header or the body reached the log: %s", raw())
	}
}

func TestLog_5xxWithRetriesConfiguredIsOneWarnLine(t *testing.T) {
	// With a response in hand there is no retry, and a 5xx is not retried either: one attempt, one WARN line.
	token := secret(t, "token")
	var hits atomic.Int32
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) })
	raw, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.RetryCount = 2 })
	resp, err := client.R().Get(srv.URL + "/x?token=" + token)
	if err != nil || resp.StatusCode() != 500 {
		t.Fatalf("precondition: a 500 must come back, got=%v %v", resp, err)
	}
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("one line per request, got=%v", lines())
	}
	if l := got[0]; l["msg"] != "http request failed" || l["level"] != "WARN" || l["status"] != float64(500) || l["attempts"] != float64(hits.Load()) {
		t.Errorf("a 5xx must be a WARN http request failed carrying the status and the attempt count (the downstream saw %d), got=%v", hits.Load(), l)
	}
	if strings.Contains(raw(), token) {
		t.Errorf("a token reached the log: %s", raw())
	}
}

func TestLog_4xxIsInfo(t *testing.T) {
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get(srv.URL + "/missing")
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["msg"] != "http request" || got[0]["status"] != float64(404) {
		t.Errorf("a 4xx is the downstream's business answer and is recorded at INFO, got=%v", lines())
	}
}

func TestLog_TransportErrorAfterRetriesOneLineNoQuery(t *testing.T) {
	// resty v2.17.2 calls OnError once after every retry has finished, with Attempt being the number of attempts used.
	token := secret(t, "token")
	var hits atomic.Int32
	srv := cutServer(t, &hits)
	raw, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.RetryCount = 2 })
	u := strings.Replace(srv.URL, "http://", "http://someone:"+token+"@", 1) + "/cut?token=" + token
	if _, err := client.R().Get(u); err == nil {
		t.Fatal("precondition: the connection was cut, so it must fail")
	}

	all := lines()
	got := requestLines(all, allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("2 retries must still record one line, got=%v", all)
	}
	l := got[0]
	if l["msg"] != "http request failed" || l["level"] != "WARN" || l["status"] != float64(0) || l["attempts"] != float64(3) || l["path"] != "/cut" {
		t.Errorf("a transport error: WARN, status 0, attempts 3, got=%v", l)
	}
	e, _ := l["error"].(string)
	if !strings.Contains(e, "/cut") || !strings.Contains(e, "EOF") {
		t.Errorf("error must keep the URL without its query string and the reason, got=%q", e)
	}
	if strings.Contains(raw(), token) || strings.Contains(raw(), "someone") {
		t.Errorf("a query string or userinfo reached the log through the error text: %s", raw())
	}
	// The same thing is not said twice: resty's own WARN (Attempt N) and ERROR on the retry path are no longer printed.
	if n := len(requestLines(all, "xhttp resty log")); n != 0 {
		t.Errorf("with Log on, resty's retry logs must not be printed, got %d lines: %v", n, all)
	}
}

func TestLog_TransportErrorRefusedNoQuery(t *testing.T) {
	token := secret(t, "token")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	raw, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get("http://" + addr + "/x?token=" + token)
	got := requestLines(lines(), "http request failed")
	if len(got) != 1 || !strings.Contains(fmt.Sprint(got[0]["error"]), "connection refused") {
		t.Fatalf("a failed connection must record one http request failed line with the reason, got=%v", lines())
	}
	if strings.Contains(raw(), token) {
		t.Errorf("a token reached the log: %s", raw())
	}
}

func TestLog_OffMeansNoRequestLinesAndRestyLogsUnchanged(t *testing.T) {
	var hits atomic.Int32
	srv := cutServer(t, &hits)
	_, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.Log, c.RetryCount = false, 1 })
	client.R().Get(srv.URL)
	all := lines()
	if got := requestLines(all, allRequestMsgs...); len(got) != 0 {
		t.Errorf("Log is off by default, so there must be no request log, got=%v", got)
	}
	// With it off, resty's own retry logs are unchanged (one WARN per attempt, one ERROR once used up).
	if n := len(requestLines(all, "xhttp resty log")); n != 3 {
		t.Errorf("with Log off resty's retry logs are still 3 lines, got=%v", all)
	}
}

func TestLog_OtherRestyWarningsStillLogged(t *testing.T) {
	// Only the two retry-path kinds are dropped; resty's other warnings are unchanged, such as Basic Auth over plaintext HTTP.
	srv, _ := echo(t, nil)
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().SetBasicAuth("u", "p").Get(srv.URL)
	for _, l := range requestLines(lines(), "xhttp resty log") {
		if strings.Contains(fmt.Sprint(l["detail"]), "Basic Auth") {
			return
		}
	}
	t.Errorf("resty's other warnings must not be swallowed, got=%v", lines())
}

func TestLog_SlowThreshold(t *testing.T) {
	for _, c := range []struct {
		name      string
		threshold time.Duration
		wantMsg   string
	}{
		{"over threshold", 20 * time.Millisecond, "slow http request"},
		{"zero disables", 0, "http request"},
		{"under threshold", time.Second, "http request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(60 * time.Millisecond) })
			_, lines := captureLogs(t)
			client := logClient(t, func(cc *Config) { cc.SlowThreshold = c.threshold })
			client.R().Get(srv.URL)
			got := requestLines(lines(), allRequestMsgs...)
			if len(got) != 1 || got[0]["msg"] != c.wantMsg {
				t.Fatalf("it must be one %s line, got=%v", c.wantMsg, lines())
			}
			if c.wantMsg == "slow http request" && (got[0]["level"] != "WARN" || got[0]["threshold_ms"] != float64(20)) {
				t.Errorf("a slow request must be a WARN carrying threshold_ms, got=%v", got[0])
			}
		})
	}
}

func TestLog_ElapsedCoversRetriesWithMetricOff(t *testing.T) {
	// The start has to be recorded even with Metric off, or the duration is only the last attempt's.
	var hits atomic.Int32
	srv := cutServer(t, &hits)
	_, lines := captureLogs(t)
	const backoff = 60 * time.Millisecond
	client := logClient(t, func(c *Config) {
		c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, backoff, backoff
	})
	client.R().Get(srv.URL)
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 {
		t.Fatalf("got=%v", lines())
	}
	// Two backoffs, each picked by resty in [wait, maxWait]; the two bounds are equal here.
	if e, _ := got[0]["elapsed_ms"].(float64); e < float64(2*backoff/time.Millisecond)*0.9 {
		t.Errorf("the duration must cover the whole logical request (the two %v backoffs included), got %vms", backoff, e)
	}
}

func TestLog_UsesCallerCtx(t *testing.T) {
	// The ctx is handed to slog as it is: xlog takes trace_id / span_id from it.
	type key struct{}
	srv, _ := echo(t, nil)
	var mu sync.Mutex
	var seen []any
	old := slog.Default()
	slog.SetDefault(slog.New(ctxHandler{fn: func(ctx context.Context, r slog.Record) {
		if r.Message == "http request" {
			mu.Lock()
			seen = append(seen, ctx.Value(key{}))
			mu.Unlock()
		}
	}}))
	t.Cleanup(func() { slog.SetDefault(old) })
	client := logClient(t)
	client.R().SetContext(context.WithValue(context.Background(), key{}, "caller")).Get(srv.URL)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "caller" {
		t.Errorf("the log must use the caller's ctx, got=%v", seen)
	}
}

type ctxHandler struct {
	fn func(context.Context, slog.Record)
}

func (h ctxHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h ctxHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h ctxHandler) WithGroup(string) slog.Handler            { return h }
func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	h.fn(ctx, r)
	return nil
}

func TestScrubText(t *testing.T) {
	for in, want := range map[string]string{
		`Get "http://user:***@h:1/x?token=a": EOF`:    `Get "http://h:1/x": EOF`,
		`Get "https://u@h/x": EOF`:                    `Get "https://h/x": EOF`,
		`Get "http://h/x": context deadline exceeded`: `Get "http://h/x": context deadline exceeded`,
	} {
		if got := scrubText(in); got != want {
			t.Errorf("scrubText(%q)\n got=%q\nwant=%q", in, got, want)
		}
	}
}

func TestConfig_LogDefaults(t *testing.T) {
	c := DefaultConfig()
	if c.Log || c.SlowThreshold != time.Second {
		t.Errorf("the defaults are Log=false and SlowThreshold=1s, got %v %v", c.Log, c.SlowThreshold)
	}
	c.SlowThreshold = -time.Millisecond
	if err := c.Validate(); err == nil {
		t.Error("a negative SlowThreshold must be an error")
	}
}

func TestLog_RedirectPolicyErrorsScrubbed(t *testing.T) {
	// When a redirect policy refuses, the standard library puts the Location into the *url.Error's URL as it is: a relative Location
	// has no http:// and is not found by searching the text for a URL.
	token := secret(t, "token")
	mux := http.NewServeMux()
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b?token="+token+"#frag", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop?token="+token, http.StatusFound)
	})
	mux.HandleFunc("/space", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/a b?token="+token)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/badloc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/b%zz?token="+token)
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		name, path, want string
		policy           resty.RedirectPolicy
		status           float64
	}{
		{"NoRedirectPolicy", "/redir", `Get "/b": auto redirect is disabled`, resty.NoRedirectPolicy(), 302},
		{"a space before the question mark in Location", "/space", `Get "/a%20b": auto redirect is disabled`, resty.NoRedirectPolicy(), 302},
		{"FlexibleRedirectPolicy used up", "/loop", `Get "/loop": stopped after 2 redirects`, resty.FlexibleRedirectPolicy(2), 302},
		{"an invalid escape in Location", "/badloc", `failed to parse Location header "/b%zz"`, nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, lines := captureLogs(t)
			client := logClient(t)
			if c.policy != nil {
				client.SetRedirectPolicy(c.policy)
			}
			if _, err := client.R().Get(srv.URL + c.path); err == nil {
				t.Fatal("precondition: it must fail")
			}
			got := requestLines(lines(), allRequestMsgs...)
			if len(got) != 1 || got[0]["msg"] != "http request failed" || got[0]["status"] != c.status {
				t.Fatalf("it must be one http request failed line with status %v, got=%v", c.status, lines())
			}
			if e, _ := got[0]["error"].(string); !strings.Contains(e, c.want) {
				t.Errorf("error must keep the Location without its query string and the reason\n got=%q\nwant to contain %q", e, c.want)
			}
			if strings.Contains(raw(), token) {
				t.Errorf("a token from Location reached the log: %s", raw())
			}
		})
	}
}

func TestLog_QueryWithRawSpaceScrubbed(t *testing.T) {
	// An unescaped space in the query string: searching by text would stop at the space and leave the token after it in the log.
	token := secret(t, "token")
	raw, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get("http://" + deadAddr(t) + "/x?q=hello world&token=" + token)
	got := requestLines(lines(), "http request failed")
	if len(got) != 1 || !strings.Contains(fmt.Sprint(got[0]["error"]), `/x": dial tcp`) {
		t.Fatalf("it must be one http request failed line whose error URL stops at the path, got=%v", lines())
	}
	if strings.Contains(raw(), token) || strings.Contains(raw(), "world") {
		t.Errorf("the query string after the space reached the log: %s", raw())
	}
}

func TestScrubError(t *testing.T) {
	ue := func(u string) error { return &url.Error{Op: "Get", URL: u, Err: io.EOF} }
	for _, c := range []struct {
		err  error
		want string
	}{
		{ue("http://user:***@h:1/x?token=a#f"), `Get "http://h:1/x": EOF`},
		{ue("/b?token=a"), `Get "/b": EOF`},
		{ue("b?token=a"), `Get "b": EOF`},
		{ue("http://h/x?q=hello world&token=a"), `Get "http://h/x": EOF`},
		{ue("/a b?token=a"), `Get "/a%20b": EOF`},                   // a space before the question mark: not found by text, only by structure
		{ue("http://h/%zz x?token=a"), `Get "http://h/%zz x": EOF`}, // unparseable: cut at the first ?
		{ue("ws://u:p@h/%zz#token=a"), `Get "ws://h/%zz": EOF`},     // unparseable, and the userinfo is removed too
		{fmt.Errorf("wrapped: %w", ue("/w x?token=a")), `wrapped: Get "/w%20x": EOF`},
		{errors.Join(ue("/a b?token=a"), ue("/c d?token=b")), "Get \"/a%20b\": EOF\nGet \"/c%20d\": EOF"},
		{errors.New(`free text "/c?token=a" and http://h/d?token=a`), `free text "/c" and http://h/d`},
	} {
		got := scrubError(c.err)
		if got != c.want {
			t.Errorf("scrubError(%q)\n got=%q\nwant=%q", c.err, got, c.want)
		}
		if strings.Contains(got, "token") {
			t.Errorf("scrubError(%q) left a query string: %q", c.err, got)
		}
	}
}

func TestLog_UserLoggerStillGetsRestyWarnings(t *testing.T) {
	// Log being on must not replace a logger the caller set: a warning such as Basic Auth over plaintext HTTP has to reach them.
	srv, _ := echo(t, nil)
	for _, c := range []struct {
		name string
		req  func(*resty.Client, resty.Logger) *resty.Request
	}{
		{"client.SetLogger", func(c *resty.Client, l resty.Logger) *resty.Request { return c.SetLogger(l).R() }},
		{"R().SetLogger", func(c *resty.Client, l resty.Logger) *resty.Request { return c.R().SetLogger(l) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := logClient(t)
			my := &recLogger{}
			if _, err := c.req(client, my).SetBasicAuth("u", "p").Get(srv.URL); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(my.String(), "Basic Auth") {
				t.Errorf("the caller's own logger must receive resty's Basic Auth warning, got=%q", my.String())
			}
		})
	}
}

func TestLog_RestyConfigErrorsStillLogged(t *testing.T) {
	// When a configuration call is ignored, resty logs one ERROR (whose format is "%v" too), and Log being on must not swallow it as the used-up-retries line.
	// The transport is wrapped in the tracing layers and is not an *http.Transport, so SetProxy only logs this line and does nothing.
	_, lines := captureLogs(t)
	client := logClient(t)
	client.SetProxy("http://proxy.internal:8080")
	for _, l := range requestLines(lines(), "xhttp resty log") {
		if l["level"] == "ERROR" && strings.Contains(fmt.Sprint(l["detail"]), "not an *http.Transport") {
			return
		}
	}
	t.Errorf("the ERROR for an ignored SetProxy must not be swallowed, got=%v", lines())
}

// recLogger records every line it receives.
type recLogger struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *recLogger) add(level, f string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, level+" "+f+"\n", v...)
}
func (l *recLogger) Errorf(f string, v ...any) { l.add("ERROR", f, v...) }
func (l *recLogger) Warnf(f string, v ...any)  { l.add("WARN", f, v...) }
func (l *recLogger) Debugf(f string, v ...any) { l.add("DEBUG", f, v...) }
func (l *recLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestLog_FailureBeforeAnyAttemptElapsedZero(t *testing.T) {
	// With an SRV lookup finding nothing, resty calls OnError before the first attempt even starts: no start was recorded and Request.Time is the zero value.
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().SetSRV(&resty.SRVRecord{Service: "nope", Domain: "invalid.invalid"}).Get("/x")
	got := requestLines(lines(), "http request failed")
	if len(got) != 1 || got[0]["elapsed_ms"] != float64(0) || got[0]["attempts"] != float64(0) {
		t.Errorf("nothing went out: elapsed_ms 0, attempts 0, got=%v", lines())
	}
}

func TestLog_DecodeErrorOn200IsFailed(t *testing.T) {
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	})
	_, lines := captureLogs(t)
	client := logClient(t)
	var out struct{ A int }
	if _, err := client.R().SetResult(&out).Get(srv.URL); err == nil {
		t.Fatal("precondition: a failing SetResult must return an error")
	}
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["msg"] != "http request failed" || got[0]["status"] != float64(200) ||
		!strings.Contains(fmt.Sprint(got[0]["error"]), "invalid character") {
		t.Errorf("a 200 whose SetResult cannot be parsed: http request failed, status 200, carrying the parse error, got=%v", lines())
	}
}

func TestLog_SlowAndFailedLogsFailed(t *testing.T) {
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(40 * time.Millisecond); w.WriteHeader(503) })
	_, lines := captureLogs(t)
	client := logClient(t, func(c *Config) { c.SlowThreshold = 10 * time.Millisecond })
	client.R().Get(srv.URL)
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["msg"] != "http request failed" || got[0]["status"] != float64(503) {
		t.Errorf("slow and failed is recorded as http request failed, not as a slow request, got=%v", lines())
	}
}

func TestLog_WarnLevelHandlerStillGetsProblems(t *testing.T) {
	// With slog accepting WARN only, the shortcut that skips assembling the fields must not skip failures and slow requests too.
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(40 * time.Millisecond)
		}
		if r.URL.Path == "/fail" {
			w.WriteHeader(500)
		}
	})
	var buf strings.Builder
	w := &lockedWriter{w: &buf}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	client := logClient(t, func(c *Config) { c.SlowThreshold = 10 * time.Millisecond })
	for _, p := range []string{"/ok", "/fail", "/slow"} {
		client.R().Get(srv.URL + p)
	}
	w.mu.Lock()
	out := buf.String()
	w.mu.Unlock()
	if strings.Count(out, `"msg":"http request failed"`) != 1 || strings.Count(out, `"msg":"slow http request"`) != 1 ||
		strings.Contains(out, `"msg":"http request"`) {
		t.Errorf("a WARN-only handler must get one failure line and one slow line and no successful one, got=%s", out)
	}
}

func TestLog_DoNotParseResponseElapsedIsTimeToHeaders(t *testing.T) {
	// The response body is left to the caller: resty is done once it has the response headers, so the duration excludes reading the body.
	srv, _ := echo(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
	})
	_, lines := captureLogs(t)
	client := logClient(t)
	resp, err := client.R().SetDoNotParseResponse(true).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.RawBody().Close()
	got := requestLines(lines(), allRequestMsgs...)
	if e, _ := got[0]["elapsed_ms"].(float64); len(got) != 1 || e >= 100 {
		t.Errorf("with SetDoNotParseResponse, elapsed_ms is the time to the response headers, got=%v", lines())
	}
}

func TestLog_RejectedBeforeSendingNoLine(t *testing.T) {
	// One resty rejects before sending it (a GET with multipart) goes through OnInvalid, not OnError.
	srv, _ := echo(t, nil)
	_, lines := captureLogs(t)
	client := logClient(t)
	if _, err := client.R().SetMultipartField("f", "a.txt", "text/plain", strings.NewReader("x")).Get(srv.URL); err == nil {
		t.Fatal("precondition: a GET with multipart must be rejected by resty")
	}
	if got := requestLines(lines(), allRequestMsgs...); len(got) != 0 {
		t.Errorf("a request that never went out has no request log, got=%v", got)
	}
}

func TestMs_KeepsSubMillisecond(t *testing.T) {
	if got := ms(1500 * time.Microsecond); got != 1.5 {
		t.Errorf("ms(1.5ms) = %v, want 1.5", got)
	}
	if got := ms(250 * time.Microsecond); got != 0.25 {
		t.Errorf("ms(250µs) = %v, want 0.25", got)
	}
}

// deadAddr is an address nobody is listening on.
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestLog_PathIsEscaped(t *testing.T) {
	// The path is recorded in its escaped form: /a%2Fb is not recorded as /a/b.
	srv, _ := echo(t, nil)
	_, lines := captureLogs(t)
	client := logClient(t)
	client.R().Get(srv.URL + "/a%2Fb/c%20d")
	got := requestLines(lines(), allRequestMsgs...)
	if len(got) != 1 || got[0]["path"] != "/a%2Fb/c%20d" {
		t.Errorf("path must be the escaped /a%%2Fb/c%%20d, got=%v", lines())
	}
}
