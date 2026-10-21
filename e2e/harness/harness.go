// Package harness is what the e2e tests lean on: it builds and starts a real service process,
// sends requests and signals, and reads its logs, spans, metrics and /proc, with a TCP proxy under
// the test's control, downstream stubs and a load generator besides.
//
// Every entry point goes through Require first, which t.Skips unless the XONE_E2E environment
// variable is 1. scripts/test.sh runs the tests of every module, and on a machine with no databases
// this module has to stay green all the same.
// To really run them, use scripts/e2e.sh: it brings up PG / MySQL / Redis and exports the
// connection settings below:
//
//	XONE_E2E_PG_ADDR      127.0.0.1:5432 by default
//	XONE_E2E_PG_USER      xone by default
//	XONE_E2E_PG_PASSWORD  e2e-secret-pw by default
//	XONE_E2E_PG_DB        xone_e2e by default
//	XONE_E2E_REDIS_ADDR   127.0.0.1:6379 by default
//	XONE_E2E_MYSQL_ADDR      127.0.0.1:3306 by default
//	XONE_E2E_MYSQL_USER      xone by default
//	XONE_E2E_MYSQL_PASSWORD  e2e-secret-pw by default
//	XONE_E2E_MYSQL_DB        xone_e2e by default
//	XONE_E2E_CH_ADDR      the ClickHouse native protocol, 127.0.0.1:9000 by default
//	XONE_E2E_CH_HTTP_ADDR the ClickHouse HTTP protocol, 127.0.0.1:8123 by default
//	XONE_E2E_CH_USER      xone by default
//	XONE_E2E_CH_PASSWORD  e2e-secret-pw by default
//	XONE_E2E_CH_DB        xone_e2e by default
//	XONE_E2E_CH           set to 0 when scripts/e2e.sh cannot bring ClickHouse up, and the CH cases skip (RequireCH)
//	XONE_E2E_KAFKA_ADDR   the Kafka broker, 127.0.0.1:9092 by default
//	XONE_E2E_KAFKA        set to 0 when scripts/e2e.sh cannot bring Kafka up, and the Kafka cases skip (RequireKafka)
//
// Every Start uses a port, a table name and a Redis key prefix of its own by default and deletes
// them when the test ends, so the cases do not disturb each other and can t.Parallel.
//
// A helper taking a t t.Fatals on failure and can only be called on the test's main goroutine;
// to send a request from another goroutine, use Request, which takes none.
package harness

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Require skips this test when e2e is off. The first line of every e2e test
func Require(t testing.TB) {
	t.Helper()
	if os.Getenv("XONE_E2E") != "1" {
		t.Skip("e2e tests are off: run scripts/e2e.sh, or set XONE_E2E=1 with PostgreSQL, MySQL and Redis running")
	}
}

// RequireCH is the first line of a ClickHouse case: it skips when e2e is off or ClickHouse is
// unavailable.
//
// ClickHouse runs in Docker, which not every machine has: scripts/e2e.sh sets XONE_E2E_CH=0 when it
// cannot bring it up, and this skips and says why; run without the script, it dials the native port
// and skips the same way when nothing answers
func RequireCH(t testing.TB) {
	t.Helper()
	Require(t)
	if os.Getenv("XONE_E2E_CH") == "0" {
		t.Skip("ClickHouse is unavailable (scripts/e2e.sh could not start the xone-ch container): ClickHouse tests skipped")
	}
	c, err := net.DialTimeout("tcp", CHAddr(), time.Second)
	if err != nil {
		t.Skipf("ClickHouse is unavailable at %s (%v): ClickHouse tests skipped, start it with scripts/e2e.sh", CHAddr(), err)
	}
	c.Close()
}

// KnownBug marks a bug in the framework that a test uncovered: it prints KNOWN BUG and skips.
//
// The assertions follow the documented behavior, and a failing one is not bent to go green: the
// failing branch calls this instead, with the evidence in the test's comment. Once the bug is
// fixed this line is simply never reached and the test passes as it should
func KnownBug(t testing.TB, msg string) {
	t.Helper()
	t.Skipf("KNOWN BUG: %s", msg)
}

// Main is for TestMain: it deletes the binaries that were built once every test has run.
//
//	func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
func Main(m *testing.M) int {
	code := m.Run()
	removeBinaries()
	return code
}

// ModuleDir is the e2e module's root (the absolute path of e2e/)
func ModuleDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// PGAddr is PostgreSQL's host:port, not through the proxy
func PGAddr() string { return env("XONE_E2E_PG_ADDR", "127.0.0.1:5432") }

// RedisAddr is Redis's host:port, not through the proxy
func RedisAddr() string { return env("XONE_E2E_REDIS_ADDR", "127.0.0.1:6379") }

// PGDSN is the DSN connecting to addr. PGAddr() connects directly, Proxy.Addr() through the proxy
func PGDSN(addr string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(env("XONE_E2E_PG_USER", "xone"), env("XONE_E2E_PG_PASSWORD", "e2e-secret-pw")),
		Host:     addr,
		Path:     "/" + env("XONE_E2E_PG_DB", "xone_e2e"),
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// MySQLAddr is MySQL's host:port, not through the proxy
func MySQLAddr() string { return env("XONE_E2E_MYSQL_ADDR", "127.0.0.1:3306") }

// MySQLPassword is the password the harness connects to MySQL with
func MySQLPassword() string { return env("XONE_E2E_MYSQL_PASSWORD", "e2e-secret-pw") }

// MySQLDSN is the go-sql-driver DSN connecting to addr. MySQLAddr() connects directly, Proxy.Addr()
// through the proxy.
//
// It carries no timeout at all: timeout / readTimeout / writeTimeout are injected by xgorm from the
// configuration, and the injected ones are exactly what a test is there to check; to test "what the
// DSN says is not overridden", append it in the test
func MySQLDSN(addr string) string {
	return env("XONE_E2E_MYSQL_USER", "xone") + ":" + MySQLPassword() + "@tcp(" + addr + ")/" + env("XONE_E2E_MYSQL_DB", "xone_e2e")
}

// CHAddr is the host:port of the ClickHouse native protocol, not through the proxy
func CHAddr() string { return env("XONE_E2E_CH_ADDR", "127.0.0.1:9000") }

// CHHTTPAddr is the host:port of the ClickHouse HTTP protocol, not through the proxy
func CHHTTPAddr() string { return env("XONE_E2E_CH_HTTP_ADDR", "127.0.0.1:8123") }

// CHPassword is the password the harness connects to ClickHouse with
func CHPassword() string { return env("XONE_E2E_CH_PASSWORD", "e2e-secret-pw") }

// CHDSN is the native-protocol DSN connecting to addr. CHAddr() connects directly, Proxy.Addr()
// through the proxy.
//
// It carries no timeout at all: dial_timeout is injected by xgorm from DialTimeout, and the
// injected one is exactly what a test is there to check
func CHDSN(addr string) string { return CHDSNWith("clickhouse", addr, CHPassword()) }

// CHDSNWith is the DSN with the scheme (clickhouse / tcp / http / https) and the password given
func CHDSNWith(scheme, addr, password string) string {
	u := url.URL{
		Scheme: scheme,
		User:   url.UserPassword(env("XONE_E2E_CH_USER", "xone"), password),
		Host:   addr,
		Path:   "/" + env("XONE_E2E_CH_DB", "xone_e2e"),
	}
	return u.String()
}

// NewID is a random lower-case identifier, 3fa9c1d2b7e4 for instance. Table names and key prefixes
// are built from it
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var ports struct {
	mu   sync.Mutex
	used map[int]bool
}

// FreePort finds a free port on this host. One test process never hands out the same port twice
func FreePort(t testing.TB) int {
	t.Helper()
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if ports.used == nil {
		ports.used = map[int]bool{}
	}
	for range 100 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find a free port: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if !ports.used[port] {
			ports.used[port] = true
			return port
		}
	}
	t.Fatal("find a free port: every port the kernel offered was already handed out")
	return 0
}
