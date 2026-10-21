package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// ClickHouse refuses these credentials at the start, over the native protocol:
// xgorm/clickhouse/README.md, "Configuration" -- "a wrong password is not retried and is reported
// as authentication to <addr> failed: what is recognized is the *clickhouse.Exception the server
// answers during the native protocol handshake, error code 516 (AUTHENTICATION_FAILED, which a
// newer server answers both for a wrong password and for a user that does not exist)".
// Neither the right password nor the wrong one may appear in the output. What the server answers
// (measured on 24.8.14):
//
//	a wrong password / no such user  code: 516, message: <user>: Authentication failed: password is incorrect, or there is no user with such name.
//
// A database that does not exist is not an authentication failure (code: 81 UNKNOWN_DATABASE): it
// is retried as usual and reported as cannot reach.
// The error is mostly code: 81 and now and then driver: bad connection (xgorm/clickhouse/README.md,
// "Authentication failure": the connection check the driver makes before the ping read the server's
// exception away), and both are accepted; the latter turns up while the whole e2e suite runs
// concurrently
func TestClickHouse_BadPasswordOrUnknownUserAtStartup_NativeAuthFail_NoRetry(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pw := harness.CHPassword()
	for _, c := range []struct {
		name string
		dsn  func(addr, wrong string) string
		user string
	}{
		{"a wrong password", func(a, w string) string { return harness.CHDSNWith("clickhouse", a, w) }, "xone"},
		{"no such user", func(a, w string) string {
			return strings.Replace(harness.CHDSNWith("clickhouse", a, w), "xone:", "nobody_"+harness.NewID()[:6]+":", 1)
		}, "nobody_"},
		{"a wrong password over the tcp scheme", func(a, w string) string { return harness.CHDSNWith("tcp", a, w) }, "xone"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wrong := "wrong-pw-" + harness.NewID()
			chp := harness.NewProxy(t, harness.CHAddr()) // only to count the connections
			dsn := c.dsn(chp.Addr(), wrong)
			exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": dsn}})

			stderr := p.Stderr()
			mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, wrong, dsn)
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "ch"`,
				"authentication to "+chp.Addr()+" failed", "code: 516", c.user)
			if strings.Contains(stderr, "cannot reach") {
				t.Errorf("an authentication failure is not a failure to connect and should not report cannot reach: %s", lastLine(stderr))
			}
			if exit.Uptime > faultStartSlack {
				t.Errorf("an authentication failure is not retried and should exit within %v, got %v", faultStartSlack, exit.Uptime)
			}
			if chp.Accepted() != 1 {
				t.Errorf("an authentication failure is not retried and the proxy should see 1 connection, got %d", chp.Accepted())
			}
			t.Logf("numbers: at the start, with ClickHouse %s: it exited with %d after %v in %d attempts; the error: %s", c.name, exit.Code, exit.Uptime.Round(time.Millisecond), chp.Accepted(), lastLine(stderr))
		})
	}

	t.Run("a database that does not exist is no authentication failure", func(t *testing.T) {
		t.Parallel()
		chp := harness.NewProxy(t, harness.CHAddr())
		dsn := strings.Replace(harness.CHDSN(chp.Addr()), "/xone_e2e", "/nodb_"+harness.NewID(), 1)
		exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": dsn}})
		stderr := p.Stderr()
		mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, dsn)
		faultMustContain(t, "stderr", stderr, "cannot reach "+chp.Addr())
		if !strings.Contains(stderr, "code: 81") && !strings.Contains(stderr, "driver: bad connection") {
			t.Errorf("a database that does not exist should report the server's code: 81 (now and then driver: bad connection), got: %s", lastLine(stderr))
		}
		if strings.Contains(stderr, "authentication to") {
			t.Errorf("a database that does not exist is no authentication failure: %s", lastLine(stderr))
		}
		if chp.Accepted() != faultPingAttempts {
			t.Errorf("an error that is not recognized is retried as usual and the proxy should see %d connections, got %d", faultPingAttempts, chp.Accepted())
		}
		if exit.Uptime > chStartBudget+faultStartSlack {
			t.Errorf("it should fail within the connect budget of %v, got %v", chStartBudget, exit.Uptime)
		}
		t.Logf("numbers: a database that does not exist: it exited after %v in %d attempts; the error: %s", exit.Uptime.Round(time.Millisecond), chp.Accepted(), lastLine(stderr))
	})
}

// A wrong password over the HTTP protocol (http://...:8123): xgorm/clickhouse/README.md,
// "Configuration" -- "it is recognized over the HTTP protocol as well".
// clickhouse-go v2.48.0 parses a response that is not 200 into a *clickhouse.HTTPError wrapping a
// *clickhouse.Exception (conn_http_errors.go, with the error code taken from the
// X-ClickHouse-Exception-Code header), which errors.As reaches. Measured (ClickHouse 24.8.14):
//
//	failed to query server hello: ...: sendQuery: [HTTP 403] code: 516, message: xone: Authentication failed: ... (AUTHENTICATION_FAILED) ...
//
// The status code is 403 (not 401). In v2.30.0 this was a piece of assembled text with no Exception
// in the error chain, which was not recognized: it tried all 3 times and reported cannot reach.
// The password still stays out of the output (the error text does not hold it)
func TestClickHouse_BadPasswordOverHTTPAtStartup_AuthFail_NoRetry_NoPassword(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pw := harness.CHPassword()
	wrong := "wrong-pw-" + harness.NewID()
	chp := harness.NewProxy(t, harness.CHHTTPAddr())
	dsn := harness.CHDSNWith("http", chp.Addr(), wrong)
	exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": dsn}})
	stderr := p.Stderr()
	mustNotContain(t, "the process's stdout / stderr", p.Output(), pw, wrong, dsn)
	faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "ch"`,
		"authentication to "+chp.Addr()+" failed", "[HTTP 403] code: 516", "AUTHENTICATION_FAILED")
	if strings.Contains(stderr, "cannot reach") {
		t.Errorf("an authentication failure is not a failure to connect and should not report cannot reach: %s", lastLine(stderr))
	}
	if exit.Uptime > faultStartSlack {
		t.Errorf("an authentication failure is not retried and should exit within %v, got %v", faultStartSlack, exit.Uptime)
	}
	if chp.Accepted() != 1 {
		t.Errorf("an authentication failure is not retried and the proxy should see 1 connection, got %d", chp.Accepted())
	}
	t.Logf("numbers: a wrong password over the HTTP protocol: it exited with %d after %v in %d attempts; the error: %s", exit.Code, exit.Uptime.Round(time.Millisecond), chp.Accepted(), grepLines(stderr, "xone start failed", 1))
}
