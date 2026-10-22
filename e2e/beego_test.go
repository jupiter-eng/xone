package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The xbeego cases. What is tested here is what no unit test reaches: a real process, a real
// signal, and beego's three globals (its router, the recovery slot in web.BConfig and its logs
// package) taken over inside a real binary.
//
// The service is e2e/service-beego, which connects to nothing -- what xbeego does with a database
// is xgorm's business and service/ already covers it.

// beegoAccessLog is this path's access log line, waited for and insisted to be the only one.
//
// The accessLog in functional_test.go waits by trace_id; this one waits by path, which is what these
// cases have to hand.
//
// Waited for rather than read straight away: the line is written once the response has gone out, so
// a client that already holds its answer can look before the server has written it. Read straight
// away it passed everywhere and then failed on a loaded CI runner with "got=0", which is what the
// rest of the suite uses LookForLog / WaitLog for.
func beegoAccessLog(t *testing.T, p *harness.Process, path string) harness.Log {
	t.Helper()
	match := func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == path }
	p.WaitLog(t, 10*time.Second, match)
	logs := p.FindLogs(match)
	if len(logs) != 1 {
		t.Fatalf("exactly one access log line for %s was expected, got=%d\n%s", path, len(logs), p.Output())
	}
	return logs[0]
}

// startBeego starts the beego service with the spans written to a file.
func startBeego(t *testing.T, o harness.Options) *harness.Process {
	t.Helper()
	o.Spans = true
	return harness.StartBeego(t, o)
}

func TestBeego_ServesTheGlobalRouterAndExitsCleanly(t *testing.T) {
	// The shape of the whole module in one case: routes registered on web.BeeApp from package-level
	// web.Get / web.Router are served by xone.MustRun(xbeego.New()), and a real SIGTERM exits 0.
	harness.Require(t)
	p := startBeego(t, harness.Options{})

	if r := p.Get(t, "/ping"); r.Status != http.StatusOK || string(r.Body) != "pong" {
		t.Fatalf("GET /ping: %v", r)
	}

	r := p.Get(t, "/users/42")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /users/42: %v", r)
	}
	var u struct{ ID string }
	r.JSON(t, &u)
	if u.ID != "42" {
		t.Errorf("the controller should have seen the path parameter, got=%v", r)
	}
	if r.Header.Get("X-Trace-Id") == "" {
		t.Error("the response should carry the trace identifier back")
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("it did not exit with 0 after the SIGTERM: %v\n%s", exit, p.Output())
	}
}

func TestBeego_AccessLogAndSpanUseTheRouteTemplate(t *testing.T) {
	// The route template only exists inside beego and is carried out by a filter. In a real process
	// that has to hold for the access log, the metrics and the span name alike, or every id in a URL
	// becomes a series and a span name of its own.
	harness.Require(t)
	p := startBeego(t, harness.Options{})

	for _, id := range []string{"1", "2", "3"} {
		if r := p.Get(t, "/users/"+id); r.Status != http.StatusOK {
			t.Fatalf("GET /users/%s: %v", id, r)
		}
	}

	line := beegoAccessLog(t, p, "/users/2")
	if got := line.Str("route"); got != "/users/:id" {
		t.Errorf("the access log should use the route template, got=%q", got)
	}
	if line.Str("trace_id") == "" {
		t.Errorf("the access log should carry trace_id, got=%v", line.Fields)
	}

	// The three requests fold into one series rather than three. Waited for: the middleware records
	// the metric in a defer, which in principle lands after the response has reached the client.
	m := waitMetrics(t, p, `e2e_http_requests_total{route="/users/:id"} == 3`, func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/users/:id") == 3
	})
	if n := m.Sum("e2e_http_requests_total", "route", "/users/:id"); n != 3 {
		t.Errorf("the three requests should fold into one series, got=%v\n%v", n, m.Find("e2e_http_requests_total"))
	}
	if n := m.Sum("e2e_http_requests_total", "route", "/users/1"); n != 0 {
		t.Errorf("the real path must not reach the labels, got=%v", n)
	}

	span := p.WaitSpan(t, 5*time.Second, func(s harness.Span) bool { return s.Str("url.path") == "/users/2" })
	if span.Name != "GET /users/:id" {
		t.Errorf("the span name should use the route template, got=%q", span.Name)
	}
	if span.Str("http.route") != "/users/:id" {
		t.Errorf("the span should carry the route attribute, got=%v", span.Attributes)
	}
}

func TestBeego_MetricsScrapeIsNotRecordedAsUnmatched(t *testing.T) {
	// The endpoint is answered before beego's router, so nothing else can fill its route template
	// in. Recorded as unmatched, every scrape would land in the bucket a dashboard watches for
	// scanners and typos -- at scrape frequency.
	harness.Require(t)
	p := startBeego(t, harness.Options{})

	p.Metrics(t) // the first scrape is counted by the time of the second
	m := waitMetrics(t, p, `e2e_http_requests_total{route="/metrics"} >= 1`, func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/metrics") >= 1
	})
	if n := m.Sum("e2e_http_requests_total", "route", "unmatched"); n != 0 {
		t.Errorf("a scrape must not land in the unmatched bucket, got=%v", n)
	}
}

func TestBeego_GracefulShutdownFinishesRequestsInFlight(t *testing.T) {
	// The handler watches the request ctx; the framework waits for it rather than cutting it off.
	harness.Require(t)
	p := startBeego(t, harness.Options{})

	done := make(chan harness.Response, 1)
	go func() {
		r, err := p.Request(t.Context(), "GET", "/slow?ms=1500", nil)
		if err != nil {
			done <- harness.Response{Body: []byte("error: " + err.Error())}
			return
		}
		done <- r
	}()
	// Let it reach the handler before the signal
	time.Sleep(300 * time.Millisecond)

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("it did not exit with 0: %v\n%s", exit, p.Output())
	}

	select {
	case r := <-done:
		if r.Status != http.StatusOK || string(r.Body) != "slow done" {
			t.Errorf("the request in flight should have been finished, got=%v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request in flight never answered")
	}
}

func TestBeego_PanicIsRecoveredLoggedAndCounted(t *testing.T) {
	// Beego's own recovery writes plain text with no trace_id; xbeego takes that slot over. In a
	// real process the panic has to become one ERROR line through slog with the credential masked,
	// a 500, an access log line and a metric -- and the process has to survive it.
	harness.Require(t)
	p := startBeego(t, harness.Options{})

	if r := p.Get(t, "/boom"); r.Status != http.StatusInternalServerError {
		t.Fatalf("GET /boom should answer 500, got=%v", r)
	}

	l := p.WaitLog(t, 5*time.Second, func(l harness.Log) bool { return l.Msg() == "panic while handling request" })
	if l.Level() != "ERROR" {
		t.Errorf("a panic is an ERROR, got=%q", l.Level())
	}
	if strings.Contains(l.Str("error"), "hunter2") {
		t.Errorf("the credential inside the panic value should be masked, got=%q", l.Str("error"))
	}
	// Taken inside beego's own recovery, where the stack still points at the handler
	if !strings.Contains(l.Str("stack"), "service-beego/main.go") {
		t.Errorf("the stack should point at the handler that panicked, got=%q", l.Str("stack"))
	}
	if l.Str("trace_id") == "" {
		t.Errorf("the panic line should carry this request's trace_id, got=%v", l.Fields)
	}

	if status, _ := beegoAccessLog(t, p, "/boom").Get("status"); status != float64(500) {
		t.Errorf("the access log should record the 500, got=%v", status)
	}
	waitMetrics(t, p, `e2e_http_requests_total{route="/boom",status="500"} == 1`, func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/boom", "status", "500") == 1
	})

	// The process is still serving
	if r := p.Get(t, "/ping"); r.Status != http.StatusOK {
		t.Errorf("the service should still be up after a panic, got=%v", r)
	}
}

func TestBeego_AbortRendersTheRegisteredErrorHandler(t *testing.T) {
	// Regression. c.Abort("418") is how a beego application answers an error code it registered a
	// handler for: the controller panics with the code as a string rather than with web.ErrAbort.
	// Taking beego's recovery over used to turn that into a 500 with an ERROR line per request.
	harness.Require(t)
	p := startBeego(t, harness.Options{})

	r := p.Get(t, "/teapot")
	if r.Status != http.StatusTeapot || string(r.Body) != "i am a teapot" {
		t.Fatalf("the registered error handler should have answered: %v", r)
	}
	// And Context.Abort, which sets the status and then panics with the body, keeps its status
	if r := p.Get(t, "/forbid"); r.Status != http.StatusForbidden {
		t.Errorf("Context.Abort should keep the status it set, got=%v", r)
	}

	// Rendering a registered handler is control flow and must not be logged as a crash. The
	// /forbid above is a different case: "nope" names no handler, so beego's own recovery logs it
	// too (logs.Critical in defaultRecoverPanic) and so does this one
	for _, l := range p.Logs() {
		if l.Msg() == "panic while handling request" && l.Str("path") == "/teapot" {
			t.Errorf("an abort onto a registered error handler is control flow, not a crash: %v", l.Fields)
		}
	}
}

func TestBeego_OwnLogsAreRoutedIntoSlog(t *testing.T) {
	// Beego's logs package writes plain text to the console, which falls out of every field-based
	// query. checkOutput would catch the line when the process exits; this says what it should be
	// instead.
	harness.Require(t)
	p := startBeego(t, harness.Options{})

	if r := p.Get(t, "/beego-log"); r.Status != http.StatusOK {
		t.Fatalf("GET /beego-log: %v", r)
	}

	l := p.WaitLog(t, 5*time.Second, func(l harness.Log) bool { return l.Msg() == "beego internal log" })
	if l.Level() != "WARN" {
		t.Errorf("beego's level should survive the trip, got=%q", l.Level())
	}
	// Beego hands the format string and its arguments over separately
	if got := l.Str("message"); got != "beego says hello" {
		t.Errorf("the format string and its arguments should be put together, got=%q", got)
	}
}

func TestBeego_ClientIPIgnoresAForgedHeaderFromAnUntrustedPeer(t *testing.T) {
	// Beego's own ctx.Input.IP() walks X-Forwarded-For from the left and believes whoever sent it.
	// The framework's client_ip comes from XBeego.TrustedProxies instead, and the controller reads
	// the same value through xbeego.ClientIP.
	harness.Require(t)
	p := startBeego(t, harness.Options{Overlay: "XBeego:\n  TrustedProxies: []\n"})

	r := p.Get(t, "/users/7", "X-Forwarded-For", "1.2.3.4")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /users/7: %v", r)
	}
	var u struct {
		ClientIP string `json:"client_ip"`
	}
	r.JSON(t, &u)
	if u.ClientIP == "1.2.3.4" {
		t.Errorf("a forged X-Forwarded-For must not decide client_ip, got=%q", u.ClientIP)
	}

	got := beegoAccessLog(t, p, "/users/7").Str("client_ip")
	if got != u.ClientIP {
		t.Errorf("the handler and the access log should agree on client_ip, got=%q and %q", u.ClientIP, got)
	}
	if got == "1.2.3.4" {
		t.Errorf("a forged X-Forwarded-For must not reach the access log, got=%q", got)
	}
}

func TestBeego_ConfigMistakeFailsStartup(t *testing.T) {
	// A misspelled key in the XBeego block is reported at startup, not when the server listens.
	harness.Require(t)
	p := harness.StartBeego(t, harness.Options{
		Overlay: "XBeego:\n  Prot: 1234\n",
		NoWait:  true,
		NonJSON: "the configuration error is reported before xlog is installed",
	})

	exit, ok := p.Wait(20 * time.Second)
	if !ok {
		t.Fatalf("a misconfigured service should not keep running\n%s", p.Output())
	}
	if exit.Code == 0 {
		t.Errorf("it should have exited non-zero, got=%v", exit)
	}
	if out := p.Output(); !strings.Contains(out, "Prot") {
		t.Errorf("the error should name the key that is wrong:\n%s", out)
	}
}
