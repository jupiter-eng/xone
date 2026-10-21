package xgorm

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
)

// TestMain shortens the retry interval: the cases that cannot connect run through a full round of retries, which would take tens of seconds at one second each.
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	os.Exit(m.Run())
}

// deadAddr returns an address nobody is listening on: connecting is certain to fail, and to fail fast.
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // closed right away, so nothing is listening on the port any more
	return addr
}

func TestNew_NoGoroutineLeakWhenUnreachable(t *testing.T) {
	// A failed New must not leave a live connection pool behind: the opener goroutine of database/sql
	// only exits on Close, so every leaked pool is a goroutine that will never go away.
	//
	// Since gorm v1.31 it closes the pool on its own failure paths, so what this mainly watches now is
	// the cleanup when the steps after gorm.Open fail -- setting pool parameters, Ping, attaching tracing.
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DSN = "u:p@tcp(" + deadAddr(t) + ")/app"
	c.DialTimeout = 50 * time.Millisecond
	c.MySQL.ReadTimeout = 50 * time.Millisecond

	const rounds = 5
	before := testkit.Stabilize()
	for i := 0; i < rounds; i++ {
		db, closer, err := New(context.Background(), c)
		if err == nil {
			reg.Close()
			t.Fatal("failing to connect must be an error")
		}
		if db != nil || closer != nil {
			t.Error("nothing half-built may be returned on failure")
		}
	}

	// The goroutines must have really exited before counting: after a pool is closed its opener goroutine
	// exits asynchronously, so counting immediately would read "exiting" as "leaked" and drown a real leak in noise.
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("after %d failed connections the goroutine count rose from %d to %d, so the pool was not closed", rounds, before, after)
	}
}

func TestNew_ErrorHasAddrButNoPassword(t *testing.T) {
	addr := deadAddr(t)
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DSN = "u:" + secret + "@tcp(" + addr + ")/app"
	c.DialTimeout = 50 * time.Millisecond
	c.MySQL.ReadTimeout = 50 * time.Millisecond

	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("failing to connect must be an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the password appeared in the error message: %v", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("the error must carry the address, otherwise there is no way to tell what was connected to: %v", err)
	}
}

func TestNew_NoConnectOnBadConfig(t *testing.T) {
	c := DefaultClientConfig() // no DSN
	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("an empty DSN must be rejected")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("the error must say plainly where the problem is, got=%v", err)
	}
}

func TestProbeTimeout(t *testing.T) {
	// The dial timeout alone is not enough: the probe is a connection plus one round trip,
	// so spending the dial budget on the whole thing would time out just as the connection comes up.
	c := mysqlCfg("u:p@tcp(h:3306)/d")
	c.DialTimeout = time.Second
	c.MySQL.ReadTimeout = 2 * time.Second
	if got := resolvedProbeTimeout(t, c); got != 3*time.Second {
		t.Errorf("the MySQL probe budget must be dial + read timeout, got=%v", got)
	}

	// A 0 means "no limit in the driver", not "this step costs nothing": that step gets a budget of the same
	// order as the other one, for the same reason as the 2 x DialTimeout used when the dialect cannot work it out.
	// With only the dial timeout, ReadTimeout: 0 would time out just after the connection comes up, before the login packet arrives.
	for _, z := range []struct {
		name       string
		dial, read time.Duration
		want       time.Duration
	}{
		{"ReadTimeout is 0", time.Second, 0, 2 * time.Second},
		{"DialTimeout is 0", 0, 2 * time.Second, 4 * time.Second},
		{"both 0 falls back", 0, 0, fallbackPingTimeout},
	} {
		c := mysqlCfg("u:p@tcp(h:3306)/d")
		c.DialTimeout, c.MySQL.ReadTimeout = z.dial, z.read
		if got := resolvedProbeTimeout(t, c); got != z.want {
			t.Errorf("%s: MySQL probe budget want %v, got %v", z.name, z.want, got)
		}
	}

	// The connect_timeout PG injects is a whole number of seconds rounded up and covers the entire connection (TCP, TLS, auth).
	// A shorter budget would time out a slow but legal handshake before pgx itself gives up.
	c = pgCfg("host=h dbname=d")
	c.DialTimeout = 500 * time.Millisecond
	if got := resolvedProbeTimeout(t, c); got != time.Second+500*time.Millisecond {
		t.Errorf("the PG budget must cover the injected connect_timeout (1s) plus one round trip, got=%v", got)
	}

	// A dialect that cannot work it out (no Resolve) gets 2 x DialTimeout; if DialTimeout is 0 too, the fallback is used.
	c = DefaultClientConfig()
	c.DialTimeout = 300 * time.Millisecond
	if got := probeTimeout(c, ConnInfo{}); got != 600*time.Millisecond {
		t.Errorf("without a budget from the dialect it must be 2 x DialTimeout, got=%v", got)
	}
	c.DialTimeout = 0
	if got := probeTimeout(c, ConnInfo{}); got != fallbackPingTimeout {
		t.Errorf("the fallback must be used when no budget can be worked out, got=%v", got)
	}
	if got := probeTimeout(c, ConnInfo{ProbeTimeout: 7 * time.Second}); got != 7*time.Second {
		t.Errorf("a budget given by the dialect must be used, got=%v", got)
	}
}

// resolvedProbeTimeout runs the dialect's Resolve and returns the budget New would really use.
func resolvedProbeTimeout(t *testing.T, c ClientConfig) time.Duration {
	t.Helper()
	_, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	return probeTimeout(c, info)
}

func TestProbeTimeout_DSNLongerTimeoutWidensBudget(t *testing.T) {
	// The timeouts in the config are only defaults injected into the DSN. If the DSN says something longer the driver
	// waits that long, and a budget still computed from the config would time out a slow but legal connection before the driver gives up.
	for _, c := range []struct {
		name string
		cfg  ClientConfig
		min  time.Duration
	}{
		{"PG connect_timeout", pgCfg("host=h dbname=d connect_timeout=10"), 10 * time.Second},
		{"MySQL timeout", mysqlCfg("u:p@tcp(h:3306)/d?timeout=10s"), 10 * time.Second},
		{"MySQL readTimeout", mysqlCfg("u:p@tcp(h:3306)/d?readTimeout=10s"), 10 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvedProbeTimeout(t, c.cfg); got < c.min {
				t.Errorf("the budget must cover the %v written in the DSN, got=%v", c.min, got)
			}
		})
	}
}

func TestProbe_FailsAfterRetries(t *testing.T) {
	// Retries must really retry, and must finish within the budget -- hanging at startup is harder to diagnose than failing to connect.
	pool, err := sql.Open("mysql", "u:p@tcp("+deadAddr(t)+")/app?timeout=30ms")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	c := mysqlCfg("u:p@tcp(h:1)/app")
	c.DialTimeout = 30 * time.Millisecond
	c.MySQL.ReadTimeout = 30 * time.Millisecond
	_, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	policy := probePolicy(c, info, mysqlDialect())

	start := time.Now()
	if err := xclient.Probe(context.Background(), policy, probe(pool, nil)); err == nil {
		t.Fatal("failing to connect must return an error")
	}
	// Only the upper bound is checked. With jitter each wait is a random value in [0, current backoff] and can be
	// close to zero, so "how long it waited at least" is no longer a valid proxy for the number of retries.
	// The upper bound on backoff waiting is interval + 2*interval = 3*interval; 4x leaves enough room.
	ceiling := pingAttempts*policy.Timeout + 4*pingInterval + 2*time.Second
	if elapsed := time.Since(start); elapsed > ceiling {
		t.Errorf("the retries went over budget, took %v", elapsed)
	}
}

// ---- global instances ----

func withClients(t *testing.T, m map[string]*gorm.DB) {
	t.Helper()
	insts := make(map[string]instance, len(m))
	for name, db := range m {
		insts[name] = instance{db: db, metric: true}
	}
	publish(t, insts)
}

// publish hands a set of ready-made instances to xclient.Build and clears them again when the test ends.
func publish(t *testing.T, insts map[string]instance) {
	t.Helper()
	keep := func(_ context.Context, _ string, v instance) (instance, io.Closer, error) { return v, nil, nil }
	if err := xclient.Build(context.Background(), reg, insts, keep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = xclient.Build(context.Background(), reg, nil, keep) })
}

func TestC_PanicsWhenMissing(t *testing.T) {
	// Returning nil only defers the same panic to the caller's first use, where the stack holds nothing but
	// invalid memory address and gives no hint that the root cause is a missing config block.
	withClients(t, map[string]*gorm.DB{})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("asking for an instance that does not exist must panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, ConfigKey) {
			t.Errorf("with nothing configured it must point at the config block, got=%v", msg)
		}
	}()
	C()
}

func TestC_PanicListsConfiguredInstances(t *testing.T) {
	// A typo in the name and a missing config block are two different problems; listing what is actually configured tells them apart at a glance.
	withClients(t, map[string]*gorm.DB{"main": {}, "report": {}})

	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "main") || !strings.Contains(msg, "report") {
			t.Errorf("the configured instance names must be listed, got=%v", msg)
		}
	}()
	C("typo")
}

func TestHasNames(t *testing.T) {
	withClients(t, map[string]*gorm.DB{"report": {}, "main": {}})

	if !Has("main") || Has("nope") {
		t.Error("Has must report truthfully whether something was configured")
	}
	if got := Names(); len(got) != 2 || got[0] != "main" || got[1] != "report" {
		t.Errorf("Names must be sorted by name, got=%v", got)
	}
	if Has() {
		t.Error("Has() must be false when there is no default")
	}
}

func TestRegister_MatchesFramework(t *testing.T) {
	// This is the only wire between this package and the framework: a missing hook registration or a wrong stage
	// shows up as "the config has no effect" or "the instance is ready later than what uses it", and nothing else catches it.
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgorm" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("no start hook was registered")
	}
	if got.Stage != hook.StageClient {
		t.Errorf("the database must be ready before business code (StageClient), got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xgorm" {
			stopped = true
			if e.Stage != hook.StageClient {
				// If start and stop are not on the same stage, it is closed while business code is still using it.
				t.Errorf("the stop hook must be on StageClient too, got=%v", e.Stage)
			}
		}
	}
	if true != stopped {
		t.Errorf("the stop hook registration is wrong, got=%v", stopped)
	}
}

func TestInitAll_NoopWithoutConfig(t *testing.T) {
	err := initComponent(t, DefaultConfig())
	if err != nil {
		t.Fatalf("no XGorm configured must not be an error: %v", err)
	}
	if err := reg.Close(); err != nil {
		t.Errorf("closing must not be an error: %v", err)
	}
	if len(Names()) != 0 {
		t.Errorf("no instance may be built, got=%v", Names())
	}
}

func TestInitAll_OneFailureRollsBackAll(t *testing.T) {
	// When Init returns an error the framework never gets the closer, so whatever was already built must clean itself up or the pools leak.
	bad := DefaultClientConfig()
	bad.Driver, bad.DSN = DriverMySQL, "u:p@tcp("+deadAddr(t)+")/app"
	bad.DialTimeout, bad.MySQL.ReadTimeout = 30*time.Millisecond, 30*time.Millisecond
	cfg := Config{Clients: map[string]ClientConfig{"a": bad, "b": bad}}

	before := testkit.Stabilize()
	err := initComponent(t, cfg)
	if err == nil {
		t.Fatal("failing to connect must be an error")
	}
	if !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("the error must name the instance, got=%v", err)
	}
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("the rollback was not clean, the goroutine count rose from %d to %d", before, after)
	}
}

func TestInstallPoolMetrics_RegistersOnceWhenCalledTwice(t *testing.T) {
	// Every instance with metrics enabled calls it once, while the collector is a single process-wide object:
	// a second registration makes Prometheus report a duplicate and none of these metrics is ever exported again.
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	installPoolMetrics()
	installPoolMetrics()

	if strings.Contains(buf.String(), "failed to register") {
		t.Errorf("attaching a second time must reuse the existing one instead of making Prometheus report a duplicate registration\ngot=\n%s", buf.String())
	}
}

func TestCWithCtx(t *testing.T) {
	// GORM requires the ctx to be passed this way, unlike go-redis where every method takes a ctx.
	withClients(t, map[string]*gorm.DB{DefaultName: {Config: &gorm.Config{}, Statement: &gorm.Statement{}}})
	db := CWithCtx(context.Background())
	if db == nil {
		t.Fatal("an instance must be returned")
	}
}

func TestSettle_DetectsLeakedPool(t *testing.T) {
	// First prove the measuring stick is accurate: the previous version read the count without waiting for goroutines to exit
	// and used a threshold of +2, so the "no goroutine leak" test passed no matter what was changed -- a test that can never
	// fail is worse than no test, because it makes people believe the thing was checked.
	addr := deadAddr(t)
	before := testkit.Stabilize()

	const leaked = 5
	pools := make([]*sql.DB, 0, leaked)
	for i := 0; i < leaked; i++ {
		pool, err := sql.Open("mysql", "u:p@tcp("+addr+")/app")
		if err != nil {
			t.Fatal(err)
		}
		pool.SetMaxIdleConns(1)
		// Touch it so the opener goroutine really starts.
		pool.PingContext(context.Background())
		pools = append(pools, pool)
	}

	// ClimbTo is used to decide "did it rise": SettleTo waits for a fall back to within target, and a leaked
	// pool never falls back, so it would burn the full 10 seconds every time. Each pool has at least one opener goroutine.
	if during := testkit.ClimbTo(before+leaked, 3*time.Second); during < before+leaked {
		t.Fatalf("%d leaked pools produced no visible goroutine growth (%d -> %d), so this measuring stick is broken", leaked, before, during)
	}
	for _, p := range pools {
		p.Close()
	}
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("it must fall back once everything is closed, got %d -> %d", before, after)
	}
}

func TestNew_CtxCanceledAbortsFirstConnect(t *testing.T) {
	// The ping GORM does by itself uses its own context, which our shutdown signal cannot reach.
	// Left on, connecting to an unreachable address would first wait out the full connect_timeout from the DSN.
	c := DefaultClientConfig()
	c.DSN = "postgres://u:p@10.255.255.1:5432/db" // a black-hole address; the connection just hangs
	c.DialTimeout = 3 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, _, err := New(ctx, c)
	if err == nil {
		t.Fatal("connecting must not succeed once the ctx is cancelled")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("it must give up immediately, waited %v (so the first connection did not use our ctx)", elapsed)
	}
}

func TestNew_LogOffKeepsSQLErrorsOffStdout(t *testing.T) {
	// Without a gormCfg.Logger, gorm.Open fills in logger.Default, whose implementation writes to
	// os.Stdout with ANSI colors: slow queries and execution errors are still printed, they just bypass
	// slog -- no level, no TraceID, not JSON, a line of colored text spliced straight into the log stream.
	// Log=false means do not print, not print somewhere else.
	var leaked bytes.Buffer
	old := logger.Default
	logger.Default = logger.New(log.New(&leaked, "", 0), logger.Config{LogLevel: logger.Info})
	t.Cleanup(func() { logger.Default = old })

	sentinel := errors.New("stub dialector was used")
	withDialect(t, Dialect{
		Name: "logprobe",
		Open: func(string) gorm.Dialector {
			return loggingDialector{err: sentinel}
		},
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN, c.Log = "logprobe", "logprobe://h:1/d", false
	if _, _, err := New(context.Background(), c); !errors.Is(err, sentinel) {
		t.Fatalf("the registered Open must be used, got=%v", err)
	}
	if leaked.Len() > 0 {
		t.Errorf("with Log=false GORM must not write to its own default output, but it wrote: %q", leaked.String())
	}
}

// loggingDialector writes one line in Initialize through the Logger GORM resolved,
// which makes visible which logger is actually in effect when Log=false.
type loggingDialector struct{ err error }

func (d loggingDialector) Initialize(db *gorm.DB) error {
	db.Logger.Error(context.Background(), "probe: a query failed")
	return d.err
}
func (d loggingDialector) Name() string                    { return "logprobe" }
func (d loggingDialector) Migrator(*gorm.DB) gorm.Migrator { return nil }
func (d loggingDialector) DataTypeOf(*schema.Field) string { return "" }
func (d loggingDialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{}
}
func (d loggingDialector) BindVarTo(clause.Writer, *gorm.Statement, any) {}
func (d loggingDialector) QuoteTo(clause.Writer, string)                 {}
func (d loggingDialector) Explain(sql string, _ ...any) string           { return sql }

func TestResolveDSN_PGParamNameInPasswordNotMistaken(t *testing.T) {
	// The old code scanned for key= with a regexp to decide whether something was set, so a connect_timeout= inside
	// the password was enough to fool it and the DialTimeout setting quietly stopped working -- with no sign of it anywhere.
	cases := []struct {
		name, dsn string
		want      bool // connect_timeout is expected to be injected
	}{
		{"the password contains a parameter name", "host=h dbname=d password='a connect_timeout=99 b'", true},
		{"an ordinary password", "host=h dbname=d password=plain", true},
		{"really already set", "host=h dbname=d connect_timeout=9", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := resolveDSN(pgCfg(c.dsn))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := pgconn.ParseConfig(got)
			if err != nil {
				t.Fatal(err)
			}
			injected := cfg.ConnectTimeout == time.Second
			if injected != c.want {
				t.Errorf("injected=%v want=%v\nin:  %s\nout: %s", injected, c.want, c.dsn, got)
			}
		})
	}
}

// initComponent walks the path the framework really takes: build the instances one by one from this config.
// Everything about building several of them (ordering, rollback on failure, panic isolation) lives on that path.
func initComponent(t *testing.T, c Config) error {
	t.Helper()
	t.Cleanup(func() { _ = reg.Close() })
	return install(context.Background(), c)
}

func TestInitXGorm_NoSectionConnectsNothing(t *testing.T) {
	// xgorm is an optional dependency: not configuring it must not keep the service from starting, and must certainly not connect to some default address.
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXGorm(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be built, got=%v", got)
	}
}

func TestInitXGorm_EmptySectionConnectsNothing(t *testing.T) {
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XGorm:\n")

	if err := initXGorm(context.Background()); err != nil {
		t.Fatalf("an empty block must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be built, got=%v", got)
	}
}

func TestInitXGorm_BadConfigFailsStartup(t *testing.T) {
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XGorm:\n  DS: \"host=127.0.0.1\"\n")

	if err := initXGorm(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup, otherwise the caller keeps believing the setting took")
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be left behind on failure, got=%v", got)
	}
}

func TestInitXGorm_ConfigAppliedToInstance(t *testing.T) {
	// Failing to connect is expected -- what matters here is that the error carries the instance name and address from the
	// config file, proving this config section really reached the connect step instead of being dropped somewhere.
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	addr := deadAddr(t)
	xonetest.UseConfigYAML(t, "XGorm:\n  Clients:\n    report:\n      Driver: mysql\n      DialTimeout: 30ms\n      DSN: \"u:p@tcp("+addr+")/app\"\n")

	err := initXGorm(context.Background())
	if err == nil {
		t.Fatal("failing to connect must be an error")
	}
	if !strings.Contains(err.Error(), `"report"`) {
		t.Errorf("the error must name the instance, got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be left behind on failure, got=%v", got)
	}
}

func TestCloseXGorm_SafeWithoutBuild(t *testing.T) {
	if err := closeXGorm(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestPoolCloser_ClosesUnderlyingPool(t *testing.T) {
	// The Closer holds the *sql.DB directly instead of going back through gorm.DB.DB(): the latter returns an error
	// once the instance is already broken, and then the pool could never be closed.
	pool, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/app")
	if err != nil {
		t.Fatal(err)
	}
	c := &poolCloser{pool: pool, info: ConnInfo{Addr: "127.0.0.1:1"}}

	if err := c.Close(); err != nil {
		t.Fatalf("closing must not be an error: %v", err)
	}
	if err := pool.PingContext(context.Background()); err == nil {
		t.Error("the underlying pool was not closed")
	}
}

func TestPoolCloser_CloseErrorHasAddrButNoPassword(t *testing.T) {
	// Error messages reach logs and alerts, so credentials must not travel with them.
	pool, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/app")
	if err != nil {
		t.Fatal(err)
	}
	c := &poolCloser{pool: pool, info: ConnInfo{Addr: "127.0.0.1:1"}}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// database/sql's Close is idempotent, so a real failure cannot be constructed;
	// this simply confirms that the message template carries the address rather than the DSN.
	msg := xerror.Newf("xgorm", "close", "close %s failed: %w", c.info.Addr, errors.New("boom")).Error()
	if !strings.Contains(msg, "127.0.0.1:1") {
		t.Errorf("the error must carry the address so it is clear which database it is, got=%v", msg)
	}
	if strings.Contains(msg, "u:p@") {
		t.Errorf("credentials must not appear in the error, got=%v", msg)
	}
}

// assertOneFrame asserts the error is exactly one xerror frame from this module with the expected op.
// One module boundary, one xerror: wrapping at every level repeats the module name and buries the real op inside.
func assertOneFrame(t *testing.T, err error, module, op string) {
	t.Helper()
	var xe *xerror.Error
	if !errors.As(err, &xe) {
		t.Fatalf("must be an xerror, got %v", err)
	}
	if xe.Module != module || xe.Op != op {
		t.Errorf("want module=%s op=%s, got module=%s op=%s (%v)", module, op, xe.Module, xe.Op, err)
	}
	var inner *xerror.Error
	if errors.As(xe.Err, &inner) {
		t.Errorf("there must be only one xerror frame per module, got %v", err)
	}
}

func TestNew_ErrorWrappedOnceWithAccurateOp(t *testing.T) {
	c := DefaultClientConfig()
	assertOneFrame(t, func() error { _, _, err := New(context.Background(), c); return err }(), "xgorm", "config")

	c.Driver, c.DSN = DriverMySQL, "u:p@tcp(127.0.0.1:1/app" // unbalanced parenthesis, the driver cannot parse it
	assertOneFrame(t, func() error { _, _, err := New(context.Background(), c); return err }(), "xgorm", "config")
}

func TestInstall_ErrorWrappedOnceWithAccurateOp(t *testing.T) {
	// An error coming out of Build must name the instance, but must not add another frame of new that buries config / connect.
	bad := DefaultClientConfig()
	bad.Driver, bad.DSN = DriverMySQL, "u:p@tcp(127.0.0.1:1/app"
	err := initComponent(t, Config{Clients: map[string]ClientConfig{"a": bad}})
	assertOneFrame(t, err, "xgorm", "config")

	dead := DefaultClientConfig()
	dead.Driver, dead.DSN = DriverMySQL, "u:p@tcp("+deadAddr(t)+")/app"
	dead.DialTimeout, dead.MySQL.ReadTimeout = 30*time.Millisecond, 30*time.Millisecond
	err = initComponent(t, Config{Clients: map[string]ClientConfig{"b": dead}})
	assertOneFrame(t, err, "xgorm", "connect")
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("the error must name the instance, got %v", err)
	}
}

func TestInitXGorm_CSaysNotConfiguredRatherThanTooEarly(t *testing.T) {
	// Even with nothing configured the registry must know the start hook ran. Otherwise C() would report "not configured"
	// as "called too early", sending the caller to look at call timing when the config file is what needs checking.
	t.Cleanup(func() { _ = closeXGorm(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXGorm(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "none is configured") {
			t.Errorf("when nothing is configured it must say so, got=%v", msg)
		}
	}()
	C()
}

func TestInstall_ConnectLogNamesInstance(t *testing.T) {
	// With several instances, addr / db cannot tell them apart (report and default may be connected to the same database).
	withDialect(t, Dialect{Name: "namedb", Open: func(string) gorm.Dialector { return okDialector{} }})
	c := DefaultClientConfig()
	c.Driver, c.DSN = "namedb", "namedb://h/d"

	lines := capture(t)
	if err := install(context.Background(), Config{Clients: map[string]ClientConfig{"report": c}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	var connected, ready map[string]any
	for _, l := range lines() {
		switch l["msg"] {
		case "xgorm connected":
			connected = l
		case "xgorm ready":
			ready = l
		}
	}
	if connected["name"] != "report" {
		t.Errorf("xgorm connected must carry the instance name report, got=%v", connected)
	}
	if fmt.Sprint(ready["instances"]) != "[report]" {
		t.Errorf("xgorm ready must list the instance names, got=%v", ready)
	}

	// A direct call to New has no name, and no empty name field is written.
	lines = capture(t)
	_, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	closer.Close()
	for _, l := range lines() {
		if _, ok := l["name"]; ok && l["msg"] == "xgorm connected" {
			t.Errorf("a direct call to New must have no name field, got=%v", l)
		}
	}
}

func TestNew_DialectDecidesAuthFailureRecognition(t *testing.T) {
	// Recognizing an auth failure moved into the individual dialects: a dialect that provides no AuthFailed
	// retries the same 28P01 as usual and reports it as unreachable -- the core no longer branches on driver name.
	rejected := &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password authentication failed"}
	calls := 0
	withDialect(t, Dialect{
		Name:  "noauthdb",
		Open:  func(string) gorm.Dialector { return okDialector{} },
		Ready: func(context.Context, *gorm.DB) error { calls++; return rejected },
	})
	c := DefaultClientConfig()
	c.Driver, c.DSN = "noauthdb", "noauthdb://h/d"
	_, _, err := New(context.Background(), c)
	if calls != pingAttempts || !strings.Contains(fmt.Sprint(err), "cannot reach ") {
		t.Errorf("a dialect that does not recognize it must retry as usual, calls=%d err=%v", calls, err)
	}
}

func TestNew_GormOptionsReachGorm(t *testing.T) {
	// These items are passed to gorm.Config as they are: unset means GORM's default, set means exactly what was set.
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprint(on), func(t *testing.T) {
			var got *gorm.Config
			withDialect(t, Dialect{
				Name: "cfgprobe",
				Open: func(string) gorm.Dialector { return configDialector{seen: &got} },
			})
			c := DefaultClientConfig()
			c.Driver, c.DSN = "cfgprobe", "cfgprobe://h:1/d"
			c.DisableForeignKeyConstraintWhenMigrating, c.SkipDefaultTransaction, c.PrepareStmt = on, on, on
			if on {
				c.CreateBatchSize = 7
			}
			New(context.Background(), c)
			if got == nil {
				t.Fatal("the dialector was never called")
			}
			if got.DisableForeignKeyConstraintWhenMigrating != on || got.SkipDefaultTransaction != on ||
				got.PrepareStmt != on || got.CreateBatchSize != c.CreateBatchSize {
				t.Errorf("the config must be passed to GORM as is: DisableForeignKeyConstraintWhenMigrating=%v SkipDefaultTransaction=%v PrepareStmt=%v CreateBatchSize=%d, want %v / %v / %v / %d",
					got.DisableForeignKeyConstraintWhenMigrating, got.SkipDefaultTransaction, got.PrepareStmt, got.CreateBatchSize,
					on, on, on, c.CreateBatchSize)
			}
		})
	}
}

// configDialector records in Initialize the config GORM finally received, then makes Open fail.
type configDialector struct {
	loggingDialector
	seen **gorm.Config
}

func (d configDialector) Initialize(db *gorm.DB) error {
	cp := *db.Config
	*d.seen = &cp
	return errors.New("stop after recording config")
}
