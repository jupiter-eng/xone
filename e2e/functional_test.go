package e2e

// The functional end-to-end tests: the TestFunctional_* family.
//
//	scripts/e2e.sh -run Functional
//
// Every assertion follows the behavior written down in README.md / docs/architecture.md /
// docs/config.md / each module's README (what is not in the documentation and lives in the code
// alone is asserted from the code, with the source named in a comment). A failure message always
// reads "the documentation says X, got Y".
//
// How the files divide up:
//
//	functional_test.go          this file: the shared helpers
//	functional_api_test.go      what every endpoint answers
//	functional_cache_test.go    the three-level read, local cache -> Redis -> PG
//	functional_log_test.go      the access log fields, trace_id throughout, the redaction, no PG password leaking, the SQL log
//	functional_metrics_test.go  /metrics
//	functional_trace_test.go    the span attributes and the parent / child relations, traceparent, the forwarded headers
//	functional_flow_test.go     /boom and the xflow rollback
//	functional_config_test.go   a malformed configuration fails the start
//
// The numbers (timings, counts) are printed with t.Logf behind a "numbers:" prefix at the start of
// the line, to grep out of a -v run.

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// waitFor is the limit for waiting on a log line, a span or a metric.
//
// Measured on this machine (4 cores, every e2e case in parallel, -count=3, about 70 waits): after
// the response reaches the client, the access log is there after 0.05ms at p50 and 4.1ms at worst,
// and the server span lands on disk after 0.7ms at p50 and 3.9ms at worst.
// 5s is three orders of magnitude more than the worst of those, so a slow machine or CI does not
// make it flaky
const waitFor = 5 * time.Second

var (
	hexTraceID = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hexSpanID  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// randomTraceID / randomSpanID stand in for the trace identifiers an upstream hands over
func randomTraceID() string { return harness.NewID() + harness.NewID() + harness.NewID()[:8] }
func randomSpanID() string  { return harness.NewID() + harness.NewID()[:4] }

// traceIDOf is X-Trace-Id from the response headers.
// xgin/README.md, XGin.Trace: "one server span per request, with X-Trace-Id in the response headers"
func traceIDOf(t *testing.T, r harness.Response) string {
	t.Helper()
	id := r.Header.Get("X-Trace-Id")
	if !hexTraceID.MatchString(id) {
		t.Fatalf("the documentation says X-Trace-Id comes back in the response headers while XGin.Trace is on, got %q (%v)", id, r)
	}
	return id
}

// serverSpan waits for this trace's server span.
//
// The child spans (SQL, Redis, outbound HTTP) all end before the handler returns and the server
// span ends after the response has been written, and the export is synchronous, so by the time it
// is there the whole trace is
func serverSpan(t *testing.T, p *harness.Process, traceID string) harness.Span {
	t.Helper()
	return p.WaitSpan(t, waitFor, func(s harness.Span) bool { return s.TraceID == traceID && s.Kind == "server" })
}

// traceSpans are all the spans of this trace exported so far, sorted by start time
func traceSpans(t *testing.T, p *harness.Process, traceID string) []harness.Span {
	t.Helper()
	var out []harness.Span
	for _, s := range p.Spans(t) {
		if s.TraceID == traceID {
			out = append(out, s)
		}
	}
	for i := 1; i < len(out); i++ { // an insertion sort: one trace holds only a few spans
		for j := i; j > 0 && out[j].Start.Before(out[j-1].Start); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// spansNamed picks by name
func spansNamed(spans []harness.Span, name string) []harness.Span {
	var out []harness.Span
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// clientSpanNames are the names of the client spans (SQL, Redis, outbound HTTP), by start time and comma separated
func clientSpanNames(spans []harness.Span) string {
	var names []string
	for _, s := range spans {
		if s.Kind == "client" {
			names = append(names, s.Name)
		}
	}
	return strings.Join(names, ",")
}

// spanNames is for the debug output
func spanNames(spans []harness.Span) string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name + "(" + s.Kind + ")"
	}
	return strings.Join(names, ", ")
}

// accessLog waits for this trace's access log line.
//
// When none turns up it tells the two cases apart: no access log was written at all, or one was
// and its trace_id is not this request's -- the latter being the README's "the logs carry trace_id
// automatically" gone unkept, which a bare "no log turned up" would not show
func accessLog(t *testing.T, p *harness.Process, traceID string) harness.Log {
	t.Helper()
	isAccess := func(l harness.Log) bool { return l.Msg() == "request completed" }
	if l, ok := p.LookForLog(waitFor, func(l harness.Log) bool { return isAccess(l) && l.Str("trace_id") == traceID }); ok {
		return l
	}
	var ids []string
	for _, l := range p.FindLogs(isAccess) {
		ids = append(ids, fmt.Sprintf("%q", l.Str("trace_id")))
	}
	t.Fatalf("waited %v and no access log line with trace_id=%s turned up. The README: the logs carry trace_id automatically, the same one as the X-Trace-Id response header; "+
		"the trace_ids of the %d access log lines the process already holds are [%s]", waitFor, traceID, len(ids), strings.Join(ids, ", "))
	return harness.Log{}
}

// waitMetrics scrapes /metrics until cond holds and returns the last result.
//
// The request metrics are recorded in the middleware's defer, which in principle may come after
// the response reaches the client, so an assertion cannot follow the request straight away
func waitMetrics(t *testing.T, p *harness.Process, what string, cond func(harness.Metrics) bool) harness.Metrics {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for {
		m := p.Metrics(t)
		if cond(m) {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %v and /metrics still does not satisfy: %s", waitFor, what)
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustNotContain: none of the secrets may appear in text
func mustNotContain(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if i := strings.Index(text, s); i >= 0 {
			lo, hi := max(0, i-120), min(len(text), i+len(s)+120)
			t.Errorf("%s holds %q, which it must not: ...%s...", where, s, text[lo:hi])
		}
	}
}

// The Overlay fragments for the switches. Fragments whose top-level blocks differ can simply be
// concatenated; several settings inside one top-level block have to go into one fragment (the same
// key twice is an error in YAML). service/application.yml writes none of these, so without an
// overlay they are the framework's defaults
const (
	// payloadLogs turns on everything the access log can record: the query string, the request headers, the request body, the response headers and the response body
	payloadLogs = "XGin:\n  LogQuery: true\n  LogRequestHeaders: true\n  LogRequestBody: true\n" +
		"  LogResponseHeaders: true\n  LogResponseBody: true\n"
	// debugLogs sets the log level to debug
	debugLogs = "XLog:\n  Level: debug\n"
)

// logLevel is the log level
func logLevel(level string) string { return "XLog:\n  Level: " + level + "\n" }

// sampleRatio is the trace sampling ratio
func sampleRatio(r string) string { return "XTrace:\n  SampleRatio: " + r + "\n" }

// stopTimeout is the budget for the whole shutdown (Service.StopTimeout -> xone.WithStopTimeout)
func stopTimeout(d time.Duration) string { return fmt.Sprintf("Service:\n  StopTimeout: %v\n", d) }

// sqlLog turns the SQL log on for these xgorm instances (default being PG, plus mysql and ch)
func sqlLog(clients ...string) string {
	s := "XGorm:\n  Clients:\n"
	for _, c := range clients {
		s += "    " + c + ":\n      Log: true\n"
	}
	return s
}

// pgPassword is the password the harness connects to PG with, taken out of the DSN it builds rather than written out here once more
func pgPassword(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(harness.PGDSN(harness.PGAddr()))
	if err != nil {
		t.Fatalf("parse harness DSN: %v", err)
	}
	pw, ok := u.User.Password()
	if !ok || pw == "" {
		t.Fatal("harness DSN has no password")
	}
	return pw
}

// user is a user as the endpoints see it
type user struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Source string `json:"source"`
}

// createUser POSTs /users and wants a 201
func createUser(t *testing.T, p *harness.Process, name, email string) user {
	t.Helper()
	r := p.PostJSON(t, "/users", map[string]string{"name": name, "email": email})
	if r.Status != 201 {
		t.Fatalf("POST /users should answer 201, got %v", r)
	}
	var u user
	r.JSON(t, &u)
	if u.ID <= 0 {
		t.Fatalf("POST /users answered no id: %v", r)
	}
	return u
}

// getUser GETs /users/:id, wants a 200 and returns the user and how long it took
func getUser(t *testing.T, p *harness.Process, id int64) (user, harness.Response, time.Duration) {
	t.Helper()
	start := time.Now()
	r := p.Get(t, fmt.Sprintf("/users/%d", id))
	elapsed := time.Since(start)
	if r.Status != 200 {
		t.Fatalf("GET /users/%d should answer 200, got %v", id, r)
	}
	var u user
	r.JSON(t, &u)
	return u, r, elapsed
}

// num is a numeric field of a log line (which JSON decodes as a float64)
func num(l harness.Log, path string) (float64, bool) {
	v, ok := l.Get(path)
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}
