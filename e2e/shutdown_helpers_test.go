package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// This file is what shutdown_test.go leans on: taking the hooks' start and stop order out of the
// output, traffic held with a timeline, and a probe of new connections after the signal.

// stopHookPkgs are the packages that registered a stop hook (measured from the stopping log lines
// of one clean exit of the e2e service).
// The cases interrupted during the start work out "which ones should be closed" from it; when the
// framework gives another package a stop hook, the shutdown order case reports it first and it is
// added here then
var stopHookPkgs = []string{"xlog", "xtrace", "xcache", "xgorm", "xredis", "xhttp", "xkafka", "xcron"}

// stopPairs says which start hook a stop hook pairs with where a package registered more than one
// pair (docs/guide.md, "A stop hook: pairing and inheriting": the one registered most recently
// before it in the same package). One xkafka pair is the producing client (StageClient) and the
// other the consumers (StageServer).
// A package with one pair alone pairs by package name
var stopPairs = map[string]string{
	"xkafka.closeXKafka":   "xkafka.initXKafka",
	"xkafka.stopConsumers": "xkafka.startConsumers",
}

// startIndex is where the start hook paired with the stop hook stop sits in starts, and ok is false when it never started
func startIndex(starts []string, stop string) (int, bool) {
	for i := len(starts) - 1; i >= 0; i-- {
		if want, ok := stopPairs[stop]; ok && starts[i] == want || !ok && pkgOf(starts[i]) == pkgOf(stop) {
			return i, true
		}
	}
	return 0, false
}

// textHook: before xlog is installed, the framework logs are text in slog's default format on stderr:
//
//	2026/09/24 01:59:17 INFO starting hook=xapp.loadConfig
var textHook = regexp.MustCompile(`\bINFO (starting|stopping) hook=(\S+)`)

// hookSeq takes the hook names out of the msg (starting / stopping) log lines in the order they
// happened.
//
// Not ordered by the arrival order of the two pipes: stdout and stderr are each read by a
// goroutine of their own and which arrives first is not guaranteed.
// They are put together in logical order: the text lines in stderr are certainly the earliest
// (xlog is installed right after them), then the JSON in stdout (while xlog is in force) and
// finally the JSON in stderr (after xlog is closed)
func hookSeq(p *harness.Process, msg string) []string {
	var out []string
	for _, line := range strings.Split(p.Stderr(), "\n") {
		if m := textHook.FindStringSubmatch(line); m != nil && m[1] == msg {
			out = append(out, m[2])
		}
	}
	logs := p.Logs()
	for _, stream := range []string{"stdout", "stderr"} {
		for _, l := range logs {
			if l.Stream == stream && l.Msg() == msg {
				out = append(out, l.Str("hook"))
			}
		}
	}
	return out
}

// pkgOf is a hook name's package: xgorm.initXGorm -> xgorm
func pkgOf(hook string) string {
	pkg, _, _ := strings.Cut(hook, ".")
	return pkg
}

// expectedStops is the stop order the documentation implies: of the hooks that started
// successfully, the ones that have a stop hook, reversed
func expectedStops(started []string) []string {
	var out []string
	for i := len(started) - 1; i >= 0; i-- {
		if slices.Contains(stopHookPkgs, pkgOf(started[i])) {
			out = append(out, pkgOf(started[i]))
		}
	}
	return out
}

func pkgsOf(hooks []string) []string {
	out := make([]string, len(hooks))
	for i, h := range hooks {
		out[i] = pkgOf(h)
	}
	return out
}

// waitGauge polls /metrics until name reaches want. It makes sure a request has reached the handler before a signal is sent
func waitGauge(t *testing.T, p *harness.Process, name string, want float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got float64
	for time.Now().Before(deadline) {
		got = p.Metrics(t).Sum(name)
		if got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not reach %v within %v (the last reading was %v): the request never reached the handler, and the assertions after this mean nothing", name, want, timeout, got)
}

// reqShot is one request's timeline
type reqShot struct {
	stream     string
	start, end time.Time
	status     int // the status code when a response arrived
	err        error
}

func (s reqShot) latency() time.Duration { return s.end.Sub(s.start) }

func (s reqShot) String() string {
	if s.err != nil {
		return fmt.Sprintf("%s err=%v (%v)", s.stream, s.err, s.latency().Round(time.Microsecond))
	}
	return fmt.Sprintf("%s status=%d (%v)", s.stream, s.status, s.latency().Round(time.Microsecond))
}

// lane is one stream of traffic at a fixed concurrency: conc workers, each with one long-lived connection, sending one request after another
type lane struct {
	name string
	conc int
	req  func(i int) (*http.Request, error)
}

// getReq is a GET that is the same for every request
func getReq(url string) func(int) (*http.Request, error) {
	return func(int) (*http.Request, error) { return http.NewRequest(http.MethodGet, url, nil) }
}

// timedTraffic keeps sending requests until stop and records when each one started and ended.
//
// What sets it apart from harness.Load is the timeline: the graceful exit assertions group the
// requests by "sent before or after the signal", which a total and some percentiles cannot tell
type timedTraffic struct {
	stopped atomic.Bool
	wg      sync.WaitGroup
	mu      sync.Mutex
	shots   []reqShot
	trs     []*http.Transport
}

func startTraffic(lanes ...lane) *timedTraffic {
	tr := &timedTraffic{}
	for _, s := range lanes {
		transport := &http.Transport{MaxIdleConns: s.conc, MaxIdleConnsPerHost: s.conc, DisableCompression: true}
		tr.trs = append(tr.trs, transport)
		// The client's 10s timeout is only a backstop: a request that hangs ends in a timeout, and the latency is how long it hung
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		var next atomic.Int64
		for range s.conc {
			tr.wg.Add(1)
			go func() {
				defer tr.wg.Done()
				var mine []reqShot
				for !tr.stopped.Load() {
					sh := reqShot{stream: s.name, start: time.Now()}
					req, err := s.req(int(next.Add(1)))
					if err == nil {
						var resp *http.Response
						if resp, err = client.Do(req); err == nil {
							_, err = io.Copy(io.Discard, resp.Body)
							resp.Body.Close()
							sh.status = resp.StatusCode
						}
					}
					sh.end, sh.err = time.Now(), err
					mine = append(mine, sh)
					if err != nil {
						time.Sleep(2 * time.Millisecond) // do not spin once the port is closed
					}
				}
				tr.mu.Lock()
				tr.shots = append(tr.shots, mine...)
				tr.mu.Unlock()
			}()
		}
	}
	return tr
}

// stop stops sending new requests, waits for the ones in flight and returns every request sorted by when it was sent
func (tr *timedTraffic) stop() []reqShot {
	tr.stopped.Store(true)
	tr.wg.Wait()
	for _, t := range tr.trs {
		t.CloseIdleConnections()
	}
	sort.Slice(tr.shots, func(i, j int) bool { return tr.shots[i].start.Before(tr.shots[j].start) })
	return tr.shots
}

// connProbe is one probe of a new connection
type connProbe struct {
	at      time.Time
	latency time.Duration
	status  int
	err     error
}

func (p connProbe) refused() bool { return errors.Is(p.err, syscall.ECONNREFUSED) }

// probeNewConns GETs url with a new connection (never reused) every every, until ctx ends.
// What it looks at is what "a client arriving after the signal" meets: refused, an error, or hanging
func probeNewConns(ctx context.Context, url string, every time.Duration) <-chan []connProbe {
	out := make(chan []connProbe, 1)
	go func() {
		transport := &http.Transport{DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		var probes []connProbe
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			pr := connProbe{at: time.Now()}
			resp, err := client.Get(url)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				pr.status = resp.StatusCode
			}
			pr.latency, pr.err = time.Since(pr.at), err
			probes = append(probes, pr)
			select {
			case <-ctx.Done():
				out <- probes
				return
			case <-tick.C:
			}
		}
	}()
	return out
}

// afterCloseErrors are the traces of "something still used a resource after it was closed" in the
// output: the "closed" errors of database/sql and go-redis, and the wording of xclient's "called
// too late" panic
var afterCloseErrors = []string{
	"sql: database is closed",
	"redis: client is closed",
	"was requested after",
}

// fmtMS renders a duration in milliseconds, for the logs and the reports
func fmtMS(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// later is the later of two moments
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// firstN is the first n, for printing
func firstN[T any](s []T, n int) []T { return s[:min(n, len(s))] }

// grepLines is the first n lines holding pat
func grepLines(text, pat string, n int) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, pat) {
			out = append(out, line)
			if len(out) == n {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// lastLines is the last n lines
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// errKind is what is left of a transport error once the request URL is taken out (the port differs
// every time), for grouping in the printout:
// Get "http://127.0.0.1:41234/slow?ms=300": dial tcp ...: connection refused -> dial tcp ...: connection refused
func errKind(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\": "); i >= 0 {
		s = s[i+3:]
	}
	return s
}
