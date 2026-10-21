package e2e

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// mysqlDriverLogLine is a line written by go-sql-driver's own logger (the standard library log, with the prefix [mysql])
var mysqlDriverLogLine = regexp.MustCompile(`(?m)^\[mysql\] \d{4}/\d{2}/\d{2} .*$`)

// The MySQL process dies while running (the port refuses connections):
//
//   - an operation using MySQL fails there and then (a refused connection waits for no timeout),
//     with the address in the error
//   - the other instance (PG) and the endpoints that do not touch MySQL are unaffected: several
//     instances means several pools, each its own
//   - once MySQL is back it recovers without a restart
func TestMySQL_RefusedAtRuntime_FailsFast_PGUnaffected_Recovers(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	my := harness.NewProxy(t, harness.MySQLAddr())
	p := harness.Start(t, harness.Options{MySQLAddr: my.Addr()})
	u, _ := mysqlCreate(t, p, "before-cut", "cut@example.com")

	my.Cut()

	t.Run("an operation using MySQL fails there and then", func(t *testing.T) {
		var took []time.Duration
		for i := range 5 {
			r := mysqlDep(p, "", 30*time.Second)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("attempt %d: /dep?target=mysql should answer 503 while MySQL refuses connections, got %v", i+1, r)
			}
			if !strings.Contains(r.Error, "connection refused") || !strings.Contains(r.Error, my.Addr()) {
				t.Errorf("attempt %d: the error should say what it cannot reach (%s, connection refused), got %q", i+1, my.Addr(), r.Error)
			}
			// A refused connection is immediate, and DialTimeout is the upper bound: waiting out even one timeout would pass it
			if r.Server > mysqlDialTimeout {
				t.Errorf("attempt %d: a refused connection should wait for no timeout, got %v", i+1, r.Server)
			}
			took = append(took, r.Server)
		}
		t.Logf("numbers: SELECT 1 took %v in turn while MySQL refused connections; %s", took, faultSummary(took))

		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, fmt.Sprintf("/mysql/users/%d", u.ID), nil},
			{http.MethodPost, "/mysql/users", map[string]string{"name": "while-cut"}},
			{http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": "while-cut"}},
			{http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil},
		} {
			r, took := faultTimed(t, p, c.method, c.path, c.body)
			if r.Status != http.StatusInternalServerError {
				t.Errorf("%s %s should answer 500 while MySQL is unavailable, got %v", c.method, c.path, r)
			}
			if took > mysqlDialTimeout {
				t.Errorf("%s %s should fail there and then while MySQL refuses connections, and it took %v", c.method, c.path, took)
			}
		}
	})

	t.Run("PG and the endpoints that do not touch MySQL are unaffected", func(t *testing.T) {
		faultQuick(t, p, "GET /ping", "/ping", http.StatusOK)
		faultQuick(t, p, "GET /dep?target=db", "/dep?target=db", http.StatusOK)
		pu := createUser(t, p, "pg-while-mysql-cut", "pg@example.com")
		faultQuick(t, p, "GET /users/:id?cache=off (PG)", fmt.Sprintf("/users/%d?cache=off", pu.ID), http.StatusOK)
	})

	t.Run("it recovers by itself once MySQL is back", func(t *testing.T) {
		my.Restore()
		took, n := mysqlWaitDep(t, p, 10*time.Second)
		t.Logf("numbers: SELECT 1 was back to 200 %s after MySQL started listening again (probe %d)", faultMS(took), n)
		fresh, _ := mysqlCreate(t, p, "after", "after@example.com")
		if r := p.Get(t, fmt.Sprintf("/mysql/users/%d", fresh.ID)); r.Status != http.StatusOK {
			t.Errorf("reads and writes should work as usual after the recovery, got %v", r)
		}
	})
}

// MySQL says nothing while running (SetDelay(time.Hour)) and the caller gives no deadline:
// xgorm/README.md, XGorm.MySQL.ReadTimeout, "the read timeout, matching readTimeout in the DSN. 3s
// by default" -- every connection in the pool is stuck on a read, a query fails at ReadTimeout and
// does not hang forever; the same holds under concurrency.
// At the same time the PG instance is as usual, and fast: the two instances' pools do not drag
// each other down.
//
// And one more: on a read timeout go-sql-driver writes a line of plain text to stderr with its own
// logger ([mysql] 2026/09/24 packets.go:58 read tcp ...: i/o timeout). In docs/architecture.md,
// "Where we differ from the underlying library on purpose", the loggers of go-redis, resty and
// GORM are all wired into slog -- "a log that goes around slog never reaches the logging
// platform"; go-sql-driver's is not, which by the same principle is a bug
func TestMySQL_SilentAtRuntime_QueryFailsAtReadTimeout_PGUnaffected(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const conc = 8
	my := harness.NewProxy(t, harness.MySQLAddr())
	p := harness.Start(t, harness.Options{MySQLAddr: my.Addr()})
	// Fill the pool to conc connections first: once it is stuck, each concurrent query holds one connection of the pool
	for _, r := range faultBurst(p, conc, "mysql", 10*time.Second) {
		if r.Status != http.StatusOK {
			t.Fatalf("SELECT 1 should answer 200 before the fault, got %v", r)
		}
	}
	my.SetDelay(time.Hour)

	var pg []faultDepResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // while the MySQL batch is stuck, PG is probed every 100ms
		defer wg.Done()
		for range 20 {
			pg = append(pg, faultDep(p, "db", "", 5*time.Second))
			time.Sleep(100 * time.Millisecond)
		}
	}()
	res := faultBurst(p, conc, "mysql", mysqlReadTimeout+10*time.Second)
	wg.Wait()

	var took []time.Duration
	for i, r := range res {
		if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
			t.Errorf("request %d: with MySQL silent the query should fail at ReadTimeout and answer 503, got %v", i+1, r)
			continue
		}
		if r.Server > mysqlReadTimeout+faultSlack || r.Server < mysqlReadTimeout-20*time.Millisecond {
			t.Errorf("request %d: the documentation says MySQL.ReadTimeout is 3s by default and the query should fail then, got %v: %s", i+1, r.Server, r.Error)
		}
		took = append(took, r.Server)
	}
	t.Logf("numbers: MySQL silent, concurrency %d, no deadline: %s; the error: %s", conc, faultSummary(took), res[0].Error)

	var pgTook []time.Duration
	for i, r := range pg {
		if r.Status != http.StatusOK || r.Server > faultUnaffected {
			t.Errorf("attempt %d: the PG instance should be as usual, and fast, while MySQL is stuck, got %v", i+1, r)
		}
		pgTook = append(pgTook, r.Server)
	}
	t.Logf("numbers: PG's SELECT 1 at the same moment: %s", faultSummary(pgTook))

	my.SetDelay(0)
	took2, n := mysqlWaitDep(t, p, 10*time.Second)
	t.Logf("numbers: it was back to 200 %s after MySQL started answering again (probe %d)", faultMS(took2), n)

	t.Run("go-sql-driver's own log goes through slog as well", func(t *testing.T) {
		// xgorm/README.md, XGorm: go-sql-driver's own log is wired into slog as one xgorm go-sql-driver log line at WARN, with the text in detail.
		// This used to be a KNOWN BUG: xgorm never called mysql.SetLogger, so the driver used its own log.New(os.Stderr, "[mysql] ", ...),
		// and the 8 read timeouts of this case were 8 [mysql] ... packets.go:58 read tcp ...: i/o timeout lines in stderr
		if lines := mysqlDriverLogLine.FindAllString(p.Stderr(), -1); len(lines) > 0 {
			t.Errorf("go-sql-driver's log should go through slog, and stderr holds %d lines of plain text, for instance: %s", len(lines), lines[0])
		}
		ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm go-sql-driver log" })
		if len(ls) == 0 {
			t.Fatalf("the driver logs on a read timeout and it should be an xgorm go-sql-driver log, and there is not one\n%s", lastLines(p.Stderr(), 10))
		}
		for _, l := range ls {
			if l.Str("level") != "WARN" || !strings.Contains(l.Str("detail"), "packets.go") {
				t.Errorf("the driver's log should be WARN with the text in detail, got %s", l.Line)
				break
			}
		}
		t.Logf("numbers: %d xgorm go-sql-driver log lines, for instance: %s", len(ls), ls[0].Str("detail"))
	})
}

// MySQL is unreachable at the start, in three ways: connections refused / the far end says nothing
// / the host is down.
//
// docs/config.md and xgorm/README.md: "Connect retries: it retries 3 times when it cannot connect"
// and "the budget of one connect probe: for MySQL it is DialTimeout + MySQL.ReadTimeout", so the
// start waits 3 x 3.5s + 3s = 13.5s at most (mysqlStartBudget); the error has to say which
// instance and which address it cannot reach, and may hold no password and no DSN.
//
// The PG instance is fine, and its name comes first so it is built first (xclient.Build goes by
// name): when MySQL will not come up it has to be closed again, and the process exits with a
// failure all the same
func TestMySQL_UnreachableAtStartup_FailsInBudget_ErrHasNameAddrNoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := harness.MySQLPassword()
	for _, c := range []struct {
		name   string
		inject func(*harness.Proxy)
		reason string
		count  bool // the proxy can count the attempts (only "the far end says nothing" reaches the proxy at all)
	}{
		{"connections refused", func(p *harness.Proxy) { p.Cut() }, "connection refused", false},
		{"the far end says nothing", func(p *harness.Proxy) { p.SetDelay(time.Hour) }, "invalid connection", true},
		{"the host is down", func(p *harness.Proxy) { p.Blackhole() }, "i/o timeout", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			my := harness.NewProxy(t, harness.MySQLAddr())
			c.inject(my)
			exit, p := faultStartFails(t, harness.Options{MySQLAddr: my.Addr()})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "mysql"`, my.Addr(), c.reason)
			mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, harness.MySQLDSN(my.Addr()))
			if exit.Uptime > mysqlStartBudget+faultStartSlack {
				t.Errorf("by the documentation the MySQL connect validation takes %d attempts x %v plus the backoff bounds %v = %v, and it failed and exited only after %v",
					faultPingAttempts, mysqlAttempt, faultPingBackoffs, mysqlStartBudget, exit.Uptime)
			}
			// PG was built first and has to be closed when MySQL fails: xgorm connected holds the default line alone, and there is no xgorm ready
			if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm ready" })); n != 0 {
				t.Errorf("xgorm should not report ready when the mysql instance cannot be built, got %d lines", n)
			}
			t.Logf("numbers: at the start, with MySQL %s: it exited with %d after %v (budget %v); the proxy saw %d connections; the error: %s",
				c.name, exit.Code, exit.Uptime.Round(time.Millisecond), mysqlStartBudget, my.Accepted(), lastLine(stderr))

			// This used to be a KNOWN BUG: the proxy saw 1 connection alone and the process exited after 3.0s (= ReadTimeout, the handshake read),
			// with the error open <addr> failed: invalid connection. GORM's MySQL Dialector queries SELECT VERSION() first in
			// Initialize inside gorm.Open (gorm.io/driver/mysql v1.6.0 mysql.go:130), and when that fails gorm.Open returns the error
			// outright, so xgorm.New never reaches the ping with the retries behind it
			if c.count && my.Accepted() != faultPingAttempts {
				t.Errorf("the documentation says it retries %d times when it cannot connect; MySQL tried only %d (the error: %s)", faultPingAttempts, my.Accepted(), lastLine(stderr))
			}
		})
	}
}

// MySQL refuses these credentials at the start: docs/behavior.md, "The connect probe at start" --
// an authentication failure is not retried, and the error says it is an authentication failure and
// not that it cannot connect.
// Neither the right password nor the wrong one may appear in the output. What the server answers
// (measured on MySQL 8.0.46):
//
//	a wrong password       Error 1045 (28000): Access denied for user 'xone'@... (a user that does not exist is 1045 as well)
//	no access to the database  Error 1044 (42000): Access denied for user 'xone'@... to database '...'
//	                       (where the database does not exist, an account without global privileges is given 1044 as well)
//
// This used to be a KNOWN BUG: xgorm recognized PG's SQLSTATE class 28 alone, while MySQL's
// authentication error comes out of gorm.Open (Dialector.Initialize querying the version being the
// first connect), and it was reported as open <address> failed: Error 1045 ...
func TestMySQL_BadPasswordOrNoDBAccessAtStartup_AuthFail_NoRetry_NoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := harness.MySQLPassword()
	for _, c := range []struct {
		name string
		dsn  func(addr, wrong string) string
		code string
	}{
		{"a wrong password", mysqlSecretDSN, "Error 1045"},
		{"no access to the database", func(a, _ string) string { return mysqlSecretDSN(a, pw) + "_noaccess_" + harness.NewID() }, "Error 1044"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wrong := "wrong-pw-" + harness.NewID()
			my := harness.NewProxy(t, harness.MySQLAddr()) // only to count the connections
			dsn := c.dsn(my.Addr(), wrong)
			exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_MYSQL_DSN": dsn}})

			stderr := p.Stderr()
			mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, wrong, dsn)
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "mysql"`,
				"authentication to "+my.Addr()+" failed", c.code, "Access denied for user 'xone'")
			for _, s := range []string{"cannot reach", "open " + my.Addr() + " failed"} {
				if strings.Contains(stderr, s) {
					t.Errorf("an authentication failure is not a failure to connect and should not report %q: %s", s, lastLine(stderr))
				}
			}
			if exit.Uptime > faultStartSlack {
				t.Errorf("an authentication failure is not retried and should exit within %v, got %v", faultStartSlack, exit.Uptime)
			}
			if my.Accepted() != 1 {
				t.Errorf("an authentication failure is not retried and the proxy should see 1 connection, got %d", my.Accepted())
			}
			t.Logf("numbers: at the start, with MySQL %s: it exited with %d after %v in %d attempts; the error: %s", c.name, exit.Code, exit.Uptime.Round(time.Millisecond), my.Accepted(), lastLine(stderr))
		})
	}
}
