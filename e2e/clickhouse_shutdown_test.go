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

// chRunning is how many queries holding marker are running on the ClickHouse server right now (the one querying system.processes aside)
func chRunning(t *testing.T, marker string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n uint64
	if err := harness.CH(t).QueryRowContext(ctx,
		`SELECT count() FROM system.processes WHERE query LIKE ? AND query NOT LIKE '%system.processes%'`, "%"+marker+"%").Scan(&n); err != nil {
		t.Fatalf("reading system.processes: %v", err)
	}
	return int(n)
}

// A query running on ClickHouse when SIGTERM arrives: the README's "Staged initialization,
// reverse-order shutdown" -- xgorm is in StageClient, so it starts before the service (xgin) and
// closes after it; docs/architecture.md, "The stop budget is one": the service stops first and the
// requests in flight are waited for, and only then come the client-style components. So a SELECT
// sleep in flight has to finish sleeping on the server and come back 200, and the handler that
// does not watch ctx has to still reach ClickHouse after its sleep (ch_error=none) -- all of it
// before xgorm's stop hook closes the pool
func TestClickHouse_SIGTERMFinishesInFlightQueriesBeforeClosingPool(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	const (
		budget = 8 * time.Second
		sleeps = 6
		stucks = 2
		sleep  = 1500 * time.Millisecond
	)
	p := chStart(t, harness.Options{Overlay: stopTimeout(budget)})

	type shot struct {
		path string
		r    harness.Response
		err  error
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
			shots = append(shots, shot{path, r, err})
			mu.Unlock()
		}()
	}
	for range sleeps {
		fire(fmt.Sprintf("/ch/sleep?ms=%d", sleep.Milliseconds()))
	}
	for range stucks {
		fire(fmt.Sprintf("/stuck?ms=%d&ch=1", sleep.Milliseconds()))
	}
	waitGauge(t, p, "e2e_ch_sleep_inflight", sleeps, 5*time.Second)
	waitGauge(t, p, "e2e_stuck_inflight", stucks, 5*time.Second)
	// The sleeps have reached ClickHouse: they are visible in system.processes, so they are running on the server when the signal arrives
	deadline := time.Now().Add(2 * time.Second)
	// The driver substitutes the parameters into the statement before sending it: clickhouse-go v2.48.0 writes 1.5 as cast(1.5, 'Float64') (in v2.30.0 it was a bare 1.5)
	const marker = "sleep(cast(1.5, 'Float64'))"
	running := chRunning(t, marker)
	for running < sleeps && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		running = chRunning(t, marker)
	}
	if running < sleeps {
		t.Fatalf("%d SELECT sleeps should be running on ClickHouse before the signal, system.processes holds only %d", sleeps, running)
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
	if exit.SinceSignal < sleep-500*time.Millisecond {
		t.Errorf("the queries in flight sleep %v and the process should wait for them, but it exited %v after the signal", sleep, exit.SinceSignal)
	}

	stops := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "stopping" && l.Str("hook") == "xgorm.closeXGorm" })
	if len(stops) != 1 {
		t.Fatalf("there should be one xgorm.closeXGorm stopping line, got %d", len(stops))
	}
	closeAt := mysqlLogTime(stops[0])
	var last time.Time
	done := p.FindLogs(func(l harness.Log) bool {
		return l.Msg() == "ch sleep finished" || l.Msg() == "stuck request finished"
	})
	if len(done) != sleeps+stucks {
		t.Errorf("all %d requests in flight should finish and record a line, got %d", sleeps+stucks, len(done))
	}
	for _, l := range done {
		if l.Msg() == "stuck request finished" && l.Str("ch_error") != "none" {
			t.Errorf("the pool should still be open when the handler that does not watch ctx queries ClickHouse after its sleep, got ch_error=%q", l.Str("ch_error"))
		}
		last = later(last, mysqlLogTime(l))
	}
	if !closeAt.After(last) {
		t.Errorf("xgorm's pool should close only once every ClickHouse query in flight has finished: the last one finished %s after the signal and the pool closed %s after it",
			fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)))
	}
	t.Logf("numbers: the last ClickHouse query in flight finished %s after the signal, xgorm's pool closed %s after it, and the process exited after %v",
		fmtMS(last.Sub(sigAt)), fmtMS(closeAt.Sub(sigAt)), exit.SinceSignal.Round(time.Millisecond))
}

// With ClickHouse saying nothing, whether a query carrying the request's ctx stops when that ctx
// is cancelled (rather than when a deadline passes).
//
// xgorm/clickhouse/README.md, "Behavior (measured)": "a query and an exec honor ctx's
// cancellation: reading the packets back happens in a goroutine, a cancelled ctx returns context
// canceled there and then, and a Cancel packet goes to the server while this connection is closed
// (and not returned to the pool)". Measured on a pooled connection:
//
//   - the client disconnects: the request's ctx is cancelled, the handler returns there and then
//     and does not wait for read_timeout (300s by default);
//   - the connection is broken after SIGTERM: a cancellation as well, so the stuck query returns
//     the moment the connection breaks and the process exits within the budget
func TestClickHouse_StuckQuery_ReturnsOnRequestCtxCancel_NoReadTimeoutWait(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()

	t.Run("the client disconnects", func(t *testing.T) {
		t.Parallel()
		const giveUp = 500 * time.Millisecond
		chp := harness.NewProxy(t, harness.CHAddr())
		p := chStart(t, harness.Options{CHAddr: chp.Addr()})
		chFillPool(t, p, 2)
		chp.SetDelay(time.Hour)
		r := chDep(p, "", giveUp)
		if r.ReqErr == nil {
			t.Fatalf("with no deadline and read_timeout 300s, the query should not return within %v, got %v", giveUp, r)
		}
		gaveUpAt := time.Now()
		l, ok := p.LookForLog(10*time.Second, func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/dep" })
		if !ok {
			t.Fatalf("the request's ctx is cancelled once the client disconnects and the query should return with it: the handler had still not returned after 10s")
		}
		after := mysqlLogTime(l).Sub(gaveUpAt)
		if after > faultSlack {
			t.Errorf("the query should return there and then once the client disconnects (the documentation: a cancelled ctx returns context canceled there and then), but the handler returned only %v later", after)
		}
		t.Logf("numbers: with ClickHouse silent and the client giving up after %v, the handler returned %s later (status=%s)", giveUp, fmtMS(after), l.Str("status"))
	})

	t.Run("the connection is broken after SIGTERM", func(t *testing.T) {
		t.Parallel()
		const (
			budget     = 3 * time.Second
			serverPart = budget - budget/3         // 2s
			closeAt    = serverPart - serverPart/5 // 1.6s
		)
		chp := harness.NewProxy(t, harness.CHAddr())
		p := chStart(t, harness.Options{CHAddr: chp.Addr(), Overlay: stopTimeout(budget)})
		ids, _ := chInsert(t, p, "stalled", 1)
		chp.SetDelay(time.Hour)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = p.Request(context.Background(), http.MethodGet, fmt.Sprintf("/ch/events/%d", ids[0]), nil)
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
			t.Errorf("a ClickHouse query carrying the request's ctx should be stoppable once the connection breaks, but Stop reports that a handler did not return:\n%s", lastLines(stderr, 5))
		}
		ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "read ch event failed" })
		if len(ls) != 1 {
			t.Fatalf("the handler should record one read ch event failed once the stuck query is cancelled, got %d\n%s", len(ls), lastLines(p.Output(), 20))
		}
		at := mysqlLogTime(ls[0]).Sub(sigAt)
		if at > serverPart+faultSlack {
			t.Errorf("the connection breaks about %v after the signal and the query should return with it, but it returned only %s after the signal", closeAt, fmtMS(at))
		}
		if !strings.Contains(ls[0].Str("error"), "context canceled") {
			t.Errorf("a cancelled query should report context canceled, got %q", ls[0].Str("error"))
		}
		t.Logf("numbers: the stuck ClickHouse query returned %s after the SIGTERM (budget %v, the connection breaking at about %v): %s; the process exited after %v",
			fmtMS(at), budget, closeAt, ls[0].Str("error"), exit.SinceSignal.Round(time.Millisecond))
	})
}

// Whether the query on the server stops once the client gives up. xgorm/clickhouse/README.md: on a
// cancellation the driver "sends a Cancel packet to the server".
// Having received the Cancel, ClickHouse stops while handling the next data block:
//
//   - a query emitting one data block per row and sleeping 100ms between them disappears from
//     system.processes soon after the client gives up;
//   - SELECT sleep(2.5) is one function call with no block boundary and the Cancel cannot interrupt
//     it: the server sleeps it out all the same (measured: it disappeared only after 2.5s).
//     The caller's side still returns there and then and the connection is still closed; it is only
//     this bit of server resource that has to run itself out
func TestClickHouse_ClientGivesUp_DriverSendsCancel_ServerStopsPerBlock(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{})
	for _, c := range []struct {
		name, path, marker string
		gone               time.Duration // how long the server may take before this query is gone
	}{
		// The N in numbers(N) is the marker: a query with parameters reaches the server as the text after the substitution.
		// An integer is substituted as it is, and clickhouse-go v2.48.0 writes a float as cast(2.5, 'Float64') (in v2.30.0 it was a bare 2.5)
		{"a query going block by block", "/ch/stream?rows=4711&ms=100", "numbers(4711)", time.Second},
		{"a single sleep", "/ch/sleep?ms=2500", "sleep(cast(2.5, 'Float64'))", 3 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			const giveUp = 300 * time.Millisecond
			r := chCall(p, c.path, giveUp)
			if r.ReqErr == nil {
				t.Fatalf("this query runs for several seconds and should not return within %v, got %v", giveUp, r.faultDepResult)
			}
			gaveUpAt := time.Now()
			var left time.Duration
			for {
				if chRunning(t, c.marker) == 0 {
					left = time.Since(gaveUpAt)
					break
				}
				if time.Since(gaveUpAt) > 5*time.Second {
					t.Fatalf("the server is still running %s 5s after the client gave up", c.marker)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if left > c.gone {
				t.Errorf("the server should stop within %v after the client gave up, got %v", c.gone, left)
			}
			// With a marker that does not match, the first look above is 0 and this assertion checks nothing: which is what happens when the driver changes how it substitutes parameters
			if n := chQueryStarts(t, c.marker); n == 0 {
				t.Fatalf("precondition: the server's query_log should hold a query with %q, and it does not (did the driver change how it substitutes parameters?)", c.marker)
			}
			t.Logf("numbers: %s: the server no longer held this query %s after the client gave up at %v", c.name, fmtMS(left), giveUp)
		})
	}
}
