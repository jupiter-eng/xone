package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The budget of the connect validation at the start.
//
// docs/behavior.md, "The connect probe at start": it retries 3 times when it cannot connect, and
// the wait between two of them doubles each time with jitter; xutil.Retry's comment: "the whole
// round has one budget (attempts x timeout plus the upper bounds of the backoffs)".
// The first interval is 1s (docs/behavior.md, "The connect probe at start", which is pingInterval
// in xgorm.go / xredis.go), so the two backoff bounds are 1s and 2s. The timeout of one attempt:
//
//	XGorm (PostgreSQL) "the injected connect_timeout (DialTimeout rounded up to whole seconds) + DialTimeout, 1.5s by default"
//	XRedis             pingTimeout in xredis.go, "a connect plus one round trip" = DialTimeout + ReadTimeout = 1s
const (
	faultPingAttempts     = 3
	faultPingBackoffs     = time.Second + 2*time.Second
	faultPGAttempt        = 1500 * time.Millisecond
	faultPGConnect        = time.Second                                                   // the injected connect_timeout
	faultPGStartBudget    = faultPingAttempts*faultPGAttempt + faultPingBackoffs          // 7.5s
	faultRedisStartBudget = faultPingAttempts*faultRedisAttemptBudget + faultPingBackoffs // 6s
)

// faultStartSlack is what starting and stopping the process itself costs: with a malformed
// configuration the whole process is out in 16 to 20ms (measured on an idle machine here).
// But Uptime counts from the exec, and with the whole e2e suite running in parallel, merely
// getting the process up takes a while: on a GitHub runner (4 cores, -parallel=16) the ClickHouse
// wrong-password case exited only after 1.075s, with the process's own logs from reading the
// configuration to exiting taking 140ms of it.
// 2s leaves twice that and stays far under every budget under test (6s at the least).
// "It does not retry" rests on counting the connections the proxy saw and not on this duration:
// the first retry backs off 1s, which falls inside the slack and is still caught by the connection
// count
const faultStartSlack = 2 * time.Second

// faultStartFails starts a process that is bound to fail and waits for it: it has to exit non-zero and never start listening
func faultStartFails(t *testing.T, o harness.Options) (harness.Exit, *harness.Process) {
	t.Helper()
	o.NoWait = true
	p := harness.Start(t, o)
	exit, ok := p.Wait(30 * time.Second)
	if !ok {
		t.Fatalf("the start should fail and exit, and it is still running after 30s\n%s", p.Output())
	}
	if exit.Code == 0 || exit.Signal != nil {
		t.Fatalf("the start should fail (a non-zero exit), got %v\n%s", exit, p.Output())
	}
	if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" })) != 0 {
		t.Errorf("the service should not start listening when a dependency will not come up, and xgin listening turned up")
	}
	return exit, p
}

// faultMustContain: text has to hold every one of the fragments
func faultMustContain(t *testing.T, where, text string, parts ...string) {
	t.Helper()
	for _, s := range parts {
		if !strings.Contains(text, s) {
			t.Errorf("%s should hold %q, got:\n%s", where, s, text)
		}
	}
}

// PG is unreachable at the start, in three ways:
//
//	connections refused     the process died and nothing listens on the port: every attempt fails at once and the total is the two backoffs alone
//	the far end says nothing  TCP connects and startup is never answered: every attempt waits out connect_timeout (1s)
//	the host is down        the SYN is never answered: the same, waiting out connect_timeout
//
// All of them have to fail within the budget (faultPGStartBudget, 7.5s); the error is written to
// stderr by MustRun and has to say which address it cannot reach (docs/observability.md, "The
// framework's own logs"), and may hold no password and no DSN
func TestFault_PGUnreachableAtStartup_FailsInBudget_ErrHasAddrNoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	for _, c := range []struct {
		name     string
		inject   func(*harness.Proxy)
		lo       time.Duration // this long at the least: every attempt waits out connect_timeout
		reason   string
		attempts bool // the proxy can count the attempts (of the two that reach it, "the far end says nothing" alone)
	}{
		{"connections refused", func(p *harness.Proxy) { p.Cut() }, 0, "connection refused", false},
		{"the far end says nothing", func(p *harness.Proxy) { p.SetDelay(time.Hour) }, faultPingAttempts * faultPGConnect, "timeout", true},
		{"the host is down", func(p *harness.Proxy) { p.Blackhole() }, faultPingAttempts * faultPGConnect, "timeout", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pg := harness.NewProxy(t, harness.PGAddr())
			c.inject(pg)
			exit, p := faultStartFails(t, harness.Options{PGAddr: pg.Addr()})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", "cannot reach "+pg.Addr(), c.reason)
			mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, harness.PGDSN(pg.Addr()))

			if exit.Uptime > faultPGStartBudget+faultStartSlack {
				t.Errorf("by the documentation the PG connect validation takes %d attempts x %v plus the backoff bounds %v = %v, and it failed and exited only after %v",
					faultPingAttempts, faultPGAttempt, faultPingBackoffs, faultPGStartBudget, exit.Uptime)
			}
			if exit.Uptime < c.lo {
				t.Errorf("while %s every attempt should wait out connect_timeout (%v), so %d attempts take %v at least, and it exited after %v: it did not make the documented 3 attempts, or did not wait the timeouts out",
					c.name, faultPGConnect, faultPingAttempts, c.lo, exit.Uptime)
			}
			if c.attempts && pg.Accepted() != faultPingAttempts {
				t.Errorf("the documentation says the connect probe makes %d attempts at most, and the proxy saw %d connections", faultPingAttempts, pg.Accepted())
			}
			t.Logf("numbers: at the start, with PG %s: it exited with %d after %v (budget %v); the proxy saw %d connections; the error: %s",
				c.name, exit.Code, exit.Uptime.Round(time.Millisecond), faultPGStartBudget, pg.Accepted(), lastLine(stderr))
		})
	}
}

// A wrong PG password at the start: docs/behavior.md, "The connect probe at start" -- an
// authentication failure is not retried, and the error says it is an authentication failure and not
// that it cannot connect; neither the right password nor the wrong one may appear in the output
func TestFault_PGBadPasswordAtStartup_ErrSaysAuthFailed_NoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	wrong := "wrong-pw-" + harness.NewID()
	pg := harness.NewProxy(t, harness.PGAddr()) // only to count the connections
	dsn := strings.Replace(harness.PGDSN(pg.Addr()), ":"+pw+"@", ":"+wrong+"@", 1)
	exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": dsn}})

	stderr := p.Stderr()
	faultMustContain(t, "stderr", stderr, "xgorm connect failed", "authentication to "+pg.Addr()+" failed", `password authentication failed for user "xone"`)
	mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, wrong, dsn)
	if strings.Contains(stderr, "cannot reach") {
		t.Errorf("an authentication failure is not a failure to connect and should not report cannot reach:\n%s", stderr)
	}
	// One attempt means no backoff to wait out: it should exit within what starting and stopping the process costs
	if exit.Uptime > faultStartSlack {
		t.Errorf("an authentication failure is not retried and should exit within %v, got %v", faultStartSlack, exit.Uptime)
	}
	if pg.Accepted() != 1 {
		t.Errorf("an authentication failure is not retried and the proxy should see 1 connection, got %d", pg.Accepted())
	}
	t.Logf("numbers: at the start, with a wrong PG password: it exited with %d after %v in %d attempts; the error: %s", exit.Code, exit.Uptime.Round(time.Millisecond), pg.Accepted(), lastLine(stderr))
}

// Redis is unreachable at the start. A password is configured (a random string: it cannot connect
// anyway and no server will check it), and the error has to hold the address and not it.
// xredis/README.md, XRedis.Password: "this module writes it into no log at all".
// The budget is faultRedisStartBudget (6s): 3 attempts x (DialTimeout + ReadTimeout) plus the two
// backoff bounds
func TestFault_RedisUnreachableAtStartup_FailsInBudget_ErrHasAddrNoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	for _, c := range []struct {
		name   string
		inject func(*harness.Proxy)
		lo     time.Duration
		reason string
	}{
		// No lower bound for connections refused: every attempt fails there and then, and the total is the two jittered backoffs alone
		{"connections refused", func(p *harness.Proxy) { p.Cut() }, 0, "connection refused"},
		// The far end says nothing: every attempt waits out ReadTimeout at least (the handshake)
		{"the far end says nothing", func(p *harness.Proxy) { p.SetDelay(time.Hour) }, faultPingAttempts * faultRedisReadTimeout, "i/o timeout"},
		// The host is down: every attempt waits out one Ping's timeout, DialTimeout + ReadTimeout
		{"the host is down", func(p *harness.Proxy) { p.Blackhole() }, faultPingAttempts * faultRedisAttemptBudget, "deadline exceeded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			secret := "redis-secret-" + harness.NewID()
			rp := harness.NewProxy(t, harness.RedisAddr())
			c.inject(rp)
			exit, p := faultStartFails(t, harness.Options{RedisAddr: rp.Addr(), Overlay: fmt.Sprintf("XRedis:\n  Password: %q\n", secret)})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xredis connect failed", "cannot reach "+rp.Addr(), c.reason)
			mustNotContain(t, "the process's stdout / stderr", p.Output(), secret)
			if exit.Uptime > faultRedisStartBudget+faultStartSlack {
				t.Errorf("by the documentation the Redis connect validation takes %d attempts x %v plus the backoff bounds %v = %v, and it failed and exited only after %v",
					faultPingAttempts, faultRedisAttemptBudget, faultPingBackoffs, faultRedisStartBudget, exit.Uptime)
			}
			if exit.Uptime < c.lo {
				t.Errorf("while %s, %d attempts take %v at least, and it exited after %v", c.name, faultPingAttempts, c.lo, exit.Uptime)
			}
			t.Logf("numbers: at the start, with Redis %s: it exited with %d after %v (budget %v); the error: %s",
				c.name, exit.Code, exit.Uptime.Round(time.Millisecond), faultRedisStartBudget, lastLine(stderr))
		})
	}
}

// A wrong Redis password at the start: xredis/README.md, XRedis -- "an authentication failure
// (WRONGPASS / NOAUTH) is reported as authentication to <address> failed and is not retried", with
// the server's own text in the error and no credential in it.
//
// "It is not retried" is counted in connections: it reaches Redis through the proxy with no warmup
// (MinIdleConns: 0), so every attempt of the connect validation is a new connection (go-redis
// closes a connection that failed to authenticate there and then) and the proxy should see 1
// alone; retried it would be 3
func TestFault_RedisBadPasswordAtStartup_ErrSaysAuthFailed_NoRetry_NoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	acl, pw := faultRedisACLUser(t)
	wrong := "wrong-" + harness.NewID()
	rp := harness.NewProxy(t, harness.RedisAddr())
	exit, p := faultStartFails(t, harness.Options{RedisAddr: rp.Addr(),
		Overlay: fmt.Sprintf("XRedis:\n  Username: %s\n  Password: %q\n  MinIdleConns: 0\n", acl, wrong)})
	stderr := p.Stderr()
	faultMustContain(t, "stderr", stderr, "xredis connect failed", "authentication to "+rp.Addr()+" failed", "WRONGPASS")
	if strings.Contains(stderr, "cannot reach") {
		t.Errorf("an authentication failure should not be reported as a failure to connect, got: %s", lastLine(stderr))
	}
	mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, wrong)
	if n := rp.Accepted(); n != 1 {
		t.Errorf("a wrong password stays wrong on a retry: the connect validation should make 1 attempt (1 connection), and the proxy saw %d", n)
	}
	if exit.Uptime > faultStartSlack {
		t.Errorf("no retry means no backoff: it should fail and exit within %v, got %v", faultStartSlack, exit.Uptime)
	}
	t.Logf("numbers: at the start, with a wrong Redis password: it exited with %d after %v and the proxy saw %d connections; the error: %s",
		exit.Code, exit.Uptime.Round(time.Millisecond), rp.Accepted(), lastLine(stderr))
}

// A regression: the key=value spelling with spaces around password's equals sign, together with
// another malformed parameter.
//
// xgorm/README.md, XGorm: "pgx's own error text is the whole DSN and masks the tidy password=x
// spelling alone (measured on v5.10.0, where password = hunter2 is carried out as it is), so it is
// not handed back"; the pre-check uses pgx.ParseConfig, "which checks default_query_exec_mode,
// statement_cache_capacity and description_cache_capacity beyond what pgconn.ParseConfig does, and
// a malformed one of those three is reported at the pre-check as well, in an error that carries no
// DSN".
// What the previous round fixed was exactly password = hunter2 together with
// default_query_exec_mode=bogus: the password went into the error as it was.
//
// A failed pre-check is a configuration error, which should neither connect nor retry: it should
// exit at once (16 to 20ms measured here).
// The other two cases parse the DSN and fail to connect (a wrong password, nothing listening on the
// port), which go through pgx's connect errors and may carry no password either
func TestFault_DSNPasswordWithSpacedEqualsAndBadParam_FailsWithoutPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	secret := "hunter2-" + harness.NewID()
	base := "host=127.0.0.1 port=" + strings.Split(harness.PGAddr(), ":")[1] + " user=xone dbname=xone_e2e"
	parse := []struct{ name, dsn string }{
		{"a malformed default_query_exec_mode (the previous round's bug)", base + " password = " + secret + " default_query_exec_mode=bogus"},
		{"a statement_cache_capacity that is not a number", base + " password = " + secret + " statement_cache_capacity=abc"},
		{"a description_cache_capacity that is not a number", base + " password = " + secret + " description_cache_capacity=abc"},
		{"a malformed sslmode", base + " password = " + secret + " sslmode=bogus"},
		{"a connect_timeout that is not a number", base + " password = " + secret + " connect_timeout=abc"},
		{"a port that is not a number", "host=127.0.0.1 port=notaport user=xone dbname=xone_e2e password = " + secret},
		{"a space after the equals sign alone", base + " password= " + secret + " default_query_exec_mode=bogus"},
		{"a space before the equals sign alone", base + " password =" + secret + " default_query_exec_mode=bogus"},
		{"quoted, with a space inside the password", base + " password = '" + secret + " x' default_query_exec_mode=bogus"},
		{"quoted, with an escaped single quote inside the password", base + ` password = 'it\'s-` + secret + `' default_query_exec_mode=bogus`},
		{"a malformed pgx-only parameter in the URL spelling", "postgres://xone:" + secret + "@" + harness.PGAddr() + "/xone_e2e?sslmode=disable&default_query_exec_mode=bogus"},
	}
	for _, c := range parse {
		t.Run("the pre-check fails: "+c.name, func(t *testing.T) {
			t.Parallel()
			exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": c.dsn}})
			stderr := p.Stderr()
			mustNotContain(t, "the process's stdout / stderr", p.Output(), secret)
			faultMustContain(t, "stderr", stderr, "xgorm config failed", "failed to parse DSN")
			if exit.Uptime > faultStartSlack {
				t.Errorf("a failed DSN pre-check is a configuration error, which should neither connect nor retry and should exit at once, and it took %v", exit.Uptime)
			}
			t.Logf("numbers: %s: it exited with %d after %v; the error: %s", c.name, exit.Code, exit.Uptime.Round(time.Millisecond), lastLine(stderr))
		})
	}

	t.Run("it parses and the password is wrong", func(t *testing.T) {
		t.Parallel()
		exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": base + " sslmode=disable password = " + secret}})
		mustNotContain(t, "the process's stdout / stderr", p.Output(), secret)
		faultMustContain(t, "stderr", p.Stderr(), "xgorm connect failed", "password authentication failed")
		t.Logf("numbers: the DSN parses and the password is wrong: it exited with %d after %v; the error: %s", exit.Code, exit.Uptime.Round(time.Millisecond), lastLine(p.Stderr()))
	})

	t.Run("it parses and nothing listens on the port", func(t *testing.T) {
		t.Parallel()
		addr := fmt.Sprintf("127.0.0.1:%d", harness.FreePort(t))
		dsn := "host=127.0.0.1 port=" + strings.Split(addr, ":")[1] + " user=xone dbname=xone_e2e sslmode=disable password = " + secret
		exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": dsn}})
		mustNotContain(t, "the process's stdout / stderr", p.Output(), secret)
		faultMustContain(t, "stderr", p.Stderr(), "xgorm connect failed", "cannot reach "+addr, "connection refused")
		t.Logf("numbers: the DSN parses and nothing listens on the port: it exited with %d after %v; the error: %s", exit.Code, exit.Uptime.Round(time.Millisecond), lastLine(p.Stderr()))
	})
}
