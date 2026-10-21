package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// chResult is the result of GET /ch/sleep, /ch/echo and /ch/stream: the same two timings as
// faultDepResult, plus got / rows from the body
type chResult struct {
	faultDepResult
	Got  string
	Rows int
}

// chCall calls one /ch/... endpoint with the client waiting wait at most. It takes no t and can be
// called from any goroutine
func chCall(p *harness.Process, path string, wait time.Duration) chResult {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	start := time.Now()
	r, err := p.Request(ctx, http.MethodGet, path, nil)
	res := chResult{faultDepResult: faultDepResult{Status: r.Status, Took: time.Since(start), ReqErr: err}}
	if err == nil {
		var body struct {
			ElapsedMS float64 `json:"elapsed_ms"`
			Error     string  `json:"error"`
			Got       string  `json:"got"`
			Rows      int     `json:"rows"`
		}
		_ = json.Unmarshal(r.Body, &body)
		res.Server = time.Duration(body.ElapsedMS * float64(time.Millisecond))
		res.Error, res.Got, res.Rows = body.Error, body.Got, body.Rows
	}
	return res
}

// chFillPool runs n /ch/sleep calls concurrently to fill the pool to n connections: once it is
// stuck afterwards, the first n queries each take a connection the pool already holds
func chFillPool(t *testing.T, p *harness.Process, n int) {
	t.Helper()
	var wg sync.WaitGroup
	res := make([]chResult, n)
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); res[i] = chCall(p, "/ch/sleep?ms=200", 10*time.Second) }()
	}
	wg.Wait()
	for _, r := range res {
		if r.Status != http.StatusOK {
			t.Fatalf("SELECT sleep should answer 200 before the fault, got %v", r.faultDepResult)
		}
	}
}

// The ClickHouse process dies while running (the port refuses connections):
//
//   - an operation using ClickHouse fails there and then (a refused connection waits for no
//     timeout), with the address in the error;
//   - the other two instances (PG, MySQL) and the endpoints that do not touch ClickHouse are
//     unaffected: several instances means several pools, each its own;
//   - once ClickHouse is back it recovers without a restart
func TestClickHouse_RefusedAtRuntime_FailsFast_OthersUnaffected_Recovers(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	chp := harness.NewProxy(t, harness.CHAddr())
	p := chStart(t, harness.Options{CHAddr: chp.Addr()})
	ids, _ := chInsert(t, p, "before-cut", 1)

	chp.Cut()

	t.Run("an operation using ClickHouse fails there and then", func(t *testing.T) {
		var took []time.Duration
		for i := range 5 {
			r := chDep(p, "", 30*time.Second)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("attempt %d: /dep?target=ch should answer 503 while ClickHouse refuses connections, got %v", i+1, r)
			}
			if !strings.Contains(r.Error, "connection refused") || !strings.Contains(r.Error, chp.Addr()) {
				t.Errorf("attempt %d: the error should say what it cannot reach (%s, connection refused), got %q", i+1, chp.Addr(), r.Error)
			}
			if r.Server > chDialTimeout {
				t.Errorf("attempt %d: a refused connection should wait for no timeout, got %v", i+1, r.Server)
			}
			took = append(took, r.Server)
		}
		t.Logf("numbers: SELECT 1 took %v in turn while ClickHouse refused connections; %s", took, faultSummary(took))
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, fmt.Sprintf("/ch/events/%d", ids[0]), nil},
			{http.MethodGet, "/ch/stats?name=x", nil},
			{http.MethodPost, "/ch/events", map[string]any{"name": "while-cut", "values": []int{1}}},
		} {
			r, took := faultTimed(t, p, c.method, c.path, c.body)
			if r.Status != http.StatusServiceUnavailable {
				t.Errorf("%s %s should answer 503 while ClickHouse is unavailable, got %v", c.method, c.path, r)
			}
			if took > chDialTimeout {
				t.Errorf("%s %s should fail there and then while ClickHouse refuses connections, and it took %v", c.method, c.path, took)
			}
		}
	})

	t.Run("PG, MySQL and the endpoints that do not touch ClickHouse are unaffected", func(t *testing.T) {
		faultQuick(t, p, "GET /ping", "/ping", http.StatusOK)
		faultQuick(t, p, "GET /dep?target=db", "/dep?target=db", http.StatusOK)
		faultQuick(t, p, "GET /dep?target=mysql", "/dep?target=mysql", http.StatusOK)
	})

	t.Run("it recovers by itself once ClickHouse is back", func(t *testing.T) {
		chp.Restore()
		took, n := chWaitDep(t, p, 10*time.Second)
		t.Logf("numbers: SELECT 1 was back to 200 %s after ClickHouse started listening again (probe %d)", faultMS(took), n)
		fresh, _ := chInsert(t, p, "after", 2)
		if r := p.Get(t, fmt.Sprintf("/ch/events/%d", fresh[0])); r.Status != http.StatusOK {
			t.Errorf("reads and writes should work as usual after the recovery, got %v", r)
		}
	})
}

// ClickHouse says nothing / the host is down while running, and the caller gave a 200ms deadline.
//
// xgorm/clickhouse/README.md, "Behavior (measured)": "a read deadline is set once at the start of
// every query, and the caller's ctx deadline replaces it when there is one". That holds for a
// connection the pool already has. A new connection goes through database/sql's driver.Open
// (clickhouse-go v2.48.0's stdDriver still does not implement driver.DriverContext, so
// database/sql has no ctx to hand it): the dial is a net.DialTimeout and the handshake sets the
// whole connection's deadline from dial_timeout, neither of which looks at the caller's ctx. So
// once it is stuck the first few return at 200ms (the pooled connections, closed after they are
// cancelled), and once the pool is exhausted every one of them waits out dial_timeout (500ms by
// default) -- 300ms more than the caller asked for, but bounded
func TestClickHouse_HungOrDown_PooledConnsObeyDeadline_NewConnsWaitDialTimeout(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	const (
		deadline = 200 * time.Millisecond
		pooled   = 3
	)
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			chp := harness.NewProxy(t, harness.CHAddr())
			p := chStart(t, harness.Options{CHAddr: chp.Addr()})
			chFillPool(t, p, pooled)
			m.inject(chp)
			var fromPool, fresh []time.Duration
			for i := range pooled + 4 {
				accepted := chp.Accepted()
				r := chDep(p, deadline.String(), 10*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d: it should return a 503 while stuck, got %v", i+1, r)
				}
				// A new connection: with the far end silent the proxy accepted a new connection, and with the host down the error says dial tcp
				isNew := chp.Accepted() > accepted || strings.Contains(r.Error, "dial tcp")
				if !isNew {
					fromPool = append(fromPool, r.Server)
					if r.Server > deadline+faultSlack || r.Server < deadline-10*time.Millisecond {
						t.Errorf("attempt %d (a pooled connection): the caller gave %v and the query should return then, got %v: %s", i+1, deadline, r.Server, r.Error)
					}
					continue
				}
				fresh = append(fresh, r.Server)
				if r.Server > chDialTimeout+faultSlack {
					t.Errorf("attempt %d (a new connection): the dial and the handshake are governed by dial_timeout (%v) and it should return then, got %v: %s", i+1, chDialTimeout, r.Server, r.Error)
				}
			}
			if len(fromPool) == 0 || len(fresh) == 0 {
				t.Fatalf("both halves should be measured: %d pooled connections and %d new ones", len(fromPool), len(fresh))
			}
			t.Logf("numbers: ClickHouse %s with the caller giving %v: the pooled connections %s; the new ones %s", m.name, deadline, faultSummary(fromPool), faultSummary(fresh))
		})
	}
}

// ClickHouse says nothing while running and the caller gives no deadline:
//
//   - the only thing holding this query is the driver's read_timeout: with read_timeout=2s in the
//     DSN, this connection times out reading at 2s.
//     v2.48.0 takes a read timeout for a bad connection and reports driver.ErrBadConn, so
//     database/sql takes another connection and sends it once more: the new connection's handshake
//     meets the silent far end and fails after waiting out dial_timeout (500ms), which makes 2.5s
//     in all, with the error coming from that handshake's i/o timeout;
//   - without it, it is 300s (xgorm/clickhouse/README.md: "read_timeout is 300s when it is not
//     written"). Waiting 5 minutes is impractical: this only confirms that the query is still
//     hanging after 8s (measured on clickhouse-go v2.48.0: it returned after 300.6s -- after the
//     300s read timeout database/sql took a new connection and resent it, whose handshake waited
//     out dial_timeout=500ms, with the error coming from that handshake's i/o timeout; that was
//     measured in a run of its own);
//   - at the same time PG and MySQL are as usual, and fast
func TestClickHouse_SilentAtRuntime_NoDeadlineOnlyReadTimeout_Default300s(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	t.Run("read_timeout written in the DSN", func(t *testing.T) {
		t.Parallel()
		const readTimeout = 2 * time.Second
		chp := harness.NewProxy(t, harness.CHAddr())
		p := chStart(t, harness.Options{Env: map[string]string{"E2E_CH_DSN": harness.CHDSN(chp.Addr()) + "?read_timeout=2s"}})
		chFillPool(t, p, 4)
		chp.SetDelay(time.Hour)

		var pg []faultDepResult
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // while the ClickHouse batch is stuck, PG and MySQL are probed every 100ms
			defer wg.Done()
			for i := range 20 {
				pg = append(pg, faultDep(p, []string{"db", "mysql"}[i%2], "", 5*time.Second))
				time.Sleep(100 * time.Millisecond)
			}
		}()
		res := faultBurst(p, 4, "ch", readTimeout+10*time.Second)
		wg.Wait()
		var took []time.Duration
		for i, r := range res {
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "i/o timeout") {
				t.Errorf("request %d: with the far end silent the query should fail at read_timeout (503, i/o timeout), got %v", i+1, r)
				continue
			}
			if want := readTimeout + chDialTimeout; r.Server < want-20*time.Millisecond || r.Server > want+faultSlack {
				t.Errorf("request %d: it times out reading at read_timeout=%v and then waits out dial_timeout=%v on a new connection, so it should fail at %v, got %v",
					i+1, readTimeout, chDialTimeout, want, r.Server)
			}
			took = append(took, r.Server)
		}
		t.Logf("numbers: ClickHouse silent, concurrency 4, read_timeout=2s, dial_timeout=%v: %s; the error: %s", chDialTimeout, faultSummary(took), res[0].Error)
		var other []time.Duration
		for i, r := range pg {
			if r.Status != http.StatusOK || r.Server > faultUnaffected {
				t.Errorf("attempt %d: PG / MySQL should be as usual, and fast, while ClickHouse is stuck, got %v", i+1, r)
			}
			other = append(other, r.Server)
		}
		t.Logf("numbers: PG / MySQL's SELECT 1 at the same moment: %s", faultSummary(other))
	})

	t.Run("no read_timeout written", func(t *testing.T) {
		t.Parallel()
		const giveUp = 8 * time.Second
		chp := harness.NewProxy(t, harness.CHAddr())
		p := chStart(t, harness.Options{CHAddr: chp.Addr()})
		chp.SetDelay(time.Hour)
		r := chDep(p, "", giveUp)
		if r.ReqErr == nil {
			t.Fatalf("the documentation says read_timeout is 300s by default: with the far end silent and no deadline, the query should not return within %v, got %v", giveUp, r)
		}
		t.Logf("numbers: ClickHouse silent, no deadline, no read_timeout written: the query had still not returned after %v (the driver's default read_timeout being %v)", giveUp, chReadTimeout)
	})
}

// After the header. clickhouse-go v2.48.0 reads a query in two parts: firstBlock reads the header
// and process reads the remaining data blocks in another goroutine, and each part sets a read
// deadline of now + read_timeout when it starts (startReadWriteTimeout in conn_process.go), which
// is never extended. So read_timeout governs "how long the rest of the result may take in all" and
// not "how long without a byte": a healthy query whose remainder takes longer than read_timeout
// fails at read_timeout all the same (measured: a 50-row stream at 100ms a row with
// read_timeout=1s had read 9 rows at 1.0s when it reported i/o timeout).
// The caller's deadline replaces read_timeout (SetDeadline comes after SetReadDeadline and
// overrides it), even when it is longer than read_timeout.
//
// In v2.30.0 it was the other way around: the read deadline was cleared once the header arrived
// and the rest was read without one, so a stream going silent halfway with no deadline given hung
// forever. xgorm/clickhouse/README.md, "Behavior (measured)", writes down what was measured
func TestClickHouse_AfterHeader_ReadTimeoutCoversRest_CallerDeadlineOverrides(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	const (
		readTimeout = time.Second
		stallAt     = 700 * time.Millisecond
		healthy     = 5 * time.Second // 50 rows at 100ms between them
	)
	for _, c := range []struct {
		name    string
		stall   bool
		timeout time.Duration // 0 means no deadline is given
		ok      bool          // all 50 rows were read
		want    time.Duration // the moment it returns
	}{
		{"a_healthy_stream_with_no_deadline", false, 0, false, readTimeout},
		{"a_healthy_stream_with_the_caller_giving_10s", false, 10 * time.Second, true, healthy},
		{"it_stalls_after_the_header_with_no_deadline", true, 0, false, readTimeout},
		{"it_stalls_after_the_header_with_the_caller_giving_3s", true, 3 * time.Second, false, 3 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			chp := harness.NewProxy(t, harness.CHAddr())
			p := chStart(t, harness.Options{Env: map[string]string{"E2E_CH_DSN": harness.CHDSN(chp.Addr()) + "?read_timeout=1s"}})
			path := "/ch/stream?rows=50&ms=100"
			if c.timeout > 0 {
				path += "&timeout=" + c.timeout.String()
			}
			if c.stall {
				time.AfterFunc(stallAt, func() { chp.SetDelay(time.Hour) })
			}
			r := chCall(p, path, 15*time.Second)
			switch {
			case r.ReqErr != nil:
				t.Fatalf("it should return within %v, got %v", 15*time.Second, r.faultDepResult)
			case c.ok && r.Status != http.StatusOK:
				t.Errorf("the caller gave %v and the deadline replaces read_timeout, so the %v stream should be read whole, got %v", c.timeout, healthy, r.faultDepResult)
			case !c.ok && r.Status != http.StatusServiceUnavailable:
				t.Errorf("it should fail with a 503 at %v, got %v", c.want, r.faultDepResult)
			}
			if r.Server < c.want-20*time.Millisecond || r.Server > c.want+faultSlack {
				t.Errorf("it should return at %v, got %v: %s", c.want, r.Server, r.Error)
			}
			t.Logf("numbers: %s with read_timeout=%v: it returned after %s, status=%d: %s", c.name, readTimeout, faultMS(r.Server), r.Status, r.Error)
		})
	}
}

// After a read timeout that connection does not go back to the pool, and the next query gets its
// own result.
//
// This used to be a KNOWN BUG: clickhouse-go v2.30.0's stdDriver decided whether a connection was
// broken in isConnBrokenError with the type assertion err.(*net.OpError), and the read timeout's
// error was wrapped by ch-go, so the assertion missed: the connection went back to the pool and the
// result of that query on the server arrived a little later all the same -- measured
// (read_timeout=1s, a pool of 1 connection), B returned A's result "A/0" successfully. Upstream
// v2.47.0 changed it to errors.As (#1869).
//
// Under v2.48.0 a read timeout reports driver.ErrBadConn, so database/sql resends A:
// maxBadConnRetries=2, so the first two may use a pooled connection and the third is always a new
// one, and all three time out reading. Measured:
//
//	A  a 1.5s scalar subquery with read_timeout=1s  fails after 3.0s: driver: bad connection (the i/o timeout root cause is lost)
//	   system.query_log holds three starts of that query
//	B  SELECT 'B'...                                succeeds and returns "B/0"
func TestClickHouse_ConnNotReturnedAfterReadTimeout_TimedOutQueryResentThrice(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	const (
		readTimeout = time.Second
		attempts    = 3 // database/sql's maxBadConnRetries (2) + the final alwaysNewConn
	)
	p := chStart(t, harness.Options{
		Overlay: chOverlay("MaxOpenConns: 1\nMaxIdleConns: 1"),
		Env:     map[string]string{"E2E_CH_DSN": harness.CHDSN(harness.CHAddr()) + "?read_timeout=1s"},
	})
	tag := "A-" + harness.NewID()
	a := chCall(p, "/ch/echo?tag="+tag+"&ms=1500", 10*time.Second)
	if a.ReqErr != nil || a.Status != http.StatusServiceUnavailable || !strings.Contains(a.Error, "bad connection") {
		t.Fatalf("A sleeps 1.5s on the server with read_timeout=1s: it should time out reading and fail with bad connection after the resends, got %v", a.faultDepResult)
	}
	if want := attempts * readTimeout; a.Server < want-20*time.Millisecond || a.Server > want+faultSlack {
		t.Errorf("A should be sent %d times, each waiting out read_timeout, and fail at %v, got %v", attempts, want, a.Server)
	}
	b := chCall(p, "/ch/echo?tag=B", 10*time.Second)
	if b.Status != http.StatusOK || b.Got != "B/0" {
		t.Errorf("B should get its own result \"B/0\", got %v got=%q", b.faultDepResult, b.Got)
	}
	// A scalar subquery runs while the server analyzes the query, so QueryStart reaches query_log only once it has slept: the last one takes 1.5s longer
	sent := chQueryStarts(t, tag)
	for deadline := time.Now().Add(5 * time.Second); sent < attempts && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		sent = chQueryStarts(t, tag)
	}
	if sent != attempts {
		t.Errorf("A should be resent by database/sql and started %d times on the server, got %d", attempts, sent)
	}
	t.Logf("numbers: A failed after %s (%s) and the server started it %d times; B right after took %s, status=%d got=%q", faultMS(a.Server), a.Error, sent, faultMS(b.Server), b.Status, b.Got)
}

// chQueryStarts is how many times a query whose text holds marker was started according to system.query_log (the one querying query_log aside)
func chQueryStarts(t *testing.T, marker string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := harness.CH(t)
	if _, err := db.ExecContext(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("SYSTEM FLUSH LOGS: %v", err)
	}
	var n uint64
	if err := db.QueryRowContext(ctx, `SELECT count() FROM system.query_log WHERE type = 'QueryStart'
		AND query LIKE ? AND query NOT LIKE '%system.query_log%'`, "%"+marker+"%").Scan(&n); err != nil {
		t.Fatalf("reading system.query_log: %v", err)
	}
	return int(n)
}

// ClickHouse is unreachable at the start, in three ways: connections refused / the far end says
// nothing / the host is down.
//
// docs/config.md and xgorm/README.md: "Connect retries: it retries 3 times when it cannot connect"
// and "for the other drivers it is 2 x DialTimeout", so the start waits 3 x 1s + 3s = 6s at most
// (chStartBudget); the error has to say which instance and which address, and may hold no password
// and no DSN.
//
// The "far end says nothing" case can count the attempts, and what it keeps is the "other drivers"
// section: the version query the driver makes in Initialize was moved into the connect probe and
// is retried with it -- left where it was, it would happen inside gorm.Open and fail after a
// single attempt
func TestClickHouse_UnreachableAtStartup_FailsInBudget_ErrHasNameAddrNoPassword(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pw := harness.CHPassword()
	for _, c := range []struct {
		name   string
		inject func(*harness.Proxy)
		reason string
		count  bool
	}{
		{"connections refused", func(p *harness.Proxy) { p.Cut() }, "connection refused", false},
		{"the far end says nothing", func(p *harness.Proxy) { p.SetDelay(time.Hour) }, "i/o timeout", true},
		{"the host is down", func(p *harness.Proxy) { p.Blackhole() }, "dial tcp", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			chp := harness.NewProxy(t, harness.CHAddr())
			c.inject(chp)
			exit, p := faultStartFails(t, harness.Options{CHAddr: chp.Addr()})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "ch"`, "cannot reach "+chp.Addr(), c.reason)
			mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, harness.CHDSN(chp.Addr()))
			if exit.Uptime > chStartBudget+faultStartSlack {
				t.Errorf("by the documentation the ClickHouse connect validation takes %d attempts x %v plus the backoff bounds %v = %v, and it failed and exited only after %v",
					faultPingAttempts, chAttempt, faultPingBackoffs, chStartBudget, exit.Uptime)
			}
			if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm ready" })); n != 0 {
				t.Errorf("xgorm should not report ready when the ch instance cannot be built, got %d lines", n)
			}
			if c.count && chp.Accepted() != faultPingAttempts {
				t.Errorf("the documentation says it retries %d times when it cannot connect (the version query having moved into the probe); it tried only %d (the error: %s)", faultPingAttempts, chp.Accepted(), lastLine(stderr))
			}
			t.Logf("numbers: at the start, with ClickHouse %s: it exited with %d after %v (budget %v); the proxy saw %d connections; the error: %s",
				c.name, exit.Code, exit.Uptime.Round(time.Millisecond), chStartBudget, chp.Accepted(), lastLine(stderr))
		})
	}
}

// ClickHouse says nothing / the host is down at the start and a SIGTERM arrives.
//
// The MySQL and PG connects give up a few milliseconds after the signal; ClickHouse cannot:
// xgorm/clickhouse/README.md, "Behavior (measured)" -- "it connects with net.DialTimeout, which
// does not look at ctx and is governed by dial_timeout alone; the handshake likewise sets the whole
// connection's deadline from dial_timeout", and "Ping honors a deadline and not a cancellation".
// xgorm waits for its probe on the current goroutine (sql.DB's Close waits for the queries in
// flight, so handing it to another goroutine would not close it either), so after the signal it
// has to wait for this dial or handshake to hit dial_timeout before it exits: the upper bound is
// dial_timeout (500ms by default) and it never sits out a whole round of retries
func TestClickHouse_SIGTERMWhileSilentAtStartup_ExitsWithinOneDialTimeout(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			chp := harness.NewProxy(t, harness.CHAddr())
			m.inject(chp)
			p := chStart(t, harness.Options{CHAddr: chp.Addr(), NoWait: true, Overlay: stopTimeout(5 * time.Second)})
			p.WaitLog(t, 15*time.Second, func(l harness.Log) bool { return l.Msg() == "starting" && l.Str("hook") == "xgorm.initXGorm" })
			time.Sleep(100 * time.Millisecond) // let the first attempt get stuck in the dial or the handshake
			p.Signal(syscall.SIGTERM)
			exit, ok := p.Wait(15 * time.Second)
			if !ok {
				t.Fatalf("a SIGTERM arrived during the start and the process is still alive 15s later\n%s", lastLines(p.Output(), 30))
			}
			if ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" }); len(ls) > 0 {
				t.Errorf("the service should not start once a signal arrived during the start, and the log holds %s", ls[0].Line)
			}
			if exit.SinceSignal > chDialTimeout+faultSlack {
				t.Errorf("the dial and the handshake are bounded by dial_timeout (%v), which is the most it may wait after the signal, got %v", chDialTimeout, exit.SinceSignal.Round(time.Millisecond))
			}
			t.Logf("numbers: at the start, with ClickHouse %s: it exited %v after the signal (dial_timeout %v, the whole round's budget %v); the last line of stderr: %s",
				m.name, exit.SinceSignal.Round(time.Millisecond), chDialTimeout, chStartBudget, lastLine(p.Stderr()))
		})
	}
}
