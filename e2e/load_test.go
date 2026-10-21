package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The load tests. They run with XONE_E2E_LOAD=1 alone (scripts/e2e.sh --load), and one round takes
// a dozen minutes.
//
// The load generator, the service under test and PostgreSQL share one machine (4 cores here) and
// fight over the CPU under high concurrency: the QPS and the latencies here can only be compared
// between configurations and are not the absolute performance of a deployment of its own.
// "service CPU us/request" is the least affected by that contention and suits the framework's
// overhead better than the QPS.
//
// The switches (all durations):
//
//	XONE_E2E_LOAD_STEP    how long each step is measured, 5s by default
//	XONE_E2E_LOAD_WARMUP  how long each step warms up first, 1s by default
//	XONE_E2E_SOAK         how long the soak runs, 3m by default

// Bare gin as the control and the xone service in three configurations as the subject, over the two
// endpoints /ping and /users/:id (reading PG), at concurrency 1 / 16 / 64 / 256.
//
// What is asserted are the promises that have to hold under load, and the numbers themselves are
// only printed:
//   - not one request fails (a transport error and anything that is not 2xx both count)
//   - with the metrics on, xgin's request count and duration histogram are exact (xgin/README.md,
//     "The metrics")
//   - with everything on there is one access log line per request, and every line written to the
//     file is whole JSON (the concurrent writes are not torn)
//   - after the round a SIGTERM still exits with 0
func TestLoad_FrameworkOverheadVsBareGinByConcurrency(t *testing.T) {
	requireLoad(t)
	table := "e2e_load_" + harness.NewID()
	ids := seedUsers(t, table, 1000)
	eps := loadEndpoints(ids)
	concs := []int{1, 16, 64, 256}

	targets := []struct {
		name     string
		baseline bool
		m        mw
	}{
		{"bare gin", true, mw{}},
		{"xone everything off", false, mwAllOff},
		{"xone defaults", false, mwDefault},
		{"xone everything on", false, mwAllOn},
	}

	var cells []cell
	for _, tg := range targets {
		var p *harness.Process
		if tg.baseline {
			p = harness.StartBaseline(t, harness.Options{Table: table, DiscardStdout: true})
		} else {
			p = harness.Start(t, tg.m.options(table))
		}
		for _, ep := range eps {
			for _, conc := range concs {
				var before harness.Metrics
				if tg.m.metric {
					before = p.Metrics(t)
				}
				c := measure(t, p, tg.name, ep, tg.baseline, conc)
				if tg.m.metric {
					checkRequestMetric(t, c, ep.route, before, p.Metrics(t))
				}
				cells = append(cells, c)
			}
		}

		if tg.m.export {
			// Printed and not asserted: whether a span is dropped depends on the SpanProcessor the caller hung on (here
			// a BatchSpanProcessor with the SDK's default settings, which drops silently when its queue is full) and is
			// not something the framework promises. The sampling ratio of 1 is kept by the denominator of the reconciliation:
			// every request had a server span that reached the processor
			for _, ep := range eps {
				exported, handled := waitSpansFlushed(t, p, ep.route)
				t.Logf("%s %s: %.0f server spans exported / %.0f requests handled (%.2f%%)", tg.name, ep.route, exported, handled, 100*exported/handled)
			}
		}
		var final harness.Metrics
		if tg.m.metric {
			final = p.Metrics(t)
		}

		exit := p.Terminate(t, 30*time.Second)
		if exit.Code != 0 || exit.Signal != nil {
			t.Errorf("%s should exit with 0 on the SIGTERM after the round, got %v\n%s", tg.name, exit, p.Stderr())
		}

		if tg.m.file {
			byRoute, bad, size := accessLogStats(t, p.Dir)
			lines := 0
			for _, ep := range eps {
				want := int(final.Sum("e2e_http_requests_total", "route", ep.route))
				if byRoute[ep.route] != want {
					t.Errorf("the documentation says one access log line per request; %s %s handled %d requests and the log file holds only %d", tg.name, ep.route, want, byRoute[ep.route])
				}
				lines += byRoute[ep.route]
			}
			if bad > 0 {
				t.Errorf("%d lines of the log file are not whole JSON: the concurrent writes were torn (xlog/rotate.go says Write is under a lock)", bad)
			}
			t.Logf("%s log files %.0fMB, %d access log lines, %d bytes each on average", tg.name, mb(size), lines, size/int64(max(lines, 1)))
		}
	}

	t.Logf("The load results (%v warmup and %v measured per step; the load generator shares the machine with the service, so this is a relative comparison):\n%s", loadWarmup(t), loadStep(t), cellTable(cells))
	t.Logf("Against bare gin:\n%s", overheadTable(cells, "bare gin"))
}

// overheadTable is each subject's difference from the control: the QPS ratio, the extra CPU per request, and the extra p50 / p99 latency
func overheadTable(cells []cell, base string) string {
	find := func(target, ep string, conc int) (cell, bool) {
		for _, c := range cells {
			if c.target == target && c.endpoint == ep && c.conc == conc {
				return c, true
			}
		}
		return cell{}, false
	}
	var b strings.Builder
	b.WriteString("| endpoint | concurrency | target | QPS ratio | service CPU us/request | extra CPU us/request | extra % | extra p50 ms | extra p99 ms |\n")
	b.WriteString("|---|---:|---|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		if c.target == base {
			continue
		}
		o, ok := find(base, c.endpoint, c.conc)
		if !ok || o.res.QPS == 0 || o.cpu == 0 {
			continue
		}
		d := c.cpuUS() - o.cpuUS()
		fmt.Fprintf(&b, "| %s | %d | %s | %.2f | %.1f | %+.1f | %+.0f%% | %+.2f | %+.2f |\n",
			c.endpoint, c.conc, c.target, c.res.QPS/o.res.QPS, c.cpuUS(), d, 100*d/o.cpuUS(),
			float64(c.res.P50-o.res.P50)/float64(time.Millisecond), float64(c.res.P99-o.res.P99)/float64(time.Millisecond))
	}
	return b.String()
}

// Starting from "everything off" and turning on one middleware at a time, under concurrency 16 over
// both endpoints, to see how much CPU each one adds per request.
//
// "the access log alone (level warn)" is about the promise of xgin/middleware/log.go: with the
// level turned off it passes straight through and does not buffer the request body, capture the
// response, redact or serialize -- so its cost should be close to zero, far under what level info
// costs
func TestLoad_EnableMiddlewaresOneByOneToFindCost(t *testing.T) {
	requireLoad(t)
	table := "e2e_load_" + harness.NewID()
	ids := seedUsers(t, table, 1000)
	eps := loadEndpoints(ids)
	const conc = 16

	variants := []struct {
		name string
		m    mw
	}{
		{"everything off", mwAllOff},
		{"the access log alone", mw{log: true}},
		{"the access log alone (level warn)", mw{log: true, level: "warn"}},
		{"the access log alone (bodies included)", mw{log: true, body: true}},
		{"tracing alone", mw{trace: true}},
		// SampleRatio 0: a TraceID is still generated and handed on (trace_id in the logs and X-Trace-Id in the response headers are both there),
		// only the span is not recorded. The difference from the line above is what "recorded with no exporter to take it" costs
		{"tracing alone (SampleRatio 0)", mw{trace: true, sample: "0"}},
		{"tracing alone plus the batch export", mw{trace: true, export: true}},
		{"the metrics alone", mw{metric: true}},
		{"the defaults (all three on)", mwDefault},
		{"everything on", mwAllOn},
	}

	var cells []cell
	for _, v := range variants {
		p := harness.Start(t, v.m.options(table))
		for _, ep := range eps {
			cells = append(cells, measure(t, p, v.name, ep, false, conc))
		}
		p.Terminate(t, 30*time.Second)
	}

	cpu := func(target, ep string) float64 {
		for _, c := range cells {
			if c.target == target && c.endpoint == ep {
				return c.cpuUS()
			}
		}
		t.Fatalf("there is no %s %s step", target, ep)
		return 0
	}

	var b strings.Builder
	b.WriteString("| configuration | endpoint | QPS | p50 ms | p99 ms | service CPU us/request | against everything off us/request |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		fmt.Fprintf(&b, "| %s | %s | %.0f | %s | %s | %.1f | %+.1f |\n",
			c.target, c.endpoint, c.res.QPS, msf(c.res.P50), msf(c.res.P99), c.cpuUS(), c.cpuUS()-cpu("everything off", c.endpoint))
	}
	for _, ep := range eps {
		off := cpu("everything off", ep.name)
		sum := cpu("the access log alone", ep.name) + cpu("tracing alone", ep.name) + cpu("the metrics alone", ep.name) - 3*off
		fmt.Fprintf(&b, "\n%s: the three increments added up are %+.1fus, and all three on together measured %+.1fus", ep.name, sum, cpu("the defaults (all three on)", ep.name)-off)
	}
	t.Logf("The middleware turned on one at a time (concurrency %d, %v warmup and %v measured per step):\n%s", conc, loadWarmup(t), loadStep(t), b.String())

	// With the level turned off it passes straight through: the increment stays under half of what level info costs.
	//
	// Asserted on /ping alone: every /users/:id request also waits for PG, whose backend processes fight over the same cores
	// as the service under test, so the per-request CPU of one configuration differs by 5us between three rounds (82.5 to 87.6us with everything off),
	// and level warn's increment measured anywhere from 0 to 8.2us, the same order as the access log's own increment (12.7 to 18.0us).
	// On /ping, three rounds measured level info at 5.5 to 7.2us and level warn at 0.8 to 1.6us (a ratio under 0.22),
	// so the half line leaves plenty of room
	off := cpu("everything off", "/ping")
	info, warn := cpu("the access log alone", "/ping")-off, cpu("the access log alone (level warn)", "/ping")-off
	if warn > info/2 {
		t.Errorf("xgin/middleware/log.go says the access log middleware passes straight through when the level is off; on /ping level info costs %.1fus/request more and level warn still costs %.1fus/request more",
			info, warn)
	}
}

// The defaults at concurrency 64, a read-write mix held for a few minutes, sampling the RSS, the
// goroutine count, the heap and the fds every 10 seconds.
//
// A leak looks like "it grows with the number of requests": one goroutine leaked per request is
// millions in a few minutes, and one leaked per connection stays flat while the load holds the
// connection count fixed -- so whether it falls back to the level before the load matters too
func TestLoad_DefaultConfig64ConcurrencySoakNoLeak(t *testing.T) {
	requireLoad(t)
	soak := loadKnob(t, "XONE_E2E_SOAK", 3*time.Minute)
	const (
		conc     = 64
		interval = 10 * time.Second
	)
	table := "e2e_load_" + harness.NewID()
	ids := seedUsers(t, table, 1000)
	p := harness.Start(t, mwDefault.options(table))

	// A read-write mix: half read through the three cache levels, two tenths read the database directly, two tenths /ping
	// and one tenth updates a user (dropping both cache levels, so the next read reaches the database again); PG, Redis, the local cache, the access log, the tracing and the metrics are all turning
	newReq := func(ctx context.Context, i int) (*http.Request, error) {
		// Ten requests in a row use the same id: spread by i, i%10 would fix both the request kind and the id's remainder,
		// so the tenth that updates and the tenths that read would never meet and dropping the cache would test nothing
		id := ids[(i/10*7919)%len(ids)]
		switch i % 10 {
		case 0, 1, 2, 3, 4:
			return http.NewRequestWithContext(ctx, http.MethodGet, p.URL(fmt.Sprintf("/users/%d", id)), nil)
		case 5, 6:
			return http.NewRequestWithContext(ctx, http.MethodGet, p.URL(fmt.Sprintf("/users/%d?cache=off", id)), nil)
		case 7, 8:
			return http.NewRequestWithContext(ctx, http.MethodGet, p.URL("/ping"), nil)
		default:
			body := fmt.Sprintf(`{"name":"soak-%d","email":"soak-%d@example.com"}`, i, i)
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.URL(fmt.Sprintf("/users/%d", id)), strings.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
			}
			return req, err
		}
	}

	// Warm up for 10 seconds before recording the idle level: the goroutine count after the pools, the caches and the sync.Pools are built is the baseline
	warm := harness.Load(t, harness.LoadSpec{Concurrency: conc, Duration: 10 * time.Second, NewRequest: newReq})
	if warm.Errors > 0 || warm.OK() != warm.Requests {
		t.Fatalf("there were failures in the warmup already: %v %v", warm, warm.ErrorSamples)
	}
	idle := waitIdle(t, p, 0)
	t.Logf("idle after the warmup: %s", idle)

	samples := make(chan soakSample, 64)
	stop := make(chan struct{})
	go func() {
		defer close(samples)
		start := time.Now()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				samples <- sampleSoak(p, time.Since(start))
			}
		}
	}()
	res := harness.Load(t, harness.LoadSpec{Concurrency: conc, Duration: soak, NewRequest: newReq})
	close(stop)
	var got []soakSample
	for s := range samples {
		got = append(got, s)
	}
	after := waitIdle(t, p, idle.goroutines)

	var b strings.Builder
	b.WriteString("| moment | requests so far | QPS over these 10s | RSS MB | goroutines | heap_inuse MB | fds | threads |\n")
	b.WriteString("|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(&b, "| idle (before) | %.0f | | %.1f | %.0f | %.1f | %.0f | %d |\n", idle.requests, mb(idle.rss), idle.goroutines, mb(int64(idle.heap)), idle.fds, idle.threads)
	prev := idle.requests
	for _, s := range got {
		if s.err != "" {
			t.Errorf("the sample at %v failed: %s", s.at, s.err)
			continue
		}
		fmt.Fprintf(&b, "| %v | %.0f | %.0f | %.1f | %.0f | %.1f | %.0f | %d |\n",
			s.at.Round(time.Second), s.requests, (s.requests-prev)/interval.Seconds(), mb(s.rss), s.goroutines, mb(int64(s.heap)), s.fds, s.threads)
		prev = s.requests
	}
	fmt.Fprintf(&b, "| idle (after) | %.0f | | %.1f | %.0f | %.1f | %.0f | %d |\n", after.requests, mb(after.rss), after.goroutines, mb(int64(after.heap)), after.fds, after.threads)
	t.Logf("The soak over %v (the defaults, concurrency %d): %v\n%s", soak, conc, res, b.String())

	if res.Errors > 0 || res.OK() != res.Requests {
		t.Errorf("not one request should fail during the soak: of %d, %d failed in the transport and %d were not 2xx, with the status codes %v and the error samples %v",
			res.Requests, res.Errors, res.Requests-res.Errors-res.OK(), res.Status, res.ErrorSamples)
	}
	checkSoak(t, conc, idle, after, got)

	exit := p.Terminate(t, 30*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("it should exit with 0 on the SIGTERM after the soak, got %v\n%s", exit, p.Stderr())
	}
}

// soakSample is one sample taken during the soak
type soakSample struct {
	at                    time.Duration
	requests              float64 // the sum of xgin's request counts, /metrics itself taken out
	rss                   int64
	threads               int
	goroutines, heap, fds float64
	err                   string
}

func (s soakSample) String() string {
	return fmt.Sprintf("%.0f goroutines, RSS %.1fMB, heap_inuse %.1fMB, %.0f fds, %d threads", s.goroutines, mb(s.rss), mb(int64(s.heap)), s.fds, s.threads)
}

// sampleClient is the client the sampling uses: the sampling does not hang when the service does
var sampleClient = &http.Client{Timeout: 5 * time.Second}

// sampleSoak takes one sample. It runs on a goroutine other than the load's, so it takes no t: an error goes into err
func sampleSoak(p *harness.Process, at time.Duration) soakSample {
	s := soakSample{at: at}
	st, err := harness.ReadProc(p.Pid())
	if err != nil {
		s.err = err.Error()
		return s
	}
	s.rss, s.threads = st.RSS, st.Threads
	resp, err := sampleClient.Get(p.URL("/metrics"))
	if err != nil {
		s.err = err.Error()
		return s
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		s.err = err.Error()
		return s
	}
	m, err := harness.ParseMetrics(bytes.NewReader(body))
	if err != nil {
		s.err = err.Error()
		return s
	}
	s.goroutines = m.Sum("go_goroutines")
	s.heap = m.Sum("go_memstats_heap_inuse_bytes")
	s.fds = m.Sum("process_open_fds")
	s.requests = m.Sum("e2e_http_requests_total") - m.Sum("e2e_http_requests_total", "route", "/metrics")
	return s
}

// waitIdle waits for the goroutine count to fall back once the load has stopped: after the load
// generator closes its connections, the server's goroutine for each one exits when it reads EOF.
// It waits until it is at most want + idleSlack (and with want 0, only until it stops changing
// twice in a row), 15 seconds at most
func waitIdle(t *testing.T, p *harness.Process, want float64) soakSample {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last soakSample
	for {
		time.Sleep(time.Second)
		s := sampleSoak(p, 0)
		if s.err != "" {
			t.Fatalf("the sample failed: %s", s.err)
		}
		settled := want == 0 && last.goroutines == s.goroutines && last.fds == s.fds
		if settled || (want > 0 && s.goroutines <= want+idleSlack) || time.Now().After(deadline) {
			return s
		}
		last = s
	}
}

// idleSlack is the slack (in counts) the criteria below share. Over three soaks, the goroutine
// count after the load was exactly the one before it (13 both times), and the 10 is for the
// background goroutines that come and go
const idleSlack = 10

// checkSoak judges whether anything leaked. A leak takes two shapes:
//
//   - growing with the requests: a few minutes of soaking is over two million requests, so one
//     leaked per hundred thousand is already two dozen.
//     What is checked is whether the goroutines fall back to within idleSlack of the level before
//     the load
//   - growing with the connections: the load holds the connection count fixed at conc, so it cannot
//     grow during the load, and what is checked is whether the fds those conc connections held are
//     given back afterwards: the fds after the load <= the fds at the end of the soak - conc +
//     idleSlack.
//     Not compared against the level before the load: the PG / Redis pools grow by a few more
//     connections mid-soak (measured at 96 before and 99 after), which are bounded and are no leak
//
// A few bounds hold during the load as well, against "it grows until it drags the process down":
//
//   - goroutines <= before the load + 3 x conc: one serve goroutine per connection, plus the
//     background read net/http starts while a handler runs (startBackgroundRead in server.go), and
//     besides them only the ones that flash by around a request (pgx's background read, a timer
//     callback; pgx v5.10 watches ctx with context.AfterFunc, which starts no goroutine).
//     Three rounds measured 64 to 135 above the level before the load, which is 2.1 per connection
//     at most, so the bound is 3 per connection
//   - in the steady stretch (after a minute of soaking, with the pools, the sync.Pools and
//     ristretto's buffers all spread out) the highest RSS of the second half stays under 1.15 times
//     the highest of the first half plus 8MB. Three rounds measured the RSS at 56.6 to 61.5MB,
//     which does not swing by 3MB within one round
//   - heap_inuse goes by the lowest of each half, which is the "floor" right after a GC: which
//     moment of the GC cycle a sample lands on is random, and the highest bounced between 18 and
//     26MB while the floor stayed at 18.3 to 19.6MB.
//     The second half's floor stays under 1.25 times the first half's plus 4MB
func checkSoak(t *testing.T, conc int, idle, after soakSample, got []soakSample) {
	t.Helper()
	var ok, steady []soakSample
	for _, s := range got {
		if s.err != "" {
			continue
		}
		ok = append(ok, s)
		if s.at >= time.Minute {
			steady = append(steady, s)
		}
	}
	if len(steady) < 4 {
		t.Fatalf("the steady stretch sampled only %d points, which cannot tell whether anything leaked (give XONE_E2E_SOAK 2m at least)", len(steady))
	}

	if after.goroutines > idle.goroutines+idleSlack {
		t.Errorf("the goroutines did not fall back after the load: %.0f idle before, %.0f idle after", idle.goroutines, after.goroutines)
	}
	if last := ok[len(ok)-1]; after.fds > last.fds-float64(conc)+idleSlack {
		t.Errorf("the fds of the %d connections were not given back after the load: %.0f at the end of the soak, %.0f idle after", conc, last.fds, after.fds)
	}
	limit := idle.goroutines + float64(3*conc)
	for _, s := range ok {
		if s.goroutines > limit {
			t.Errorf("at %v there were %.0f goroutines, over the bound of %.0f before the load plus 3 per connection, %.0f", s.at, s.goroutines, idle.goroutines, limit)
		}
	}

	half := len(steady) / 2
	fold := func(ss []soakSample, f func(soakSample) float64, pick func(a, b float64) float64) float64 {
		m := f(ss[0])
		for _, s := range ss[1:] {
			m = pick(m, f(s))
		}
		return m
	}
	hi := func(a, b float64) float64 { return max(a, b) }
	lo := func(a, b float64) float64 { return min(a, b) }
	rss := func(s soakSample) float64 { return float64(s.rss) }
	heap := func(s soakSample) float64 { return s.heap }
	if a, b := fold(steady[:half], rss, hi), fold(steady[half:], rss, hi); b > a*1.15+8<<20 {
		t.Errorf("the RSS is still growing in the steady stretch: %.1fMB highest in the first half, %.1fMB in the second", a/(1<<20), b/(1<<20))
	}
	if a, b := fold(steady[:half], heap, lo), fold(steady[half:], heap, lo); b > a*1.25+4<<20 {
		t.Errorf("heap_inuse's floor is still growing in the steady stretch: %.1fMB lowest in the first half, %.1fMB in the second", a/(1<<20), b/(1<<20))
	}
}
