package e2e

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The graceful exit family: real processes, real signals, a real PG / Redis.
// The promises they follow come from docs/architecture.md, "Exit signals: taken over from the
// process's first millisecond" and "The stop budget is one", from the shutdown notes of XGin in
// xgin/README.md, and from xone.WithStopTimeout's comment.

// TestShutdown_SIGTERMUnderLoad_FinishesInFlight_RefusesNew_ExitsZero
//
// What the documentation promises (docs/architecture.md, "The stop budget is one", and the XGin
// section of xgin/README.md):
//   - Stop waits for the requests in flight, up to the service's share of the budget
//     (two thirds of WithStopTimeout);
//   - the databases and the cache are closed only once the service's share (Stop plus waiting for
//     Start to return) is over, so a request in flight never meets a pool that is already closed;
//   - the whole shutdown is over within WithStopTimeout, and a service exiting as asked is 0.
//
// The requests in flight are waited for by xgin.Stop itself (which waits for the handlers to
// return after the Shutdown): xgin's Start returns as soon as the Shutdown begins, so this case
// cannot test Run's half, "waiting for Start to return" -- that half is
// TestShutdown_StartReturnsBeforeDBAndCacheClose.
//
// The traffic: /slow?ms=300 x16 (slow requests that watch ctx), GET /users/:id?cache=off x4
// (reading PG), PUT /users/:id x2 (writing PG and dropping Redis) and /stuck x4 (sleeping 300ms
// without watching ctx and then querying the database and Redis).
// That last lane is the most sensitive probe of "a resource was closed too early": it touches
// PG / Redis 300ms after the signal, and were the framework to close them without waiting for the
// handler to return, its log would carry a db_error / redis_error other than none.
func TestShutdown_SIGTERMUnderLoad_FinishesInFlight_RefusesNew_ExitsZero(t *testing.T) {
	harness.Require(t)
	const budget = 5 * time.Second
	p := harness.Start(t, harness.Options{Overlay: stopTimeout(budget)})

	var ids []int64
	for i := range 20 {
		ids = append(ids, createUser(t, p, fmt.Sprintf("u%d", i), fmt.Sprintf("u%d@example.com", i)).ID)
	}
	tr := startTraffic(
		lane{name: "slow", conc: 16, req: getReq(p.URL("/slow?ms=300"))},
		lane{name: "pg-read", conc: 4, req: func(i int) (*http.Request, error) {
			return http.NewRequest(http.MethodGet, p.URL(fmt.Sprintf("/users/%d?cache=off", ids[i%len(ids)])), nil)
		}},
		lane{name: "pg-write", conc: 2, req: func(i int) (*http.Request, error) {
			body := fmt.Sprintf(`{"name":"w%d","email":"w%d@example.com"}`, i, i)
			req, err := http.NewRequest(http.MethodPut, p.URL(fmt.Sprintf("/users/%d", ids[i%len(ids)])), strings.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
			}
			return req, err
		}},
		lane{name: "stuck-db", conc: 4, req: getReq(p.URL("/stuck?ms=300&db=1&redis=1"))},
	)
	// Run at full tilt for a second first: every lane has requests in flight, and the pools and caches are warm
	time.Sleep(time.Second)
	gauges := p.Metrics(t)
	slowInflight, stuckInflight := gauges.Sum("e2e_slow_inflight"), gauges.Sum("e2e_stuck_inflight")

	sigAt := time.Now()
	p.Signal(syscall.SIGTERM)
	probeCtx, stopProbe := context.WithCancel(context.Background())
	probesCh := probeNewConns(probeCtx, p.URL("/ping"), 5*time.Millisecond)

	exit, ok := p.Wait(budget + 5*time.Second)
	exitAt := time.Now()
	stopProbe()
	probes := <-probesCh
	shots := tr.stop()
	if !ok {
		t.Fatalf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and the process is still alive %v after the SIGTERM was sent", budget, budget+5*time.Second)
	}

	// ---- The requests in flight at the moment of the signal: every one of them gets a 200 ----
	//
	// guard: the requests sent in the last stretch before the signal do not count as "already
	// started". net/http's Shutdown closes the idle keep-alive connections right away, and a request
	// the client has just written into such a connection that the server has not read yet never
	// started as far as the server is concerned -- the client (a GET being replayable) takes a new
	// connection and retries, meets the listener that is already closed, and gets a connection
	// refused. That is net/http's semantics and no promise of the framework, so this stretch only
	// has to "not hang", and the failures in it are printed separately. From the signal to Shutdown
	// closing the listener and the idle connections is milliseconds (the first refused probe lands
	// 5 to 6ms after the signal, with a probe every 5ms), and 20ms is several times that.
	// Measured, 150 to 170 requests go out in those 20ms per round and not one has ever failed --
	// the race is there, it is simply hard to hit
	const guard = 20 * time.Millisecond
	type laneStat struct{ pre, inflight, window, windowBad, post, postOK int }
	stats := map[string]*laneStat{}
	var preBad, postBad, hung []reqShot
	var postErrs []string
	var lastInflightEnd time.Time
	for _, s := range shots {
		st := stats[s.stream]
		if st == nil {
			st = &laneStat{}
			stats[s.stream] = st
		}
		ok := s.err == nil && s.status == http.StatusOK
		if s.start.Before(sigAt) && s.end.After(sigAt) {
			st.inflight++
			lastInflightEnd = later(lastInflightEnd, s.end)
		}
		switch {
		case s.start.Before(sigAt.Add(-guard)):
			st.pre++
			if !ok {
				preBad = append(preBad, s)
			}
		case s.start.Before(sigAt):
			st.window++
			if !ok {
				st.windowBad++
			}
		default:
			st.post++
			switch {
			case ok:
				st.postOK++
			case s.err == nil:
				// One accepted after the signal (in the instant before the port closes) has to finish as usual too
				postBad = append(postBad, s)
			case len(postErrs) < 3 && !slices.Contains(postErrs, errKind(s.err)):
				postErrs = append(postErrs, errKind(s.err))
			}
		}
		// The longest ordinary request is a 300ms slow / stuck. Anything over 1.5s can only have hung
		if s.end.After(sigAt) && s.latency() > 1500*time.Millisecond {
			hung = append(hung, s)
		}
	}
	for _, name := range []string{"slow", "pg-read", "pg-write", "stuck-db"} {
		st := stats[name]
		if st == nil {
			t.Fatalf("the %s lane sent not one request", name)
		}
		t.Logf("%-8s sent %5d before the signal, %2d in flight at it; %3d sent in the %v before it (%d failed); %4d sent after it (%d of them 200, the rest failing)",
			name, st.pre, st.inflight, st.window, guard, st.windowBad, st.post, st.postOK)
	}
	t.Logf("the metrics an instant before the signal: e2e_slow_inflight=%v e2e_stuck_inflight=%v; how the ones after it failed: %v", slowInflight, stuckInflight, postErrs)
	// Too few in flight and this case tests nothing. 16 / 4 workers sending 300ms requests back to back measured exactly 16 / 4 in flight
	if stats["slow"].inflight < 8 || stats["stuck-db"].inflight < 2 {
		t.Fatalf("too few requests were in flight at the signal (slow %d, stuck-db %d) to tell anything about the graceful exit", stats["slow"].inflight, stats["stuck-db"].inflight)
	}
	if len(preBad) > 0 {
		t.Errorf("the documentation says Stop waits for the requests in flight, and %d of those sent before the signal did not get a 200, the first few: %v", len(preBad), firstN(preBad, 5))
	}
	if len(postBad) > 0 {
		t.Errorf("a request accepted after the signal should still finish (200), and %d did not, the first few: %v", len(postBad), firstN(postBad, 5))
	}
	if len(hung) > 0 {
		t.Errorf("no request should hang after the signal, and %d took over 1.5s, the first few: %v", len(hung), firstN(hung, 5))
	}

	// ---- New connections: the port closes at once and they are refused rather than hanging ----
	firstRefused := -1
	var probeMax time.Duration
	for i, pr := range probes {
		probeMax = max(probeMax, pr.latency)
		if firstRefused < 0 && pr.refused() {
			firstRefused = i
		}
		if firstRefused >= 0 && pr.err == nil {
			t.Errorf("a new connection got a response after the port had closed (probe %d, status=%d)", i, pr.status)
		}
	}
	if firstRefused < 0 {
		t.Fatalf("not one of the %d new-connection probes after the signal was refused (connection refused): the port stayed open until the process exited", len(probes))
	}
	refusedAfter := probes[firstRefused].at.Sub(sigAt)
	t.Logf("%d new-connection probes: the first refusal came %s after the signal, with %d getting a response before it; the longest one took %s",
		len(probes), fmtMS(refusedAfter), firstRefused, fmtMS(probeMax))
	if probeMax > time.Second {
		t.Errorf("a new connection after the signal should be refused or fail quickly, and one probe waited %v", probeMax)
	}
	// The listener has to close right away and not along the way once the requests in flight are done and the process exits
	if !probes[firstRefused].at.Before(lastInflightEnd) {
		t.Errorf("the port should stop accepting new connections before the requests in flight are done: the first refusal came %s after the signal and the last request in flight finished %s after it",
			fmtMS(refusedAfter), fmtMS(lastInflightEnd.Sub(sigAt)))
	}

	// ---- The PG / Redis the requests in flight met were both still open ----
	for _, pat := range afterCloseErrors {
		if strings.Contains(p.Output(), pat) {
			t.Errorf("the documentation says the databases and the cache are closed only once the service has stopped cleanly, and the output holds %q:\n%s", pat, grepLines(p.Output(), pat, 5))
		}
	}
	stuckDone := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stuck request finished" })
	var stuckAfterSignal int
	for _, l := range stuckDone {
		if l.Str("db_error") != "none" || l.Str("redis_error") != "none" {
			t.Errorf("a /stuck in flight should reach PG / Redis after its sleep, got db_error=%q redis_error=%q", l.Str("db_error"), l.Str("redis_error"))
		}
		if ts, err := time.Parse(time.RFC3339Nano, l.Str("time")); err == nil && ts.After(sigAt) {
			stuckAfterSignal++
		}
	}
	if stuckAfterSignal == 0 {
		t.Errorf("not one /stuck finished after the signal: either the ones in flight were closed without being waited for, or their log lines never came out")
	}
	for _, l := range p.Logs() {
		if l.Level() == "ERROR" {
			t.Errorf("there should be no ERROR log line during a graceful exit, got: %s", l.Line)
		}
	}

	// ---- The exit code and the timings ----
	sinceSig := exit.SinceSignal
	// Broken out to see where the time went: the first stopping line means the service's share (Stop
	// plus waiting for Start to return) is over, and the last one is xlog's own
	if stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" }); len(stops) > 0 {
		first, _ := time.Parse(time.RFC3339Nano, stops[0].Str("time"))
		last, _ := time.Parse(time.RFC3339Nano, stops[len(stops)-1].Str("time"))
		t.Logf("the service's share ended %s after the signal (%s after the last request in flight), and the %d stop hooks took %s from the first one to xlog",
			fmtMS(first.Sub(sigAt)), fmtMS(first.Sub(lastInflightEnd)), len(stops), fmtMS(last.Sub(first)))
	}
	t.Logf("exit: %v; signal -> the last request in flight finished %s, -> the process exited %s (budget %v); %d /stuck finished after the signal",
		exit, fmtMS(lastInflightEnd.Sub(sigAt)), fmtMS(sinceSig), budget, stuckAfterSignal)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("it exited as asked with every request in flight finished, which the documentation says exits with 0, got %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 20))
	}
	if sinceSig > budget {
		t.Errorf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and it exited only %v after the signal", budget, sinceSig)
	}
	// A request in flight takes 300ms at most, and once they are done it should move on rather than
	// sitting out the budget.
	// Measured, 65 to 85ms pass from the last request in flight to the process exiting: 60 to 80ms of
	// it is net/http's Shutdown polling "are all the connections idle yet" (doubling from 1ms, capped
	// at 500ms, so at 200ms in one poll is a hundred milliseconds apart), and the 6 stop hooks take 4
	// to 7ms in all. 1s is given: really sitting out the budget would be over 3s here
	if tail := exitAt.Sub(lastInflightEnd); lastInflightEnd.IsZero() || tail > time.Second {
		t.Errorf("it should exit soon after the requests in flight are done, and the process exited %v after the last one finished", tail)
	}
}

// TestShutdown_StartReturnsBeforeDBAndCacheClose
//
// The documentation (docs/architecture.md, "The stop budget is one", and the comments of
// xone.Runnable and Run): the service's share is "Stop plus waiting for Start to return"; Stop
// returning does not mean the service has stopped cleanly, and the framework waits for Start to
// really return before it closes the rest, or work that is still running would meet a database and
// a cache that are already closed.
//
// The service is configured with Service.Drain: Start winds down for drain after xgin has returned
// (without watching ctx), then queries PG once and Pings Redis once and records a drain finished
// (see service/drain.go).
// xgin.Stop has no request in flight to wait for and returns right away -- and nothing is closed
// after that except through Run waiting for Start.
// Were the framework to close the components as soon as Stop returns, the wind-down would either
// meet a closed pool or never finish before the process exits
func TestShutdown_StartReturnsBeforeDBAndCacheClose(t *testing.T) {
	harness.Require(t)
	const (
		budget = 5 * time.Second
		drain  = 500 * time.Millisecond
	)
	p := harness.Start(t, harness.Options{Overlay: fmt.Sprintf("Service:\n  StopTimeout: %v\n  Drain: %v\n", budget, drain)})

	exit := p.Terminate(t, budget+5*time.Second)
	t.Logf("exit: %v (the wind-down %v, the budget %v)", exit, drain, budget)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("with the wind-down done and the components closed cleanly it should exit with 0, got %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 20))
	}

	drains := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "drain finished" })
	if len(drains) != 1 {
		t.Fatalf("the documentation says the framework waits for Start to really return before it closes the rest, and the process exited before Start's wind-down was done (%d drain finished lines), exiting %v after the signal\n%s",
			len(drains), exit.SinceSignal, lastLines(p.Output(), 20))
	}
	d := drains[0]
	if d.Str("db_error") != "none" || d.Str("mysql_error") != "none" || d.Str("redis_error") != "none" {
		t.Errorf("PG / MySQL / Redis should all still be open before Start returns, and the wind-down got db_error=%q mysql_error=%q redis_error=%q",
			d.Str("db_error"), d.Str("mysql_error"), d.Str("redis_error"))
	}
	drainAt, _ := time.Parse(time.RFC3339Nano, d.Str("time"))
	stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" })
	if len(stops) == 0 {
		t.Fatalf("there is not one stopping log line\n%s", lastLines(p.Output(), 30))
	}
	firstStop, _ := time.Parse(time.RFC3339Nano, stops[0].Str("time"))
	t.Logf("the wind-down finished %s before the first stop hook (%s)", fmtMS(firstStop.Sub(drainAt)), stops[0].Str("hook"))
	if !firstStop.After(drainAt) {
		t.Errorf("the stop hooks should begin only after Start returns, and %s began %s before the wind-down was done", stops[0].Str("hook"), fmtMS(drainAt.Sub(firstStop)))
	}
	if exit.SinceSignal < drain || exit.SinceSignal > budget {
		t.Errorf("it should exit after the wind-down (%v) and within the budget (%v), and it exited %v after the signal", drain, budget, exit.SinceSignal)
	}
}

// TestShutdown_CloseOrderIsReverseStartOrder_XLogLast
//
// The documentation: "Staged initialization, reverse-order shutdown"; and in docs/architecture.md,
// "The stop budget is one", "xlog, which comes last, always has time left to close its file".
// xlog closes last, which is what lets every component before it write its shutdown line -- so
// besides the order in stdout, the log file is checked too: every stopping line (xlog's own
// included) has to land in the file
func TestShutdown_CloseOrderIsReverseStartOrder_XLogLast(t *testing.T) {
	harness.Require(t)
	p := harness.Start(t, harness.Options{LogFile: true})
	createUser(t, p, "order", "order@example.com")

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("it should exit with 0 after the SIGTERM, got %v\n%s", exit, lastLines(p.Output(), 30))
	}

	starts, stops := hookSeq(p, "starting"), hookSeq(p, "stopping")
	t.Logf("the start order: %v", starts)
	t.Logf("the shutdown order: %v", stops)
	if len(stops) == 0 {
		t.Fatalf("there is not one stopping log line\n%s", lastLines(p.Output(), 30))
	}

	// The position of each stop hook's paired start hook in the start order has to decrease strictly
	prev := len(starts)
	for _, s := range stops {
		i, ok := startIndex(starts, s)
		if !ok {
			t.Errorf("the package of the stop hook %s never started", s)
			continue
		}
		if i >= prev {
			t.Errorf("the documentation says the shutdown is in reverse order, and %s closed before a component that started later: the shutdown order %v", s, stops)
		}
		prev = i
	}
	if want := expectedStops(starts); !slices.Equal(pkgsOf(stops), want) {
		t.Errorf("what closes should be exactly the ones that started and have a stop hook, in reverse: want %v, got %v"+
			" (when the framework gives another package a stop hook, add it to stopHookPkgs)", want, pkgsOf(stops))
	}
	if last := stops[len(stops)-1]; pkgOf(last) != "xlog" {
		t.Errorf("the documentation says xlog closes last, and the last one is %s", last)
	}

	// The log file has to hold every stopping line, xlog's own included: its file is still open when it closes
	var inFile []string
	for _, l := range p.FileLogs(t) {
		if l.Msg() == "stopping" {
			inFile = append(inFile, l.Str("hook"))
		}
	}
	if !slices.Equal(inFile, stops) {
		t.Errorf("xlog closes last, so every component before it should have its shutdown line in the file: stdout holds %v and the file holds %v", stops, inFile)
	}
}

// TestShutdown_CtxIgnoringHandlerInFlight_ReportsRunningCount_ExitsInBudget
//
// The documentation (the XGin section of xgin/README.md):
//  1. the Shutdown uses part of the time up to the deadline (leaving 20% of what is left, 1s at
//     most), and once that is up it Close()s every connection when requests remain;
//  2. that reserve is for waiting out the handlers that really return, where a handler that winds
//     down on a cancelled ctx finishes;
//  3. with a handler still unreturned at the deadline, the error says how many are left
//     (N handler(s) still running...);
//  4. a handler that does not watch ctx cannot be stopped by the framework, and the shutdown still
//     runs its course within WithStopTimeout.
//
// WithStopTimeout=3s: the service's share is 2s and the Close lands at about 2s - 0.4s = 1.6s.
// Two /stuck (not watching ctx) plus one /slow (watching it), so the error should say exactly 2
func TestShutdown_CtxIgnoringHandlerInFlight_ReportsRunningCount_ExitsInBudget(t *testing.T) {
	harness.Require(t)
	const (
		budget     = 3 * time.Second
		serverPart = budget - budget/3         // 2s
		closeAt    = serverPart - serverPart/5 // 1.6s: 20% of what is left is 0.4s, under 1s
		tolerance  = 300 * time.Millisecond    // measured within 20ms (delivering the signal plus the 10ms poll interval)
	)
	p := harness.Start(t, harness.Options{Overlay: stopTimeout(budget)})

	type result struct {
		path string
		at   time.Time
		resp harness.Response
		err  error
	}
	results := make(chan result, 3)
	for _, path := range []string{"/stuck?ms=20000", "/stuck?ms=20000", "/slow?ms=20000"} {
		go func() {
			r, err := p.Request(context.Background(), http.MethodGet, path, nil)
			results <- result{path, time.Now(), r, err}
		}()
	}
	waitGauge(t, p, "e2e_stuck_inflight", 2, 5*time.Second)
	waitGauge(t, p, "e2e_slow_inflight", 1, 5*time.Second)

	sigAt := time.Now()
	p.Signal(syscall.SIGTERM)
	exit, ok := p.Wait(budget + 5*time.Second)
	if !ok {
		t.Fatalf("the documentation says a handler that does not watch ctx cannot hold the shutdown up either (WithStopTimeout=%v), and the process is still alive %v after the signal",
			budget, budget+5*time.Second)
	}
	t.Logf("exit: %v (the budget %v, the service's share %v)", exit, budget, serverPart)

	// 1. At the Close, all three connections are broken and the clients get no response
	for range 3 {
		r := <-results
		cut := r.at.Sub(sigAt)
		t.Logf("%s: ended %s after the signal, err=%v status=%d", r.path, fmtMS(cut), r.err, r.resp.Status)
		if r.err == nil {
			t.Errorf("%s had not finished at the deadline and the documentation says Close() breaks every connection, and the client got the response %v", r.path, r.resp)
		}
		if cut < closeAt-tolerance || cut > closeAt+tolerance {
			t.Errorf("%s: the documentation says the Shutdown uses up to 20%% before the deadline (about %v after the signal) and then breaks the connections, got %v after the signal", r.path, closeAt, cut)
		}
	}

	// 2. The /slow that watches ctx winds down inside that reserve
	if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "slow request cancelled" })) != 1 {
		t.Errorf("a handler that watches ctx should wind down once the connection breaks, and there is no slow request cancelled log line")
	}

	// 3. The error says how many are left: the two /stuck, not counting the /slow that wound down
	const want = "2 handler(s) still running when the shutdown deadline passed"
	if !strings.Contains(p.Stderr(), want) {
		t.Errorf("the documentation says the error says how many are left (%q), got stderr:\n%s", want, lastLines(p.Stderr(), 10))
	}
	// MustRun writes Run's error to stderr and exits with 1
	if exit.Code != 1 {
		t.Errorf("Stop returned an error, so MustRun should exit with 1, got %v", exit)
	}

	// 4. It exits within the budget, and it really did wait out the service's share (it did not give up at once)
	if exit.SinceSignal > budget {
		t.Errorf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and it exited only %v after the signal", budget, exit.SinceSignal)
	}
	if exit.SinceSignal < serverPart-tolerance {
		t.Errorf("the documentation says Stop waits up to the deadline of the service's share (%v), and it exited %v after the signal", serverPart, exit.SinceSignal)
	}
	// The components close even though the service did not stop cleanly: xlog last
	stops := hookSeq(p, "stopping")
	if want := expectedStops(hookSeq(p, "starting")); !slices.Equal(pkgsOf(stops), want) {
		t.Errorf("once the service's share times out the components still close in reverse order: want %v, got %v", want, stops)
	}
}

// TestShutdown_SecondSignalKillsStuckShutdown
//
// The documentation (docs/architecture.md, "Exit signals", point 3): after the first signal the
// default disposition is restored, so while the framework sits in a call that does not watch ctx,
// a second signal terminates the process at once; "point 3 covers the shutdown phase as well: with
// Stop stuck, a second Ctrl+C works".
//
// How it is wedged: WithStopTimeout=60s with one /stuck?ms=60000 in flight -- Stop waits 40s before
// it gives up
func TestShutdown_SecondSignalKillsStuckShutdown(t *testing.T) {
	harness.Require(t)
	cases := []struct {
		name          string
		first, second syscall.Signal
	}{
		{"SIGTERM then SIGTERM again", syscall.SIGTERM, syscall.SIGTERM},
		{"SIGTERM then SIGINT", syscall.SIGTERM, syscall.SIGINT},
		{"SIGINT then SIGINT again", syscall.SIGINT, syscall.SIGINT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := harness.Start(t, harness.Options{Overlay: stopTimeout(time.Minute)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _, _ = p.Request(ctx, http.MethodGet, "/stuck?ms=60000", nil) }()
			waitGauge(t, p, "e2e_stuck_inflight", 1, 5*time.Second)

			p.Signal(c.first)
			l := p.WaitLog(t, 5*time.Second, func(l harness.Log) bool {
				return strings.HasPrefix(l.Msg(), "shutdown signal received, closing gracefully")
			})
			if got := l.Str("signal"); got != c.first.String() {
				t.Errorf("the signal in the log should be %q, got %q", c.first.String(), got)
			}
			// Make sure it really is stuck in Stop: otherwise the second signal does not test "while it is stuck"
			if e, exited := p.Wait(500 * time.Millisecond); exited {
				t.Fatalf("the /stuck in flight should wedge Stop (for 40s at most), and it exited after the first signal: %v", e)
			}

			secondAt := time.Now()
			p.Signal(c.second)
			e, ok := p.Wait(5 * time.Second)
			took := time.Since(secondAt)
			if !ok {
				t.Fatalf("the documentation says a second signal terminates the process at once, and 5s after the second %v it is still alive", c.second)
			}
			t.Logf("the second %v -> the process terminated after %s; %v", c.second, fmtMS(took), e)
			if e.Signal != c.second {
				t.Errorf("the second signal should go through the system's default disposition and be terminated by %v outright, got %v", c.second, e)
			}
			// Measured at 1 to 3ms, and 2s is given: either it dies at once, or the default disposition was not restored (which would sit out the 40s)
			if took > 2*time.Second {
				t.Errorf("the documentation says a second signal terminates the process at once, and it took %v", took)
			}
		})
	}
}

// TestShutdown_SIGTERMDuringStartup_NoServe_ClosesBuiltInReverse_ExitsZero
//
// The documentation (docs/architecture.md, "Exit signals", points 1 and 2): the signal is taken
// over before the configuration is read; the hooks take a ctx, and once a signal arrives the
// remaining start hooks do not run, what was built is closed cleanly in reverse, the service does
// not start, and it exits with 0 -- exiting as asked is no failure and is not reported as a failed
// start. The hook that is interrupted has to pass ctx down itself: xgorm/README.md says a
// PostgreSQL connect is governed by ctx; xredis.New's comment says "an exit signal should be given
// up on there and then", and ping's comment says "a cancelled parent is given up on at once".
//
// How a connect is slowed down: PG or Redis goes through a TCP proxy and the proxy holds the far
// end's answer back (SetDelay(time.Hour)), so TCP connects at once and the handshake is never
// answered. The signal is sent once that hook's starting line is in the log and the proxy has seen
// a connection.
//
// In those two cases the interrupted connect sees the cancelled ctx and returns an error, so
// runStart takes the error path -- and point 2's "the framework checks once more before every
// hook" never gets its turn (take that recheck out and these two still pass).
// So there is a third case: a start hook that does not watch ctx (warmup, Service.StartStall)
// receives the signal while it sleeps and returns successfully once it has slept; it counts as
// built, and the framework has to stop by itself before the next hook
func TestShutdown_SIGTERMDuringStartup_NoServe_ClosesBuiltInReverse_ExitsZero(t *testing.T) {
	harness.Require(t)
	// immediate is the bound of "given up on there and then". PG measured 2 to 3ms after the signal
	// (pgx's read returns at once on a cancelled ctx), and 200ms is dozens of times that while
	// staying far under Redis's default 500ms ReadTimeout
	const immediate = 200 * time.Millisecond
	// stall is how long warmup sleeps without watching ctx. It cannot be interrupted, so the bound on the exit is that sleep plus immediate
	const stall = 500 * time.Millisecond
	cases := []struct {
		name   string
		hook   string // the start hook that is wedged
		target string // the far end whose answer the proxy holds back; empty means no proxy
		opts   func(addr string) harness.Options
		stall  time.Duration // how long the warmup start hook sleeps without watching ctx (Service.StartStall), 0 not sleeping
		// completes: the wedged hook does not watch ctx and returns successfully once it has slept, so it counts as built and has to be closed
		completes bool
		limit     time.Duration // how long after the signal it has to exit, 0 meaning immediate
	}{
		{name: "while PG is connecting", hook: "xgorm.initXGorm", target: harness.PGAddr(),
			opts: func(a string) harness.Options { return harness.Options{PGAddr: a} }},
		{name: "a start hook that does not watch ctx", hook: "warmup.preload", completes: true, limit: stall + immediate, stall: stall,
			opts: func(string) harness.Options { return harness.Options{} }},
		{name: "while Redis is connecting", hook: "xredis.initXRedis", target: harness.RedisAddr(),
			// go-redis honors a ctx deadline alone and a cancellation does not wake a Ping blocked on a read, which is why xredis's probe runs on a goroutine
			// while this side watches ctx: the signal used to take effect only once that read hit ReadTimeout (about 450ms with the default 500ms, about 2.95s with 3s configured).
			// ReadTimeout is set to 3s: an order of magnitude over immediate, so falling back to the old behavior is plain to see
			opts: func(a string) harness.Options {
				return harness.Options{RedisAddr: a, Overlay: "XRedis:\n  ReadTimeout: 3s\n"}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var proxy *harness.Proxy
			var o harness.Options
			if c.target != "" {
				proxy = harness.NewProxy(t, c.target)
				proxy.SetDelay(time.Hour)
				o = c.opts(proxy.Addr())
			} else {
				o = c.opts("")
			}
			o.NoWait = true
			o.Overlay += fmt.Sprintf("Service:\n  StopTimeout: 5s\n  StartStall: %v\n", c.stall)
			p := harness.Start(t, o)

			p.WaitLog(t, 15*time.Second, func(l harness.Log) bool { return l.Msg() == "starting" && l.Str("hook") == c.hook })
			if proxy != nil {
				deadline := time.Now().Add(5 * time.Second)
				for proxy.Accepted() == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if proxy.Accepted() == 0 {
					t.Fatalf("%s has been going for 5s and the proxy has had no connection", c.hook)
				}
			}
			time.Sleep(50 * time.Millisecond) // let it reach the handshake and wait for an answer, or reach warmup's Sleep

			p.Signal(syscall.SIGTERM)
			exit, ok := p.Wait(10 * time.Second)
			if !ok {
				t.Fatalf("a SIGTERM arrived during the start and the process is still alive 10s later\n%s", lastLines(p.Output(), 30))
			}
			if proxy != nil {
				t.Logf("exit: %v (the signal was sent while %s was wedged, and the proxy saw %d connections)", exit, c.hook, proxy.Accepted())
			} else {
				t.Logf("exit: %v (the signal was sent while %s was wedged)", exit, c.hook)
			}

			if exit.Code != 0 || exit.Signal != nil {
				t.Errorf("the documentation says exiting as asked during the start exits with 0, got %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 20))
			}

			starts := hookSeq(p, "starting")
			if len(starts) == 0 || starts[len(starts)-1] != c.hook {
				t.Fatalf("no further start hook should run after the signal: the last starting should be %s, got %v", c.hook, starts)
			}
			started := starts[:len(starts)-1] // the last one was interrupted and never came up
			if c.completes {
				started = starts // the one that does not watch ctx succeeded after its sleep and counts as built
			}

			// "The service does not start": xgin never listened
			if ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" }); len(ls) > 0 {
				t.Errorf("the documentation says the service does not start once a signal arrives during the start, and the log holds %s", ls[0].Line)
			}
			// The framework says why it did not start and does not treat it as a failure: at INFO, with ready being how many were built
			ls := p.FindLogs(func(l harness.Log) bool {
				return l.Msg() == "shutdown signal received during startup, not starting the server"
			})
			if len(ls) != 1 {
				t.Errorf("there should be one shutdown signal received during startup log line, got %d", len(ls))
			} else {
				t.Logf("what the framework says: %s", ls[0].Line)
				if ls[0].Level() != "INFO" {
					t.Errorf("exiting as asked is no failure and this line should be INFO, got %s", ls[0].Level())
				}
				if got, want := ls[0].Str("ready"), fmt.Sprint(len(started)); got != want {
					t.Errorf("ready should be the %s that were built, got %s", want, got)
				}
			}

			// "What was built is closed cleanly in reverse": exactly the ones that were built, in reverse; the interrupted one and the ones that never had their turn are not closed
			stops := hookSeq(p, "stopping")
			t.Logf("built: %v; closed: %v", started, stops)
			if want := expectedStops(started); !slices.Equal(pkgsOf(stops), want) {
				t.Errorf("the documentation says what was built is closed cleanly in reverse: want %v, got %v", want, pkgsOf(stops))
			}

			// "It is not reported as a failed start": no error from MustRun and no ERROR log line
			for _, line := range strings.Split(p.Stderr(), "\n") {
				if strings.Contains(line, "failed, err=[") {
					t.Errorf("the documentation says it is not reported as a failed start, and stderr holds: %s", line)
				}
			}
			for _, l := range p.Logs() {
				if l.Level() == "ERROR" {
					t.Errorf("exiting as asked should have no ERROR log line, got: %s", l.Line)
				}
			}

			// The interrupted connect is given up on there and then; the one that does not watch ctx leaves after its sleep and runs nothing else
			limit := cmp.Or(c.limit, immediate)
			if exit.SinceSignal > limit {
				t.Errorf("the documentation says a connect is given up on there and then on an exit signal and the remaining start hooks do not run, and it exited only %v after the signal (the bound being %v)", exit.SinceSignal, limit)
			}
		})
	}
}

// TestShutdown_AfterDisconnect_PGCallWithCtxStops_RedisWaitsForPoolClose
//
// The documentation (the XGin section of xgin/README.md): for a request that has not finished at
// the deadline, Close() breaks the connection and cancels the request's ctx; "a slow operation in a
// handler (a query, a downstream call) has to be passed c.Request.Context() to be stoppable once
// the connection breaks", and the reserve is there to wait such a handler out.
//
// Redis is the exception that section names (xredis/README.md, "Behavior (measured)"): go-redis
// honors a ctx deadline alone, a cancellation does not wake a command blocked on a read, and it
// returns only at ReadTimeout, at the ctx deadline, or when xredis's stop hook closes the pool. So
// what the Redis case asserts is exactly that: the handler returns only after closeXRedis, and Stop
// reports truthfully that a handler has not returned.
//
// How it is done: the service reaches PG / Redis through a TCP proxy, and once it is up the proxy
// holds the far end's answer back (SetDelay(time.Hour)), so a request reading a user is stuck on
// the PG / Redis read -- with the handler passing the request's ctx. WithStopTimeout=3s: the
// connections break at about 1.6s and the service's share is up at 2.0s. With the handler watching
// ctx it winds down right after the connections break, and what Stop reports is "force closed after
// the timeout" rather than "N handlers have not returned".
//
// The Redis case sets ReadTimeout to 10s: the default 500ms is shorter than the reserve left after
// the connections break (0.4s), so one read would time out by itself and whether ctx works could
// not be told
func TestShutdown_AfterDisconnect_PGCallWithCtxStops_RedisWaitsForPoolClose(t *testing.T) {
	harness.Require(t)
	const (
		budget     = 3 * time.Second
		serverPart = budget - budget/3         // 2s
		closeAt    = serverPart - serverPart/5 // 1.6s
	)
	cases := []struct {
		name   string
		target string
		opts   func(addr string) harness.Options
		path   string // %d is the user id
		// untilPoolClosed: a cancellation does not wake a call blocked on this dependency, and the handler returns only once its stop hook has closed the pool
		untilPoolClosed string
	}{
		{name: "PG", target: harness.PGAddr(), path: "/users/%d?cache=off",
			opts: func(a string) harness.Options { return harness.Options{PGAddr: a} }},
		{name: "Redis", target: harness.RedisAddr(), path: "/users/%d",
			opts: func(a string) harness.Options {
				return harness.Options{RedisAddr: a, Overlay: "XRedis:\n  ReadTimeout: 10s\n"}
			},
			// Measured, the connections break 1.6s after the signal and the handler returns only at 2.0s, when the service's share is up
			// and xredis's stop hook closes the pool
			untilPoolClosed: "xredis.closeXRedis"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proxy := harness.NewProxy(t, c.target)
			o := c.opts(proxy.Addr())
			o.Overlay += stopTimeout(budget)
			p := harness.Start(t, o)
			u := createUser(t, p, "slowcall", "slowcall@example.com")
			path := fmt.Sprintf(c.path, u.ID)

			proxy.SetDelay(time.Hour)
			type result struct {
				at   time.Time
				resp harness.Response
				err  error
			}
			done := make(chan result, 1)
			go func() {
				r, err := p.Request(context.Background(), http.MethodGet, path, nil)
				done <- result{time.Now(), r, err}
			}()
			// Entering the handler and sending the query takes microseconds, so 200ms is enough for it to be stuck in the read
			time.Sleep(200 * time.Millisecond)

			sigAt := time.Now()
			p.Signal(syscall.SIGTERM)
			exit, ok := p.Wait(budget + 5*time.Second)
			if !ok {
				t.Fatalf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and the process is still alive %v after the signal", budget, budget+5*time.Second)
			}
			r := <-done
			t.Logf("exit: %v; the client ended %s after the signal (err=%v)", exit, fmtMS(r.at.Sub(sigAt)), r.err)
			if exit.SinceSignal > budget {
				t.Errorf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and it exited only %v after the signal", budget, exit.SinceSignal)
			}

			// When the handler returned and what it met before returning: the access log is written after the handler returns,
			// and its path carries no query string
			route, _, _ := strings.Cut(path, "?")
			var completedAt, poolClosedAt time.Time
			for _, l := range p.FindLogs(func(l harness.Log) bool {
				return (l.Msg() == "request completed" && l.Str("path") == route) ||
					l.Msg() == "redis read failed, falling back to the database" || l.Msg() == "read user failed" ||
					(l.Msg() == "stopping" && (l.Str("hook") == "xredis.closeXRedis" || l.Str("hook") == "xgorm.closeXGorm"))
			}) {
				at, _ := time.Parse(time.RFC3339Nano, l.Str("time"))
				switch {
				case l.Msg() == "request completed":
					completedAt = at
				case l.Msg() == "stopping" && l.Str("hook") == c.untilPoolClosed:
					poolClosedAt = at
				}
				t.Logf("%s after the signal (the connections breaking at about %v): %s %s%s%s", fmtMS(at.Sub(sigAt)), closeAt, l.Msg(),
					l.Str("hook"), l.Str("status"), l.Str("error"))
			}

			stderr := p.Stderr()
			if c.untilPoolClosed != "" {
				// The handler wakes only once the pool is closed, which is tens of microseconds before the process exits: its access log
				// sometimes does not make it out (hit once in a full e2e run). Not making it out says just as much about it not being woken by the break,
				// and the evidence for that is the "1 handler(s) still running" below; if it did make it out, it has to be later than the pool closing
				if poolClosedAt.IsZero() || (!completedAt.IsZero() && completedAt.Before(poolClosedAt)) {
					t.Errorf("the documentation says a cancellation does not wake a blocked Redis command and the handler returns only once %s has closed the pool: the handler returned at %v and %s at %v. "+
						"Had the handler returned first, go-redis would have begun honoring cancellation, and that limitation in the XRedis section of xredis/README.md should be taken out",
						c.untilPoolClosed, completedAt, c.untilPoolClosed, poolClosedAt)
				}
				if !strings.Contains(stderr, "1 handler(s) still running") {
					t.Errorf("the handler returned after the deadline of the service's share, which Stop should report truthfully, got stderr:\n%s", lastLines(stderr, 5))
				}
				return
			}
			if strings.Contains(stderr, "handler(s) still running") {
				t.Errorf("the documentation says a slow operation carrying the request's ctx is stoppable once the connection breaks, and Stop reports that a handler did not return:\n%s", lastLines(stderr, 5))
			}
			// Every handler returned before the deadline, which is what makes Stop report "force closed"; with one unreturned it reports the line above
			if !strings.Contains(stderr, "graceful shutdown timed out, connections were force closed") {
				t.Errorf("requests still running at the deadline should be force closed and reported, got stderr:\n%s", lastLines(stderr, 5))
			}
		})
	}
}
