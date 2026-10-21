package xredis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
)

func liveCfg(f *fakeRedis) ClientConfig {
	c := DefaultClientConfig()
	c.Addr = f.addr()
	c.DialTimeout, c.ReadTimeout, c.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	return c
}

// deadAddr is an address nobody is listening on.
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestNew_ReturnsUsableClientWhenReachable(t *testing.T) {
	f := newFakeRedis(t)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatalf("it must connect: %v", err)
	}
	defer closer.Close()

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Errorf("what comes back must be a usable native client: %v", err)
	}
	if !slices.Contains(f.seen(), "ping") {
		t.Errorf("connecting must Ping once to confirm it works, commands received=%v", f.seen())
	}
}

func TestNew_VerifiesConnectivityAtStartup(t *testing.T) {
	// A wrong address or a wrong password should surface at startup, not at the first cache read in production.
	f := newFakeRedis(t)
	f.setFailPing(true)

	c := liveCfg(f)
	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("a failing Ping must be an error")
	}
	if !strings.Contains(err.Error(), c.Addr) {
		t.Errorf("the error must carry the address, otherwise there is no telling what was connected to: %v", err)
	}
}

func TestNew_NoPoolLeakWhenUnreachable(t *testing.T) {
	c := DefaultClientConfig()
	c.Addr = deadAddr(t)
	c.DialTimeout, c.ReadTimeout = 50*time.Millisecond, 50*time.Millisecond
	c.Trace = false

	before := testkit.Stabilize()
	for i := 0; i < 5; i++ {
		client, closer, err := New(context.Background(), c)
		if err == nil {
			reg.Close()
			t.Fatal("failing to connect must be an error")
		}
		if client != nil || closer != nil {
			t.Error("nothing half-built may be returned on failure")
		}
	}
	if after := testkit.SettleTo(before); after > before+1 {
		t.Errorf("after 5 failed connections the goroutine count rose from %d to %d, so the client was not closed", before, after)
	}
}

func TestInstall_LogsNameAndAddrButNotPassword(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)

	c := liveCfg(f)
	c.Password = "hunter2"
	if err := initComponent(t, Config{Clients: map[string]ClientConfig{"session": c}}); err != nil {
		t.Fatal(err)
	}

	var connected map[string]any
	for _, l := range lines() {
		blob := fmt.Sprint(l)
		if strings.Contains(blob, "hunter2") {
			t.Errorf("the password appeared in the log: %v", l)
		}
		if l["msg"] == "xredis connected" {
			connected = l
		}
	}
	// With several Redis instances configured, the address alone does not say which one it is.
	if connected == nil || connected["addr"] != c.Addr || connected["name"] != "session" {
		t.Errorf("the instance name and the address must be written, otherwise there is no telling what was connected to, got=%v", lines())
	}
}

func TestNew_NoConnectOnInvalidConfig(t *testing.T) {
	c := DefaultClientConfig()
	c.Addr = ""
	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("an empty Addr must be an error")
	}
}

func TestNew_ValidatesBeforeConnecting(t *testing.T) {
	// The peer is fine and only the configuration is invalid: without validation go-redis takes -1s as "no limit" and connects as usual.
	c := liveCfg(newFakeRedis(t))
	c.ReadTimeout = -time.Second
	_, closer, err := New(context.Background(), c)
	if closer != nil {
		closer.Close()
	}
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "config" {
		t.Fatalf("a direct New must validate the configuration first and report op=config, got=%v", err)
	}
}

func TestNew_PoolOptionsPassedThrough(t *testing.T) {
	f := newFakeRedis(t)
	c := liveCfg(f)
	c.PoolSize, c.MinIdleConns = 7, 2

	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if got := client.Options().PoolSize; got != 7 {
		t.Errorf("PoolSize must be passed down, got=%d", got)
	}
	if got := client.Options().MinIdleConns; got != 2 {
		t.Errorf("MinIdleConns must be passed down, got=%d", got)
	}
}

func TestPingTimeout(t *testing.T) {
	if got := pingTimeout(&redis.Options{DialTimeout: time.Second, ReadTimeout: 2 * time.Second}); got != 3*time.Second {
		t.Errorf("the Ping budget must be dial + read timeout, got=%v", got)
	}
	// A 0 in the configuration is left to go-redis to fill in, and the budget follows what it filled in.
	c := redis.NewClient(&redis.Options{})
	defer c.Close()
	if got := pingTimeout(c.Options()); got != 10*time.Second {
		t.Errorf("0 is go-redis's default (5s + 5s), got=%v", got)
	}
	if got := pingTimeout(&redis.Options{}); got != fallbackPingTimeout {
		t.Errorf("the fallback must be used when nothing can be worked out, got=%v", got)
	}
}

// A 0 means "use its default" in go-redis v9.22.0 (DialTimeout 5s, ReadTimeout 5s, options.go init),
// not "costs no time". Computing the budget from the configured 0 + 0 would leave only the 1s fallback: a connection
// answering PONG after 1.5s, which go-redis itself was still willing to wait for, would be timed out by the check, three times over.
func TestNew_ZeroTimeoutsProbeWithGoRedisDefaults(t *testing.T) {
	f := newFakeRedis(t)
	f.setDelay("ping", 1500*time.Millisecond)
	c := liveCfg(f)
	c.DialTimeout, c.ReadTimeout, c.WriteTimeout = 0, 0, 0
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatalf("go-redis waits 5s by default, so a 1.5s PONG must connect: %v", err)
	}
	defer closer.Close()
	if o := client.Options(); o.DialTimeout != 5*time.Second || o.ReadTimeout != 5*time.Second {
		t.Errorf("precondition: a 0 handed to go-redis becomes its default, got dial=%v read=%v", o.DialTimeout, o.ReadTimeout)
	}
}

func TestNew_AbortsConnectOnExitSignal(t *testing.T) {
	// With the address unreachable this runs a full round of Ping retries (3 attempts with their intervals by default).
	// A SIGTERM halfway through startup should make it give up at once rather than leaving the process stuck on a
	// database it will never reach, stretching the exit by a whole round of retries.
	f := newFakeRedis(t)
	f.setFailPing(true)
	c := liveCfg(f)
	// The retry interval is set to an hour: a full round means hanging, while giving up at once is milliseconds, and the 5s bound is far from both.
	// Using TestMain's 10ms as the bound would misfire as soon as the machine is busy.
	old := pingInterval
	pingInterval = time.Hour
	t.Cleanup(func() { pingInterval = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate a stop signal arriving before the connection is made

	start := time.Now()
	_, _, err := New(ctx, c)
	if err == nil {
		t.Fatal("connecting must not succeed once the ctx is cancelled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("it must give up on the spot rather than run a full round of retries, took=%v", elapsed)
	}
}

func TestProbe_FailsAfterRetries(t *testing.T) {
	f := newFakeRedis(t)
	f.setFailPing(true)
	c := liveCfg(f)

	client := redis.NewClient(&redis.Options{Addr: c.Addr})
	defer client.Close()

	if err := xclient.Probe(context.Background(), probePolicy(client.Options()), probe(client)); err == nil {
		t.Fatal("a Ping that keeps failing must return an error")
	}
	// Count the pings the server received rather than looking at the elapsed time: with jitter,
	// each wait is a random value in [0, the current backoff] and can be close to zero.
	// "how many retries" should be counted, and the elapsed time is only a weak proxy for it.
	n := 0
	for _, cmd := range f.seen() {
		if cmd == "ping" {
			n++
		}
	}
	if n != pingAttempts {
		t.Errorf("the server must receive %d pings, got %d", pingAttempts, n)
	}
}

// ---- global instances ----

func withClients(t *testing.T, m map[string]*redis.Client) {
	t.Helper()
	insts := make(map[string]instance, len(m))
	for name, c := range m {
		insts[name] = instance{client: c, metric: true}
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

func TestC_PanicsWhenNotFound(t *testing.T) {
	withClients(t, map[string]*redis.Client{})
	defer func() {
		msg := fmt.Sprint(recover())
		if msg == "<nil>" {
			t.Fatal("fetching an instance that does not exist must panic")
		}
		if !strings.Contains(msg, ConfigKey) {
			t.Errorf("with nothing configured it must point at the config block, got=%v", msg)
		}
	}()
	C()
}

func TestC_PanicMessageListsConfiguredInstances(t *testing.T) {
	withClients(t, map[string]*redis.Client{"cache": {}, "session": {}})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "cache") || !strings.Contains(msg, "session") {
			t.Errorf("the configured instance names must be listed, got=%v", msg)
		}
	}()
	C("typo")
}

func TestHasNames(t *testing.T) {
	withClients(t, map[string]*redis.Client{"session": {}, "cache": {}})
	if !Has("cache") || Has("nope") || Has() {
		t.Error("Has must report truthfully whether something was configured")
	}
	if got := Names(); len(got) != 2 || got[0] != "cache" {
		t.Errorf("Names must be sorted by name, got=%v", got)
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// This is the only wire between this package and the framework: a missing hook registration or a wrong stage
	// shows up as "the config has no effect" or "the instance is ready later than what uses it", and nothing else catches it.
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xredis" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("no start hook was registered")
	}
	if got.Stage != hook.StageClient {
		t.Errorf("the cache must be ready before business code (StageClient), got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xredis" {
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

func TestInitAll_BuildsAndClosesCleanly(t *testing.T) {
	// The configuration is passed in directly, with no package variable to swap and remember to swap back:
	// start's receiver is that configuration.
	f := newFakeRedis(t)
	c := Config{Clients: map[string]ClientConfig{"a": liveCfg(f), "b": liveCfg(f)}}

	err := initComponent(t, c)
	if err != nil {
		t.Fatalf("it must build: %v", err)
	}
	if got := Names(); len(got) != 2 {
		t.Fatalf("there must be two instances, got=%v", got)
	}
	if err := reg.Close(); err != nil {
		t.Errorf("closing must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("it must be cleared after closing, otherwise C() would return a closed instance, got=%v", got)
	}
}

func TestInitAll_OneFailureRollsBackAll(t *testing.T) {
	// When Init returns an error the framework never gets the closer, so whatever was already built has to clean
	// itself up -- otherwise those connections live until the process ends and C() may still touch them.
	f := newFakeRedis(t)

	bad := DefaultClientConfig()
	bad.Addr = deadAddr(t)
	bad.DialTimeout, bad.ReadTimeout, bad.MinIdleConns = 30*time.Millisecond, 30*time.Millisecond, 0
	cfg := Config{Clients: map[string]ClientConfig{"a": liveCfg(f), "z": bad}}

	err := initComponent(t, cfg)
	if err == nil {
		t.Fatal("an instance that cannot connect must be an error")
	}
	if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("the error must name the instance, got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be published on failure, or C() would touch something half-built, got=%v", got)
	}
	// Look at the fake server directly: only with the connections gone is the one built first really closed.
	if n := f.waitConns(0); n != 0 {
		t.Errorf("the instance built first must be rolled back and closed, %d connections remain on the fake server", n)
	}
}

func TestInitAll_NoOpWhenUnconfigured(t *testing.T) {
	cfg := DefaultConfig()

	err := initComponent(t, cfg)
	if err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	reg.Close()
	if len(Names()) != 0 {
		t.Errorf("no instance may be built, got=%v", Names())
	}
}

func TestInstallPoolMetrics_RegistersOnceWhenInstalledRepeatedly(t *testing.T) {
	// Every instance with metrics enabled calls it once, while the collector is a single process-wide object:
	// a second registration makes Prometheus report a duplicate and none of these metrics is ever exported again.
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	// go-redis background goroutines left by other cases also write to the default logger, so the buffer has to be safe across goroutines.
	var buf syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	installPoolMetrics()
	installPoolMetrics()

	if strings.Contains(buf.String(), "failed to register") {
		t.Errorf("attaching a second time must reuse the existing one instead of making Prometheus report a duplicate registration\ngot=\n%s", buf.String())
	}
}

func TestPoolCollector(t *testing.T) {
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	stats := map[string]*redis.PoolStats{
		"cache": {TotalConns: 5, IdleConns: 3, StaleConns: 1, Hits: 100, Misses: 7, Timeouts: 2},
	}
	m.Registry.MustRegister(newPoolCollector("demo", nil, func() map[string]*redis.PoolStats { return stats }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()

	for _, want := range []string{
		`demo_redis_pool_connections{name="cache"} 5`,
		`demo_redis_pool_connections_idle{name="cache"} 3`,
		`demo_redis_pool_connections_stale_total{name="cache"} 1`,
		`demo_redis_pool_hits_total{name="cache"} 100`,
		`demo_redis_pool_misses_total{name="cache"} 7`,
		`demo_redis_pool_timeouts_total{name="cache"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the export must contain %s\ngot=\n%s", want, out)
		}
	}
}

func TestPoolStats_ReadsLiveInstance(t *testing.T) {
	f := newFakeRedis(t)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	withClients(t, map[string]*redis.Client{"cache": client})

	got := poolStats()
	if len(got) != 1 || got["cache"] == nil {
		t.Errorf("a live instance must be read, got=%v", got)
	}
}

func TestNew_CommandsHonorCallerDeadline(t *testing.T) {
	// go-redis does not let the request context govern the socket reads and writes by default: without
	// ContextTimeoutEnabled, every command uses the fixed ReadTimeout pair and
	// the caller's deadline is decoration -- a request with a 200ms timeout would still wait out
	// ReadTimeout on a slow Redis, and the upstream's timeout budget and cascade protection fail with it.
	f := newFakeRedis(t)
	f.setStall("get") // accept GET and never answer

	c := liveCfg(f)
	c.ReadTimeout = 5 * time.Second // far larger than the ctx's budget
	c.MaxRetries = -1               // retries would mask this
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = client.Get(ctx, "k").Err()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("it must time out")
	}
	// go-redis sets the ctx deadline on the socket, so what is reported is
	// os.ErrDeadlineExceeded (i/o timeout) rather than context.DeadlineExceeded.
	// A caller checking for a timeout has to recognize that one, or simply check ctx.Err().
	if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("it must be a timeout error, got=%v (%T)", err, err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the ctx gave 200ms and it waited %v -- the deadline did not govern the socket reads and writes", elapsed.Round(10*time.Millisecond))
	}
}

// initComponent walks the path the framework really takes: build the instances one by one from this config.
// Everything about building several of them (ordering, rollback on failure, panic isolation) lives on that path.
func initComponent(t *testing.T, c Config) error {
	t.Helper()
	t.Cleanup(func() { _ = reg.Close() })
	return install(context.Background(), c)
}

func TestInitXRedis_NoConnectionsWithoutSection(t *testing.T) {
	// xredis is an optional dependency: not configuring it must not keep the service from starting, and must certainly not connect to localhost:6379.
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be built, got=%v", got)
	}
}

func TestInitXRedis_NoConnectionsWithEmptySection(t *testing.T) {
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XRedis:\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("an empty block must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be built, got=%v", got)
	}
}

func TestInitXRedis_ConfigTypoFailsStartup(t *testing.T) {
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XRedis:\n  Addrs: \"127.0.0.1:6379\"\n")

	if err := initXRedis(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup, otherwise the caller keeps believing the setting took")
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be left behind on failure, got=%v", got)
	}
}

func TestInitXRedis_BuildsFromConfigAndClosesCleanly(t *testing.T) {
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	f := newFakeRedis(t)
	xonetest.UseConfigYAML(t, "XRedis:\n  Clients:\n    cache:\n      Addr: \""+f.addr()+"\"\n    session:\n      Addr: \""+f.addr()+"\"\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("it must build: %v", err)
	}
	if got := Names(); len(got) != 2 {
		t.Fatalf("there must be two instances, got=%v", got)
	}
	if C("cache").Options().Addr != f.addr() {
		t.Errorf("the address from the configuration did not reach the instance, got=%v", C("cache").Options().Addr)
	}

	if err := closeXRedis(context.Background()); err != nil {
		t.Errorf("closing must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("an instance is still reachable after closing, so a caller would touch a closed pool, got=%v", got)
	}
}

func TestCloseXRedis_ClosesWithoutInit(t *testing.T) {
	if err := closeXRedis(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestInitXRedis_EmptySectionDoesNotFailStartup(t *testing.T) {
	// An empty block is "holding the place": commenting the contents out, or moving them into a profile file,
	// leaves exactly this shape. It used to make Run report "nobody reads the key XRedis,
	// check the spelling, or whether the matching package is imported" -- neither of which was true.
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XRedis:\n")

	if err := initXRedis(context.Background()); err != nil {
		t.Fatalf("an empty block must not be an error: %v", err)
	}
	if got := config.Unclaimed(); len(got) != 0 {
		t.Errorf("skipping it still counts as reading it, or the framework would fail startup over an unclaimed key, got=%v", got)
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

func TestInstall_ErrorWrappedOnceWithAccurateOp(t *testing.T) {
	bad := DefaultClientConfig()
	bad.Addr = ""
	err := initComponent(t, Config{Clients: map[string]ClientConfig{"a": bad}})
	assertOneFrame(t, err, "xredis", "config")

	dead := DefaultClientConfig()
	dead.Addr, dead.DialTimeout, dead.ReadTimeout = deadAddr(t), 30*time.Millisecond, 30*time.Millisecond
	err = initComponent(t, Config{Clients: map[string]ClientConfig{"b": dead}})
	assertOneFrame(t, err, "xredis", "connect")
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("the error must name the instance, got %v", err)
	}
}

func TestInstall_ExportsOnlyMetricEnabledInstances_SurvivesXmetricReinstall(t *testing.T) {
	// The collector is a single process-wide object that walks every instance when scraped: without consulting
	// each instance's own switch, Metric: false is an empty phrase. The second round is another lifecycle in the
	// same process -- xmetric has a new Registry, and the collector has to be attached to it or every pool metric is lost.
	f := newFakeRedis(t)
	on, off := liveCfg(f), liveCfg(f)
	off.Metric = false

	for round := 1; round <= 2; round++ {
		m, closer, err := xmetric.New(xmetric.Config{})
		if err != nil {
			t.Fatal(err)
		}
		m.Install()
		if err := install(context.Background(), Config{Clients: map[string]ClientConfig{"on": on, "off": off}}); err != nil {
			t.Fatal(err)
		}
		out := testkit.Scrape(m.Handler)
		if !strings.Contains(out, `redis_pool_connections{name="on"}`) {
			t.Errorf("round %d: an instance with Metric on must export\ngot=\n%s", round, out)
		}
		if strings.Contains(out, `name="off"`) {
			t.Errorf("round %d: an instance with Metric: false must not export\ngot=\n%s", round, out)
		}
		if err := reg.Close(); err != nil {
			t.Fatal(err)
		}
		closer.Close()
	}
}

func TestInitXRedis_CSaysUnconfiguredNotTooEarly(t *testing.T) {
	// Even with nothing configured the registry must know the start hook ran. Otherwise C() would report "not
	// configured" as "called too early", sending the caller to look at call timing when the config file is what needs checking.
	t.Cleanup(func() { _ = closeXRedis(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXRedis(context.Background()); err != nil {
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

// syncBuffer is a bytes.Buffer with a lock.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
