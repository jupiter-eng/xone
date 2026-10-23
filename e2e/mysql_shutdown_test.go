package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// mysqlLogTime is a log line's time field
func mysqlLogTime(l harness.Log) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, l.Str("time"))
	return at
}

// A query running on MySQL when SIGTERM arrives: the README's "Staged initialization, reverse-order
// shutdown" -- xgorm is in StageClient, so it starts before the service (xgin) and closes after it;
// docs/architecture.md, "The stop budget is one": the service stops first and the requests in
// flight are waited for, and only then come the client-style components. So a SELECT SLEEP in
// flight has to finish sleeping on the server and come back 200, and the handler that does not
// watch ctx has to still reach MySQL after its sleep (mysql_error=none) -- all of it before
// xgorm's stop hook closes the pool
func TestMySQL_SIGTERMFinishesInFlightQueriesBeforeClosingPool(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const (
		budget = 8 * time.Second
		sleeps = 6
		stucks = 2
		sleep  = 1500 * time.Millisecond
	)
	p := harness.Start(t, harness.Options{Overlay: stopTimeout(budget)})

	type shot struct {
		path string
		r    harness.Response
		err  error
		at   time.Time
	}
	var mu sync.Mutex
	var shots []shot
	var wg sync.WaitGroup
	fire := func(path string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.Request(context.Background(), http.MethodGet, path, nil)
			mu.Lock()
			shots = append(shots, shot{path, r, err, time.Now()})
			mu.Unlock()
		}()
	}
	// When the sleeping started, which is what the assertions below go by: the SELECT SLEEPs begin
	// here and not at the signal, and what is between the two -- waiting for the gauges, reading the
	// processlist -- is time they have already slept through by then
	firedAt := time.Now()
	for range sleeps {
		fire(fmt.Sprintf("/mysql/sleep?ms=%d", sleep.Milliseconds()))
	}
	for range stucks {
		fire(fmt.Sprintf("/stuck?ms=%d&mysql=1", sleep.Milliseconds()))
	}
	waitGauge(t, p, "e2e_mysql_sleep_inflight", sleeps, 5*time.Second)
	waitGauge(t, p, "e2e_stuck_inflight", stucks, 5*time.Second)
	// The SELECT SLEEPs have reached MySQL: they are visible in the processlist, so they are running on the server when the signal arrives
	var running int
	if err := harness.MySQL(t).QueryRow(`SELECT COUNT(*) FROM information_schema.processlist WHERE info LIKE 'SELECT SLEEP(%'`).Scan(&running); err != nil {
		t.Fatalf("reading the processlist: %v", err)
	}
	if running < sleeps {
		t.Fatalf("%d SELECT SLEEPs should be running on MySQL before the signal, the processlist holds only %d", sleeps, running)
	}
	sigAt := time.Now()
	p.Signal(syscall.SIGTERM)
	exit, ok := p.Wait(budget + 5*time.Second)
	if !ok {
		t.Fatalf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and the process is still alive %v after the signal", budget, budget+5*time.Second)
	}
	wg.Wait()
	t.Logf("exit: %v", exit)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("every request in flight finished within the budget, so it should exit with 0, got %v\nstderr:\n%s", exit, lastLines(p.Stderr(), 10))
	}
	for _, s := range shots {
		if s.err != nil || s.r.Status != http.StatusOK {
			t.Errorf("%s was in flight at the signal and should finish with a 200, got err=%v %v", s.path, s.err, s.r)
		}
	}
	stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" && l.Str("hook") == "xgorm.closeXGorm" })
	if len(stops) != 1 {
		t.Fatalf("there should be one xgorm.closeXGorm stopping line, got %d", len(stops))
	}
	closeAt := mysqlLogTime(stops[0])
	var last time.Time
	done := p.FindLogs(func(l harness.Log) bool {
		return l.Msg() == "mysql sleep finished" || l.Msg() == "stuck request finished"
	})
	if len(done) != sleeps+stucks {
		t.Errorf("all %d requests in flight should finish and record a line, got %d", sleeps+stucks, len(done))
	}
	for _, l := range done {
		if l.Msg() == "stuck request finished" && l.Str("mysql_error") != "none" {
			t.Errorf("the pool should still be open when the handler that does not watch ctx queries MySQL after its sleep, got mysql_error=%q", l.Str("mysql_error"))
		}
		last = later(last, mysqlLogTime(l))
	}
	if !closeAt.After(last) {
		t.Errorf("xgorm's pool should close only once every MySQL query in flight has finished: the last one finished %s after the signal and the pool closed %s after it",
			fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)))
	}
	// That the queries were not cut short, and that the process stayed for them. Both go by when
	// the sleeping began rather than by the signal: it used to assert that the process outlived the
	// signal by the whole sleep, which only holds when the signal arrives the moment the queries
	// start. It does not -- waiting for the gauges and reading the processlist come in between --
	// so on a slow machine the queries had already slept most of it and the process rightly exited
	// sooner. Measured on a CI runner: the last query finished 1019ms after the signal rather than
	// the 1500ms the assertion wanted, with nothing wrong (https://github.com/jupiter-eng/xone/actions/runs/39854239684)
	if ran := last.Sub(firedAt); ran < sleep {
		t.Errorf("the queries in flight should run their whole %v rather than be cut short, and the last finished %s after it was sent", sleep, fmtMS(ran))
	}
	if exitAt := sigAt.Add(exit.SinceSignal); !exitAt.After(last) {
		t.Errorf("the process should outlive the queries in flight: the last finished %s after the signal and it exited %s after it",
			fmtMS(last.Sub(sigAt)), fmtMS(exit.SinceSignal))
	}
	t.Logf("numbers: the last MySQL query in flight finished %s after the signal, xgorm's pool closed %s after it, and the process exited after %v",
		fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)), exit.SinceSignal.Round(time.Millisecond))
}

// With MySQL saying nothing, whether a query carrying the request's ctx stops when that ctx is
// cancelled (rather than when a deadline passes).
//
// The XGorm section of xgorm/README.md used to say nothing about cancellation on MySQL (it only
// said, for PG, "the request's own ctx works in a web request as well: it is cancelled when the
// client disconnects and the query returns with it"). Measured: go-sql-driver closes the
// connection when ctx ends, so a query blocked on a read returns there and then (context
// cancelled) without waiting for ReadTimeout. The measured numbers went into xgorm/README.md,
// "MySQL", and this asserts what it says.
//
// ReadTimeout is set to 10s: with the default 3s the read timeout could arrive on its own during
// the stretch after the disconnect, and the cancellation would not be what is measured
func TestMySQL_StuckQuery_ReturnsOnRequestCtxCancel_NoReadTimeoutWait(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const readTimeout = 10 * time.Second
	overlay := mysqlOverlay("MySQL:\n  ReadTimeout: 10s")

	t.Run("the client disconnects", func(t *testing.T) {
		t.Parallel()
		const giveUp = 500 * time.Millisecond
		my := harness.NewProxy(t, harness.MySQLAddr())
		p := harness.Start(t, harness.Options{MySQLAddr: my.Addr(), Overlay: overlay})
		my.SetDelay(time.Hour)
		r := mysqlDep(p, "", giveUp)
		if r.ReqErr == nil {
			t.Fatalf("with no deadline and ReadTimeout 10s, the query should not return within %v, got %v", giveUp, r)
		}
		gaveUpAt := time.Now()
		l, ok := p.LookForLog(readTimeout, func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/dep" })
		if !ok {
			t.Fatalf("the request's ctx is cancelled once the client disconnects and the query should return with it: the handler had still not returned after %v", readTimeout)
		}
		after := mysqlLogTime(l).Sub(gaveUpAt)
		if after > faultSlack {
			t.Errorf("the query should return there and then once the client disconnects (the documentation: go-sql-driver closes the connection when ctx ends), but the handler returned only %v later", after)
		}
		t.Logf("numbers: with MySQL silent and the client giving up after %v, the handler returned %s later (status=%s)", giveUp, fmtMS(after), l.Str("status"))
	})

	t.Run("the connection is broken after SIGTERM", func(t *testing.T) {
		t.Parallel()
		const (
			budget     = 3 * time.Second
			serverPart = budget - budget/3         // 2s
			closeAt    = serverPart - serverPart/5 // 1.6s
		)
		my := harness.NewProxy(t, harness.MySQLAddr())
		p := harness.Start(t, harness.Options{MySQLAddr: my.Addr(), Overlay: overlay + stopTimeout(budget)})
		u, _ := mysqlCreate(t, p, "stalled", "stalled@example.com")
		my.SetDelay(time.Hour)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = p.Request(context.Background(), http.MethodGet, fmt.Sprintf("/mysql/users/%d", u.ID), nil)
		}()
		time.Sleep(200 * time.Millisecond) // entering the handler and sending the query takes microseconds, so 200ms is enough for it to be stuck in the read

		sigAt := time.Now()
		p.Signal(syscall.SIGTERM)
		exit, ok := p.Wait(budget + 5*time.Second)
		<-done
		if !ok {
			t.Fatalf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and the process is still alive", budget)
		}
		if exit.SinceSignal > budget {
			t.Errorf("the documentation says the whole shutdown stays within WithStopTimeout=%v, and it exited only %v after the signal", budget, exit.SinceSignal)
		}
		stderr := p.Stderr()
		if strings.Contains(stderr, "handler(s) still running") {
			t.Errorf("a MySQL query carrying the request's ctx should be stoppable once the connection breaks, but Stop reports that a handler did not return:\n%s", lastLines(stderr, 5))
		}
		if !strings.Contains(stderr, "graceful shutdown timed out, connections were force closed") {
			t.Errorf("requests still running at the deadline should be force closed and reported, got stderr:\n%s", lastLines(stderr, 5))
		}
		ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "read mysql user failed" })
		if len(ls) != 1 {
			t.Fatalf("the handler should record one read mysql user failed once the stuck query is cancelled, got %d\n%s", len(ls), lastLines(p.Output(), 20))
		}
		at := mysqlLogTime(ls[0]).Sub(sigAt)
		if at > serverPart+faultSlack {
			t.Errorf("the connection breaks about %v after the signal and the query should return with it, but it returned only %s after the signal", closeAt, fmtMS(at))
		}
		t.Logf("numbers: the stuck MySQL query returned %s after the SIGTERM (budget %v, the connection breaking at about %v): %s; the process exited after %v",
			fmtMS(at), budget, closeAt, ls[0].Str("error"), exit.SinceSignal.Round(time.Millisecond))
	})
}

// MySQL says nothing during the start and a SIGTERM arrives. xgorm/README.md, XGorm: every MySQL
// connect (the version query included) happens inside the connect validation that ctx governs, so
// an exit signal during the start takes effect there and then and does not wait for the handshake
// read to hit ReadTimeout.
//
// This used to be a KNOWN BUG: it exited about 2.95s after the signal (= the handshake read
// waiting out its 3s ReadTimeout, less the stretch already waited before the signal). That read
// happens in gorm.Open where Dialector.Initialize queries the version, on a context.Background();
// the documentation furthermore put its upper bound at DialTimeout, while with TCP connecting at
// once and the handshake answering nothing, what governs that read is readTimeout
func TestMySQL_SIGTERMWhileSilentAtStartup_ExitsImmediately(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const immediate = 200 * time.Millisecond // the same "gives up there and then" as TestShutdown_SIGTERMDuringStartup_NoServe_ClosesBuiltInReverse_ExitsZero
	my := harness.NewProxy(t, harness.MySQLAddr())
	my.SetDelay(time.Hour)
	p := harness.Start(t, harness.Options{MySQLAddr: my.Addr(), NoWait: true, Overlay: stopTimeout(5 * time.Second)})
	p.WaitLog(t, 15*time.Second, func(l harness.Log) bool { return l.Msg() == "starting" && l.Str("hook") == "xgorm.initXGorm" })
	deadline := time.Now().Add(5 * time.Second)
	for my.Accepted() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if my.Accepted() == 0 {
		t.Fatal("xgorm has been going for 5s and MySQL's proxy has had no connection")
	}
	time.Sleep(50 * time.Millisecond)
	p.Signal(syscall.SIGTERM)
	exit, ok := p.Wait(15 * time.Second)
	if !ok {
		t.Fatalf("a SIGTERM arrived during the start and the process is still alive 15s later\n%s", lastLines(p.Output(), 30))
	}
	t.Logf("numbers: with MySQL silent at the start it exited %v after the signal (%v); the last line of stderr: %s", exit.SinceSignal.Round(time.Millisecond), exit, lastLine(p.Stderr()))
	if ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" }); len(ls) > 0 {
		t.Errorf("the service should not start once a signal arrived during the start, and the log holds %s", ls[0].Line)
	}
	if exit.SinceSignal > immediate {
		t.Errorf("the documentation says an exit signal during the start takes effect there and then, and it exited only %v after the signal (ReadTimeout %v)", exit.SinceSignal.Round(time.Millisecond), mysqlReadTimeout)
	}
}
