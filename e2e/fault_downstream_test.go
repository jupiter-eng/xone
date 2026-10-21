package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The defaults of XHttp in xhttp/README.md, and the Timeout this group configures
const (
	faultHTTPTimeout          = 300 * time.Millisecond // XHttp.Timeout, the same number as the "measured 1.24s" example in the documentation
	faultHTTPRetryWaitTime    = 100 * time.Millisecond // RetryWaitTime's default
	faultHTTPRetryMaxWaitTime = 2 * time.Second        // RetryMaxWaitTime's default
)

// faultHTTPWorstCase, xhttp/README.md: "with RetryCount on, the worst case is
// (RetryCount+1) x Timeout plus a few backoff waits". One backoff stays under RetryMaxWaitTime
func faultHTTPWorstCase(retries int) time.Duration {
	return time.Duration(retries+1)*faultHTTPTimeout + time.Duration(retries)*faultHTTPRetryMaxWaitTime
}

// faultDownstreamCall is the result of one downstream call through /proxy
type faultDownstreamCall struct {
	resp harness.Response
	took time.Duration
}

// faultStartDownstream starts a service whose /proxy reaches downstream: Timeout 300ms, with
// overlay laid over XHttp
func faultStartDownstream(t *testing.T, downstream, overlay string) *harness.Process {
	t.Helper()
	return harness.Start(t, harness.Options{Downstream: downstream, Overlay: "XHttp:\n  Timeout: " + faultHTTPTimeout.String() + "\n" + overlay})
}

// faultCallDownstream GET /proxy?token=...[&method=...]
func faultCallDownstream(t *testing.T, p *harness.Process, token, method string) faultDownstreamCall {
	t.Helper()
	path := "/proxy?token=" + token
	if method != "" {
		path += "&method=" + method
	}
	r, took := faultTimed(t, p, http.MethodGet, path, nil)
	return faultDownstreamCall{r, took}
}

// faultRestyLogs are resty's log lines wired into slog by xhttp, counted by level.
// resty records them with RetryCount > 0 alone: one WARN per failed attempt (..., Attempt N) and
// one ERROR once they are used up (Execute in resty v2.17.2's request.go; the first row of the
// table in xhttp/README.md, "Behavior (measured)")
func faultRestyLogs(p *harness.Process) (warn, errs int, lines []harness.Log) {
	for _, l := range p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xhttp resty log" }) {
		switch l.Level() {
		case "WARN":
			warn++
		case "ERROR":
			errs++
		}
		lines = append(lines, l)
	}
	return warn, errs, lines
}

// faultWaitRestyLogs waits for resty's ERROR line to turn up (it is written before the handler
// returns, but the harness reads the output with a delay) and then checks that the token in this
// request's query string never reached the logs:
//
//	not one line wired into slog may hold it (xhttp/README.md, XHttp: "the URL drops its query string")
//	nor may stderr: with resty's log not wired into slog, resty's own logger writes
//	"WARN RESTY Get \".../echo?token=...\"" to stderr, and those lines go around slog, which looking at the slog lines alone would never find.
//	While xlog is running it writes to stdout, so stderr holds what goes around slog alone, and that is checked whole
func faultWaitRestyLogs(t *testing.T, p *harness.Process, token string) (warn, errs int, lines []harness.Log) {
	t.Helper()
	if _, ok := p.LookForLog(waitFor, func(l harness.Log) bool { return l.Msg() == "xhttp resty log" && l.Level() == "ERROR" }); !ok {
		t.Errorf("xhttp/README.md says resty's log is wired into slog (the message xhttp resty log): the ERROR line for the retries being used up did not turn up within %v; resty's own plain text in stderr holds %d lines",
			waitFor, strings.Count(p.Stderr(), " RESTY "))
	}
	warn, errs, lines = faultRestyLogs(p)
	for _, l := range lines {
		mustNotContain(t, "resty's log line", l.Line, token)
	}
	mustNotContain(t, "the process's stderr (what goes around slog)", p.Stderr(), token)
	return warn, errs, lines
}

// The downstream is slower than XHttp.Timeout (the stub waits 2s per request):
//
//	xhttp/README.md, XHttp.Timeout: "the timeout of one attempt", "it governs one attempt and not one
//	  logical request: with RetryCount on, the worst case is (RetryCount+1) x Timeout plus a few backoff waits"
//	RetryCount: "0 by default, meaning no retry"
//	RetryOnlyIdempotent: "it retries idempotent methods alone by default", "turn it off once the endpoint is known to be idempotent"
//	what is retried: "a transport error alone is retried ... once a response has arrived there is no retry -- a 5xx is not retried either"
//	"to bound a whole logical request, use the caller's ctx, which every attempt and every backoff between them honors"
//
// The number of attempts is counted by the downstream stub, and each attempt's length is measured
// from the arrival times it records
func TestFault_DownstreamSlowerThanTimeout_PerAttemptFail_IdempotentRetryOnly(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("no retry by default and one attempt failing at Timeout", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, "")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), "")
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("a downstream timeout should answer 502, got %v", c.resp)
		}
		if n := stub.Count(); n != 1 {
			t.Errorf("the documentation says RetryCount is 0 by default and nothing is retried, so the downstream should see 1 request, got %d", n)
		}
		if c.took < faultHTTPTimeout || c.took > faultHTTPTimeout+faultSlack {
			t.Errorf("the documentation says Timeout governs one attempt: with %v configured it should fail then, got %v", faultHTTPTimeout, c.took)
		}
		t.Logf("numbers: Timeout=%v with no retry: 502 after %s (%s)", faultHTTPTimeout, faultMS(c.took), c.resp.Body)
	})

	t.Run("a GET retries as RetryCount says, each attempt has its own Timeout and the total stays within the documented worst case", func(t *testing.T) {
		t.Parallel()
		const retries = 2
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, fmt.Sprintf("  RetryCount: %d\n", retries))
		token := "t-" + harness.NewID()
		start := time.Now()
		c := faultCallDownstream(t, p, token, "")
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("every attempt timed out, so it should answer 502, got %v", c.resp)
		}
		reqs := stub.Requests()
		if len(reqs) != retries+1 {
			t.Fatalf("the documentation says with RetryCount=%d a GET timeout is a transport error and %d requests should go out in all, and the downstream saw %d", retries, retries+1, len(reqs))
		}
		if lo, hi := time.Duration(retries+1)*faultHTTPTimeout, faultHTTPWorstCase(retries); c.took < lo || c.took > hi+faultSlack {
			t.Errorf("the documentation says the worst case is (RetryCount+1)xTimeout plus the backoffs = %v at most, while every attempt waiting out Timeout is %v at least, got %v", hi, lo, c.took)
		}
		// The gap between two arrivals = the previous attempt (cut off at Timeout) + one backoff (from RetryWaitTime, under RetryMaxWaitTime)
		var gaps []time.Duration
		for i := 1; i < len(reqs); i++ {
			gap := reqs[i].At.Sub(reqs[i-1].At)
			gaps = append(gaps, gap)
			if gap < faultHTTPTimeout+faultHTTPRetryWaitTime-20*time.Millisecond || gap > faultHTTPTimeout+faultHTTPRetryMaxWaitTime+faultSlack {
				t.Errorf("attempts %d and %d were %v apart: it should be one Timeout (%v) plus one backoff (%v to %v)", i, i+1, gap, faultHTTPTimeout, faultHTTPRetryWaitTime, faultHTTPRetryMaxWaitTime)
			}
		}
		last := start.Add(c.took).Sub(reqs[len(reqs)-1].At)
		if last < faultHTTPTimeout-20*time.Millisecond || last > faultHTTPTimeout+faultSlack {
			t.Errorf("the last attempt should be cut off at Timeout (%v), and it took %v from its arrival to the 502", faultHTTPTimeout, last)
		}
		for _, r := range reqs {
			if r.Query.Get("token") != token || r.Method != http.MethodGet {
				t.Errorf("every retry should be the same GET with the same query string, got %s %s?%s", r.Method, r.Path, r.RawQuery)
			}
		}
		t.Logf("numbers: Timeout=%v with RetryCount=%d: the downstream saw %d requests %v apart, the last was cut off after %s, %s in all (the documented worst case being %v)",
			faultHTTPTimeout, retries, len(reqs), gaps, faultMS(last), faultMS(c.took), faultHTTPWorstCase(retries))

		// The table in xhttp/README.md, "Behavior (measured)": resty's log is "wired into slog (the message xhttp resty log, with the content in the detail field),
		// keeping the level and dropping the URL's query string"; "with the retries on, every failure writes a WARN ... and one ERROR once they are used up"
		warn, errs, lines := faultWaitRestyLogs(t, p, token)
		if warn != retries+1 || errs != 1 {
			t.Errorf("the documentation says one WARN per failure and one ERROR once they are used up: %d failures should be %d WARN + 1 ERROR, got %d WARN + %d ERROR", retries+1, retries+1, warn, errs)
		}
		for _, l := range lines {
			if !strings.Contains(l.Str("detail"), "/echo") {
				t.Errorf("the detail of resty's log line should hold the failing URL, got %s", l.Line)
			}
		}
	})

	t.Run("a POST is not retried", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, "  RetryCount: 2\n")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), http.MethodPost)
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("a downstream timeout should answer 502, got %v", c.resp)
		}
		if n := stub.Count(); n != 1 {
			t.Errorf("the documentation says RetryOnlyIdempotent is on by default and idempotent methods alone are retried: a POST timeout should not be resent, and the downstream saw %d requests", n)
		}
		if last := stub.Last(t); last.Method != http.MethodPost {
			t.Errorf("the downstream should see a POST, got %s", last.Method)
		}
		if c.took > faultHTTPTimeout+faultSlack {
			t.Errorf("without a retry there is one attempt, which should fail at about Timeout (%v), got %v", faultHTTPTimeout, c.took)
		}
		t.Logf("numbers: RetryCount=2 with a POST: the downstream saw %d requests, 502 after %s", stub.Count(), faultMS(c.took))
	})

	t.Run("with RetryOnlyIdempotent off a POST is retried as well", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, "  RetryCount: 2\n  RetryOnlyIdempotent: false\n")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), http.MethodPost)
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("a downstream timeout should answer 502, got %v", c.resp)
		}
		if n := stub.Count(); n != 3 {
			t.Errorf("with RetryOnlyIdempotent: false a POST is retried as RetryCount=2 says, which is 3 requests in all, and the downstream saw %d", n)
		}
		t.Logf("numbers: RetryCount=2, RetryOnlyIdempotent=false, a POST: the downstream saw %d requests, 502 after %s", stub.Count(), faultMS(c.took))
	})

	t.Run("a downstream 5xx is not retried", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetStatus(http.StatusServiceUnavailable)
		stub.SetBody("application/json", `{"down":true}`)
		p := faultStartDownstream(t, stub.URL, "  RetryCount: 2\n")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), "")
		if c.resp.Status != http.StatusServiceUnavailable || string(c.resp.Body) != `{"down":true}` {
			t.Errorf("a response from the downstream is passed back as it is, so it should be 503 {\"down\":true}, got %v", c.resp)
		}
		if n := stub.Count(); n != 1 {
			t.Errorf("the documentation says \"once a response has arrived there is no retry -- a 5xx is not retried either\", and the downstream saw %d requests", n)
		}
		t.Logf("numbers: RetryCount=2 with the downstream answering 503: the downstream saw %d requests, returned after %s", stub.Count(), faultMS(c.took))
	})

	t.Run("nothing is retried once the caller has given up", func(t *testing.T) {
		t.Parallel()
		const retries, giveUp = 5, 450 * time.Millisecond
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, fmt.Sprintf("  RetryCount: %d\n", retries))
		// The caller disconnects mid-way through the second attempt: /proxy uses the request's ctx, which is cancelled when the connection breaks
		ctx, cancel := context.WithTimeout(context.Background(), giveUp)
		defer cancel()
		_, err := p.Request(ctx, http.MethodGet, "/proxy?token=t", nil)
		if err == nil {
			t.Fatalf("the caller gave up after %v, so the request should have no response", giveUp)
		}
		time.Sleep(100 * time.Millisecond) // one attempt may have been on its way at the moment it gave up
		atGiveUp := stub.Count()
		time.Sleep(faultHTTPWorstCase(retries) / 4) // without honoring ctx, two or three more would go out in this stretch
		after := stub.Count()
		if after != atGiveUp {
			t.Errorf("the documentation says every attempt and every backoff honors the caller's ctx: the downstream had seen %d requests when the caller gave up and %d more afterwards", atGiveUp, after-atGiveUp)
		}
		if atGiveUp > 2 {
			t.Errorf("two attempts fit into %v at most (each %v plus a backoff from %v), and the downstream saw %d", giveUp, faultHTTPTimeout, faultHTTPRetryWaitTime, atGiveUp)
		}
		t.Logf("numbers: RetryCount=%d with the caller giving up after %v: the downstream saw %d requests in all (it would be %d without the give-up)", retries, giveUp, after, retries+1)
	})
}

// The downstream process died (connections refused) and its host is down (the SYN never answered):
// both are transport errors, and a GET is retried as RetryCount says all the same.
// With the host down the dial hangs, and what holds it is Timeout: xhttp/README.md calls it "the
// timeout of one attempt", and the dial is inside one attempt (DialTimeout is 30s by default, far
// over Timeout, so Timeout arrives first).
// With connections refused every attempt fails at once and the total is the backoffs alone.
// The downstream stub cannot count the attempts (no connection reaches it), so the WARN resty
// records per failure is counted instead
func TestFault_DownstreamRefusedOrDown_GETRetriesRetryCount_EachWithinTimeout(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const retries = 2
	for _, c := range []struct {
		name   string
		inject func(*harness.Proxy)
		lo, hi time.Duration // the range the total falls in
		errMsg []string      // the error holds at least one of them
	}{
		// Connections refused: all three attempts fail at once and only two backoffs are left (resty's backoff is never under RetryWaitTime)
		{"refuses connections", func(p *harness.Proxy) { p.Cut() }, retries * faultHTTPRetryWaitTime, faultHTTPWorstCase(retries), []string{"connection refused"}},
		// The host is down: every attempt waits out Timeout. Stuck on the dial, http.Client's timeout is reported as context deadline exceeded,
		// and stuck waiting for the response headers as Client.Timeout exceeded, both of which are Timeout arriving
		{"is down", func(p *harness.Proxy) { p.Blackhole() }, (retries + 1) * faultHTTPTimeout, faultHTTPWorstCase(retries),
			[]string{"context deadline exceeded", "Client.Timeout exceeded"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stub := harness.NewStub(t)
			px := harness.NewProxy(t, stub.Addr())
			p := faultStartDownstream(t, "http://"+px.Addr(), fmt.Sprintf("  RetryCount: %d\n", retries))
			if ok := faultCallDownstream(t, p, "warmup", ""); ok.resp.Status != http.StatusOK {
				t.Fatalf("/proxy should answer 200 before the fault, got %v", ok.resp)
			}
			c.inject(px)

			token := "t-" + harness.NewID()
			call := faultCallDownstream(t, p, token, "")
			found := false
			for _, m := range c.errMsg {
				found = found || strings.Contains(string(call.resp.Body), m)
			}
			if call.resp.Status != http.StatusBadGateway || !found {
				t.Errorf("while the downstream %s it should answer 502 with one of %q in the error, got %v", c.name, c.errMsg, call.resp)
			}
			if call.took < c.lo-20*time.Millisecond || call.took > c.hi+faultSlack {
				t.Errorf("while the downstream %s with RetryCount=%d, the total should be between %v and %v (the documented worst case), got %v", c.name, retries, c.lo, c.hi, call.took)
			}
			warn, errs, _ := faultWaitRestyLogs(t, p, token)
			if warn != retries+1 || errs != 1 {
				t.Errorf("all %d attempts failed, which should be %d WARN + 1 ERROR, got %d WARN + %d ERROR", retries+1, retries+1, warn, errs)
			}
			if stub.Count() != 1 {
				t.Errorf("no connection reaches the stub during the fault, so it should hold the 1 request from before it, got %d", stub.Count())
			}
			t.Logf("numbers: the downstream %s with Timeout=%v and RetryCount=%d: 502 after %s, and resty recorded %d WARN + %d ERROR", c.name, faultHTTPTimeout, retries, faultMS(call.took), warn, errs)
		})
	}
}
