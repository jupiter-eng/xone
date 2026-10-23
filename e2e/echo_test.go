package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The xecho cases. Until now xecho was the one web integration e2e never started: the module has
// the largest unit suite of the three, and not one line of it had ever run inside a real binary
// with a real signal.
//
// What is tested here is what no unit test reaches: that xone.MustRun(xecho.New()) brings an echo
// application up, that a real SIGTERM finishes the requests in flight and exits 0, that echo's two
// loggers really end inside slog in a real process (checkOutput fails the case on any line that is
// not JSON, which is exactly what an unrouted echo logger writes), and that the layers echo has and
// the other two integrations do not -- Pre, and the HTTPErrorHandler rendering after the handler
// returned -- still leave a complete access log, metric and span behind.
//
// The service is e2e/service-echo, which connects to nothing: what xecho does with a database is
// xgorm's business and service/ already covers it.

// echoAccessLog is this path's access log line, waited for and insisted to be the only one.
//
// The accessLog in functional_test.go waits by trace_id; this one waits by path, which is what
// these cases have to hand. Waited for rather than read straight away: the line is written once the
// response has gone out, so a client that already holds its answer can look before the server has
// written it.
func echoAccessLog(t *testing.T, p *harness.Process, path string) harness.Log {
	t.Helper()
	match := func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == path }
	p.WaitLog(t, 10*time.Second, match)
	logs := p.FindLogs(match)
	if len(logs) != 1 {
		t.Fatalf("exactly one access log line for %s was expected, got=%d\n%s", path, len(logs), p.Output())
	}
	return logs[0]
}

// startEcho starts the echo service with the spans written to a file.
func startEcho(t *testing.T, o harness.Options) *harness.Process {
	t.Helper()
	o.Spans = true
	return harness.StartEcho(t, o)
}

func TestEcho_ServesAndExitsCleanly(t *testing.T) {
	// The shape of the whole module in one case: routes registered through WithRoutes are served by
	// xone.MustRun(xecho.New()), the response carries the trace identifier, and a real SIGTERM
	// exits 0.
	harness.Require(t)
	p := startEcho(t, harness.Options{})

	if r := p.Get(t, "/ping"); r.Status != http.StatusOK || string(r.Body) != "pong" {
		t.Fatalf("GET /ping: %v", r)
	}

	r := p.Get(t, "/api/v1/users/42")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /api/v1/users/42: %v", r)
	}
	var u struct{ ID string }
	r.JSON(t, &u)
	if u.ID != "42" {
		t.Errorf("the handler should have seen the path parameter, got=%v", r)
	}
	if r.Header.Get("X-Trace-Id") == "" {
		t.Error("the response should carry the trace identifier back")
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("it did not exit with 0 after the SIGTERM: %v\n%s", exit, p.Output())
	}
}

func TestEcho_AccessLogAndSpanUseTheWholeRouteTemplate(t *testing.T) {
	// The route is registered inside a Group, which is how an echo application lays a version
	// prefix out. In a real process the whole template has to hold for the access log, the metrics
	// and the span name alike, or every id in a URL becomes a series and a span name of its own and
	// a later /api/v2 shares the labels of /api/v1.
	harness.Require(t)
	p := startEcho(t, harness.Options{})
	const route = "/api/v1/users/:id"

	for _, id := range []string{"1", "2", "3"} {
		if r := p.Get(t, "/api/v1/users/"+id); r.Status != http.StatusOK {
			t.Fatalf("GET /api/v1/users/%s: %v", id, r)
		}
	}

	line := echoAccessLog(t, p, "/api/v1/users/2")
	if got := line.Str("route"); got != route {
		t.Errorf("the access log should use the whole route template, got=%q", got)
	}
	if line.Str("trace_id") == "" {
		t.Errorf("the access log should carry trace_id, got=%v", line.Fields)
	}

	m := waitMetrics(t, p, `e2e_http_requests_total{route="`+route+`"} == 3`, func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", route) == 3
	})
	if n := m.Sum("e2e_http_requests_total", "route", route); n != 3 {
		t.Errorf("the three requests should fold into one series, got=%v\n%v", n, m.Find("e2e_http_requests_total"))
	}
	if n := m.Sum("e2e_http_requests_total", "route", "/api/v1/users/1"); n != 0 {
		t.Errorf("the real path must not reach the labels, got=%v", n)
	}

	span := p.WaitSpan(t, 5*time.Second, func(s harness.Span) bool { return s.Str("url.path") == "/api/v1/users/2" })
	if span.Name != "GET "+route {
		t.Errorf("the span name should use the whole route template, got=%q", span.Name)
	}
	if span.Str("http.route") != route {
		t.Errorf("the span should carry the route attribute, got=%v", span.Attributes)
	}
}

func TestEcho_GracefulShutdownFinishesRequestsInFlight(t *testing.T) {
	// The handler watches the request ctx; the framework waits for it rather than cutting it off.
	harness.Require(t)
	p := startEcho(t, harness.Options{})

	done := make(chan harness.Response, 1)
	go func() {
		r, err := p.Request(t.Context(), http.MethodGet, "/slow?ms=1500", nil)
		if err != nil {
			done <- harness.Response{Body: []byte("error: " + err.Error())}
			return
		}
		done <- r
	}()
	// Wait for the request to be inside the handler: the access log is written on the way out, so
	// what says it arrived is the span not being there yet and the server having accepted it. The
	// handler sleeps 1500ms, and 300ms is enough for the request to have crossed localhost.
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

func TestEcho_PanicIsRecoveredLoggedAndCounted(t *testing.T) {
	// A panic has to become one ERROR line through slog with the credential masked, a 500, an
	// access log line and a metric -- and the process has to survive it.
	harness.Require(t)
	p := startEcho(t, harness.Options{})

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
	if !strings.Contains(l.Str("stack"), "service-echo/main.go") {
		t.Errorf("the stack should point at the handler that panicked, got=%q", l.Str("stack"))
	}
	if l.Str("trace_id") == "" {
		t.Errorf("the panic line should carry this request's trace_id, got=%v", l.Fields)
	}

	if status, _ := echoAccessLog(t, p, "/boom").Get("status"); status != float64(500) {
		t.Errorf("the access log should record the 500, got=%v", status)
	}
	waitMetrics(t, p, `e2e_http_requests_total{route="/boom",status="500"} == 1`, func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/boom", "status", "500") == 1
	})

	if r := p.Get(t, "/ping"); r.Status != http.StatusOK {
		t.Errorf("the service should still be up after a panic, got=%v", r)
	}
}

func TestEcho_HTTPErrorHandlerRendersAndIsRecordedAsSent(t *testing.T) {
	// An *echo.HTTPError is rendered by echo's HTTPErrorHandler after the handler has returned,
	// which no other integration has. What the access log, the metrics and the span record has to
	// be the status that handler really sent, not the 200 the response carried before it ran.
	harness.Require(t)
	p := startEcho(t, harness.Options{})

	r := p.Get(t, "/teapot")
	if r.Status != http.StatusTeapot {
		t.Fatalf("the HTTPErrorHandler should have answered 418: %v", r)
	}

	if status, _ := echoAccessLog(t, p, "/teapot").Get("status"); status != float64(http.StatusTeapot) {
		t.Errorf("the access log should record the status the error handler sent, got=%v", status)
	}
	waitMetrics(t, p, `e2e_http_requests_total{route="/teapot",status="418"} == 1`, func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/teapot", "status", "418") == 1
	})

	// 4xx is the caller's mistake, not this service failing, so the span is not marked as an error
	span := p.WaitSpan(t, 5*time.Second, func(s harness.Span) bool { return s.Str("url.path") == "/teapot" })
	if span.StatusCode == "Error" {
		t.Errorf("a 418 is not a server error and should leave the span unmarked, got status_code=%q", span.StatusCode)
	}
}

func TestEcho_OwnLogsAreRoutedIntoSlog(t *testing.T) {
	// echo writes through two loggers of its own: e.Logger, and the std *log.Logger it prepares for
	// http.Server.ErrorLog. Either one left alone writes plain text to the console, which falls out
	// of every field-based query -- checkOutput fails the case on such a line when the process
	// exits, and this says what should be there instead.
	harness.Require(t)
	p := startEcho(t, harness.Options{})

	if r := p.Get(t, "/echo-log"); r.Status != http.StatusOK {
		t.Fatalf("GET /echo-log: %v", r)
	}

	l := p.WaitLog(t, 5*time.Second, func(l harness.Log) bool { return l.Msg() == "echo internal log" })
	if l.Level() != "ERROR" {
		t.Errorf("echo's level should survive the trip, got=%q", l.Level())
	}
	if got := l.Str("message"); got != "echo says hello" {
		t.Errorf("echo's message should come through whole, got=%q", got)
	}

	s := p.WaitLog(t, 5*time.Second, func(l harness.Log) bool { return l.Msg() == "xecho http server error" })
	if got := s.Str("error"); !strings.Contains(got, "echo std says hello") {
		t.Errorf("e.StdLogger should reach slog as the server's error log, got=%q", got)
	}
}

func TestEcho_PreRewrittenPathStillRoutesAndIsRecorded(t *testing.T) {
	// Pre runs before the router, which no other integration has: a middleware there can change the
	// path the router will match. The request still has to reach the access log, the metrics and a
	// span, and under the template it really routed to -- with the route resolved before Pre ran,
	// every rewritten request would be recorded as unmatched.
	harness.Require(t)
	p := startEcho(t, harness.Options{})

	r := p.Get(t, "/old/77")
	if r.Status != http.StatusOK {
		t.Fatalf("a path rewritten in Pre should still route, got=%v", r)
	}
	var u struct{ ID string }
	r.JSON(t, &u)
	if u.ID != "77" {
		t.Errorf("the rewritten path should reach the handler with its parameter, got=%v", r)
	}

	l := echoAccessLog(t, p, "/api/v1/users/77")
	if got := l.Str("route"); got != "/api/v1/users/:id" {
		t.Errorf("the route label should be the template it routed to, got=%q", got)
	}
	waitMetrics(t, p, `e2e_http_requests_total{route="/api/v1/users/:id",status="200"} >= 1`, func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/api/v1/users/:id", "status", "200") >= 1
	})
}

func TestEcho_ClientIPIgnoresAForgedHeaderFromAnUntrustedPeer(t *testing.T) {
	// echo's default IPExtractor believes whoever sent X-Forwarded-For. The framework installs one
	// from XEcho.TrustedProxies instead, and the handler reads the same value through c.RealIP.
	harness.Require(t)
	p := startEcho(t, harness.Options{Overlay: "XEcho:\n  TrustedProxies: []\n"})

	r := p.Get(t, "/api/v1/users/7", "X-Forwarded-For", "1.2.3.4")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /api/v1/users/7: %v", r)
	}
	var u struct {
		ClientIP string `json:"client_ip"`
	}
	r.JSON(t, &u)
	if u.ClientIP == "1.2.3.4" {
		t.Errorf("a forged X-Forwarded-For must not decide client_ip, got=%q", u.ClientIP)
	}

	got := echoAccessLog(t, p, "/api/v1/users/7").Str("client_ip")
	if got != u.ClientIP {
		t.Errorf("the handler and the access log should agree on client_ip, got=%q and %q", u.ClientIP, got)
	}
	if got == "1.2.3.4" {
		t.Errorf("a forged X-Forwarded-For must not reach the access log, got=%q", got)
	}
}

func TestEcho_ConfigMistakeFailsStartup(t *testing.T) {
	// A misspelled key in the XEcho block is reported at startup, not when the server listens.
	harness.Require(t)
	p := harness.StartEcho(t, harness.Options{
		Overlay: "XEcho:\n  Prot: 1234\n",
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
