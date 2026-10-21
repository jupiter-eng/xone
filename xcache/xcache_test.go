package xcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xonetest"
)

func load(t *testing.T, yml string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	return c
}

func loadErr(t *testing.T, yml string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte(yml), 0o644)
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		return err
	}
	_, err := loadConfig()
	return err
}

// withInstances installs a set of instances and restores the previous ones when the test ends.
func withInstances(t testing.TB, cfgs map[string]ClientConfig) {
	t.Helper()
	built := map[string]instance{}
	for name, c := range cfgs {
		cache, closer, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = closer.Close() })
		built[name] = instance{cache: cache, ttl: c.DefaultTTL, metric: c.Metric}
	}
	publish(t, built)
}

// publish hands a set of ready-made instances to xclient.Build and clears them again when the test ends.
func publish(t testing.TB, insts map[string]instance) {
	t.Helper()
	keep := func(_ context.Context, _ string, v instance) (instance, io.Closer, error) { return v, nil, nil }
	if err := xclient.Build(context.Background(), reg, insts, keep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = xclient.Build(context.Background(), reg, nil, keep) })
}

func TestConfig_SingleInstanceForm(t *testing.T) {
	c := load(t, "XCache:\n  MaxCost: 500\n")
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("the single-instance form must be normalized to an instance named %s, got=%v", DefaultName, c.Clients)
	}
	if got.MaxCost != 500 {
		t.Errorf("a field that is set must take effect, got=%+v", got)
	}
	if got.NumCounters != 1_000_000 || got.DefaultTTL != 5*time.Minute {
		t.Errorf("a field that is not set must keep its default, got=%+v", got)
	}
}

func TestConfig_MultiInstanceForm(t *testing.T) {
	c := load(t, "XCache:\n  Clients:\n    default: {MaxCost: 100}\n    session: {MaxCost: 200, DefaultTTL: 30m}\n")
	if len(c.Clients) != 2 {
		t.Fatalf("two instances must be decoded, got=%v", c.Clients)
	}
	if c.Clients["session"].DefaultTTL != 30*time.Minute {
		t.Errorf("a field that is set must take effect, got=%+v", c.Clients["session"])
	}
	if c.Clients["default"].DefaultTTL != 5*time.Minute {
		t.Errorf("one instance changing the TTL must not affect the other, got=%+v", c.Clients["default"])
	}
}

func TestConfig_TypoFails(t *testing.T) {
	if err := loadErr(t, "XCache:\n  MaxCoat: 1\n"); err == nil {
		t.Fatal("a misspelled field must fail startup")
	}
	if err := loadErr(t, "XCache:\n  Clients:\n    a: {MaxCoat: 1}\n"); err == nil {
		t.Fatal("a misspelled field inside an instance must fail startup too")
	}
}

func TestConfig_FormsCannotBeMixed(t *testing.T) {
	if err := loadErr(t, "XCache:\n  MaxCost: 1\n  Clients:\n    a: {MaxCost: 2}\n"); err == nil {
		t.Fatal("mixing the two forms must fail")
	}
}

func TestConfig_NoConfigNoInstances(t *testing.T) {
	if c := load(t, "# no XCache block\n"); len(c.Clients) != 0 {
		t.Errorf("without it configured no cache may be built, got=%v", c.Clients)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	if err := ok.Validate(); err != nil {
		t.Errorf("the default configuration must be valid: %v", err)
	}
	for name, mutate := range map[string]func(*ClientConfig){
		"NumCounters is 0": func(c *ClientConfig) { c.NumCounters = 0 },
		"MaxCost is 0":     func(c *ClientConfig) { c.MaxCost = 0 },
		"BufferItems is 0": func(c *ClientConfig) { c.BufferItems = 0 },
		// ristretto drops a write with ttl<0 outright, so a negative value stores nothing at all
		"DefaultTTL is negative": func(c *ClientConfig) { c.DefaultTTL = -time.Second },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s must be an error", name)
		}
	}
}

func TestNew_ReturnsNativeCache(t *testing.T) {
	cache, closer, err := New(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	cache.SetWithTTL("k", 42, 1, time.Minute)
	cache.Wait() // writes take effect asynchronously through a ring buffer, so reading back deterministically needs a wait
	if v, ok := cache.Get("k"); !ok || v != 42 {
		t.Errorf("the value just written must be read back, got=%v ok=%v", v, ok)
	}
}

func TestSet_UsesConfiguredDefaultTTL(t *testing.T) {
	// This is the whole value of the package-level Set: the cost and the TTL do not have to be written every time.
	withInstances(t, map[string]ClientConfig{DefaultName: func() ClientConfig {
		c := DefaultClientConfig()
		c.DefaultTTL = 90 * time.Second
		return c
	}()})

	Set("k", "v")
	C().Wait()

	ttl, ok := C().GetTTL("k")
	if !ok {
		t.Fatal("the key just written must be readable")
	}
	if ttl > 90*time.Second || ttl < 80*time.Second {
		t.Errorf("the TTL from the configuration must be used, got=%v", ttl)
	}
	if got := DefaultTTL(); got != 90*time.Second {
		t.Errorf("DefaultTTL must return the configured value, got=%v", got)
	}
}

func TestGetSetDel(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})

	Set("k", "v")
	C().Wait()
	if v, ok := Get[string]("k"); !ok || v != "v" {
		t.Errorf("the value just written must be read back, got=%v ok=%v", v, ok)
	}

	Del("k")
	C().Wait()
	if _, ok := Get[string]("k"); ok {
		t.Error("it must no longer be readable after being deleted")
	}
}

func TestGet_TypeMismatchIsMiss(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	var b bytes.Buffer
	old := config.DebugOut
	config.DebugOut = &b
	t.Cleanup(func() { config.DebugOut = old })
	t.Setenv(config.DebugEnvKey, "1")

	type user struct{ Name string }
	Set("u", user{Name: "a"})
	C().Wait()

	if u, ok := Get[*user]("u"); ok || u != nil {
		t.Errorf("a stored user read as a *user must count as a miss, got=%v ok=%v", u, ok)
	}
	if !strings.Contains(b.String(), `"u"`) {
		t.Errorf("with XONE_DEBUG on it must say the types do not match, got=%q", b.String())
	}
	if u, ok := Get[user]("u"); !ok || u.Name != "a" {
		t.Errorf("a matching type must hit, got=%v ok=%v", u, ok)
	}
	if v, ok := Get[any]("u"); !ok || v.(user).Name != "a" {
		t.Errorf("Get[any] accepts anything, got=%v ok=%v", v, ok)
	}
}

func TestSetWithTTL(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})

	SetWithTTL("k", "v", 10*time.Second)
	C().Wait()
	ttl, ok := C().GetTTL("k")
	if !ok || ttl > 10*time.Second {
		t.Errorf("the TTL passed in must be used, got=%v ok=%v", ttl, ok)
	}
}

func TestC_PanicsWhenMissing(t *testing.T) {
	withInstances(t, nil)
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

func TestDefaultTTL_PanicsOnWrongNameInsteadOfNeverExpire(t *testing.T) {
	// In ristretto 0 means "never expires": quietly getting it from a misspelled name would make every entry
	// written with that TTL never expire.
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig()})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "session") {
			t.Errorf("it must panic as C() does and list the configured instance names, got=%v", msg)
		}
	}()
	DefaultTTL("typo")
}

func TestSet_PanicsWithoutDefaultInstance(t *testing.T) {
	// The package-level Set takes another path to the instance, so this layer of protection must not be missed there.
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig()})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "session") {
			t.Errorf("the configured instance names must be listed, got=%v", msg)
		}
	}()
	Set("k", "v")
}

func TestHasNames(t *testing.T) {
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig(), "page": DefaultClientConfig()})
	if !Has("page") || Has("nope") || Has() {
		t.Error("Has must report truthfully whether something was configured")
	}
	if got := Names(); len(got) != 2 || got[0] != "page" {
		t.Errorf("Names must be sorted by name, got=%v", got)
	}
}

func TestRegister_MatchesFramework(t *testing.T) {
	// This is the only wire between this package and the framework: a missing hook registration or a wrong stage
	// shows up as "the config has no effect" or "the instance is ready later than what uses it", and nothing else catches it.
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xcache" {
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
		if e.Pkg == "github.com/jupiter-eng/xone/xcache" {
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
	c := Config{Clients: map[string]ClientConfig{"a": DefaultClientConfig(), "b": DefaultClientConfig()}}

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
	bad := DefaultClientConfig()
	bad.MaxCost = -1
	cfg := Config{Clients: map[string]ClientConfig{"a": DefaultClientConfig(), "z": bad}}

	if err := initComponent(t, cfg); err == nil {
		t.Fatal("an invalid configuration must be an error")
	} else if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("the error must name the instance, got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be published on failure, got=%v", got)
	}
}

func TestInitAll_NoopWithoutConfig(t *testing.T) {
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

func TestNew_MaxCostIsEntryCount(t *testing.T) {
	// ristretto adds the 56 bytes of per-entry internal overhead to the cost by default, so a write with cost=1
	// really occupies 57. Without turning that off, MaxCost=2000 would hold only thirty-odd entries -- the number
	// in the configuration and the real capacity more than fifty times apart, with nothing anywhere mentioning it.
	c := DefaultClientConfig()
	c.MaxCost = 2000
	c.NumCounters = 20000
	cache, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	const n = 2000
	for i := range n {
		cache.SetWithTTL(strconv.Itoa(i), i, 1, time.Hour)
	}
	cache.Wait()

	live := 0
	for i := range n {
		if _, ok := cache.Get(strconv.Itoa(i)); ok {
			live++
		}
	}
	// The admission policy drops some of them anyway, so only the same order of magnitude is required here.
	if live < n/2 {
		t.Errorf("with MaxCost=%d and cost=1 per entry it must hold close to %d entries, only %d remain", c.MaxCost, n, live)
	}
}

// initComponent walks the path the framework really takes: build the instances one by one from this config.
// Everything about building several of them (ordering, rollback on failure, panic isolation) lives on that path.
func initComponent(t *testing.T, c Config) error {
	t.Helper()
	t.Cleanup(func() { _ = reg.Close() })
	return install(context.Background(), c)
}

func TestInitXCache_NoSectionBuildsNothing(t *testing.T) {
	// There is no reason to open a local cache by default: not configured means not used.
	// Without this check, every process would swallow a chunk of memory for nothing.
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXCache(context.Background()); err != nil {
		t.Fatalf("not configuring it must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be built, got=%v", got)
	}
}

func TestInitXCache_EmptySectionBuildsNothing(t *testing.T) {
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XCache:\n")

	if err := initXCache(context.Background()); err != nil {
		t.Fatalf("an empty block must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be built, got=%v", got)
	}
}

func TestInitXCache_BadConfigFailsStartup(t *testing.T) {
	// If a misspelled field were silently ignored, the caller would keep believing the setting took.
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XCache:\n  MaxCos: 500\n")

	if err := initXCache(context.Background()); err == nil {
		t.Fatal("a misspelled field must fail startup")
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("no instance may be left behind on failure, got=%v", got)
	}
}

func TestInitXCache_BuildsFromConfigAndClosesCleanly(t *testing.T) {
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XCache:\n  Clients:\n    read:\n      MaxCost: 500\n    write:\n      MaxCost: 500\n")

	if err := initXCache(context.Background()); err != nil {
		t.Fatalf("it must build: %v", err)
	}
	if got := Names(); len(got) != 2 {
		t.Fatalf("there must be two instances, got=%v", got)
	}

	if err := closeXCache(context.Background()); err != nil {
		t.Errorf("closing must not be an error: %v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("an instance is still reachable after closing, so a caller would touch a closed cache, got=%v", got)
	}
}

func TestCloseXCache_SafeWithoutBuild(t *testing.T) {
	// The stop hook is called even when this package was never configured, so it must not blow up here.
	if err := closeXCache(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestClose_NoCrashWithConcurrentReadWrite(t *testing.T) {
	// ristretto v2.4.2's Close closes setBuf first and sets isClosed last, so a Set / Del running concurrently
	// with it sends on a closed channel; the policy's itemsCh is closed the same way, so a concurrent Get blows
	// up too. Callers holding the native *Cache cannot be stopped from doing that, so the closing step itself has
	// to be safe against concurrent reads and writes.
	// Measured: calling Close directly panicked 750 goroutines over 100 rounds, with -race reporting data races.
	// Several blow up every round, so 20 rounds is enough: the mutation "call ristretto's Close on shutdown" was caught on all 5 consecutive runs.
	// With opsPerWorker capping the writer goroutines, this one alone takes about 2.4s under -race (including compilation);
	// the mutation "call ristretto's Close on shutdown" is still caught every time (3 consecutive runs).
	const rounds, workers = 20, 8
	// The most each writer goroutine writes. ristretto's Clear only returns once setBuf is empty at some moment
	// (the drain loop in v2.4.2 cache.go), and with 8 goroutines writing nonstop while only being stopped after
	// Close returns, an idle multi-core machine writes faster than it clears and Close never returns (measured:
	// 3 of 3 solo runs hung, while a busy machine passed). Stopping after this many writes lets Close finish, and there are still concurrent writes during and after shutdown.
	const opsPerWorker = 5000
	small := DefaultClientConfig()
	small.NumCounters, small.MaxCost = 1000, 100 // the default million counters would allocate 4MB per round, which is too slow
	var panics atomic.Int64
	for i := 0; i < rounds; i++ {
		cache, closer, err := New(small)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if recover() != nil {
						panics.Add(1)
					}
				}()
				for n := 0; n < opsPerWorker; n++ {
					select {
					case <-stop:
						return
					default:
					}
					k := strconv.Itoa(n % 64)
					cache.SetWithTTL(k, n, 1, time.Minute)
					cache.Get(k)
					if n%16 == 0 {
						cache.Del(k)
					}
				}
			}()
		}
		time.Sleep(200 * time.Microsecond)
		if err := closer.Close(); err != nil {
			t.Fatalf("closing must not be an error: %v", err)
		}
		time.Sleep(200 * time.Microsecond) // writes still happening after closing count too
		close(stop)
		wg.Wait()
	}
	if n := panics.Load(); n > 0 {
		t.Fatalf("with concurrent reads and writes during shutdown, %d goroutines panicked", n)
	}
}

func TestClose_ReleasesEntries(t *testing.T) {
	// Closing does not go through ristretto's Close (the reason is in New), but what is in the cache has to be
	// released: otherwise a stopped instance holds MaxCost worth of entries until the process exits.
	cache, closer, err := New(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	cache.Set("k", 1, 1)
	cache.Wait()
	if _, ok := cache.Get("k"); !ok {
		t.Fatal("precondition: the value just written must be readable")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get("k"); ok {
		t.Error("the entries must be cleared after closing")
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
	bad.MaxCost = -1
	err := initComponent(t, Config{Clients: map[string]ClientConfig{"a": bad}})
	assertOneFrame(t, err, "xcache", "config")
	if !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("the error must name the instance, got %v", err)
	}
}

func TestInitXCache_CSaysNotConfiguredRatherThanTooEarly(t *testing.T) {
	// Even with nothing configured the registry must know the start hook ran. Otherwise C() would report "not
	// configured" as "called too early", sending the caller to look at call timing when the config file is what needs checking.
	t.Cleanup(func() { _ = closeXCache(context.Background()) })
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXCache(context.Background()); err != nil {
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

// The conveniences each fetch the default instance from the registry first and then hand over to ristretto.
func BenchmarkGet_Hit(b *testing.B) {
	withInstances(b, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	Set("k", "v")
	C().Wait()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Get[string]("k")
	}
}

func BenchmarkGet_Hit_Parallel(b *testing.B) {
	withInstances(b, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	Set("k", "v")
	C().Wait()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			Get[string]("k")
		}
	})
}

func BenchmarkSet_DefaultTTL(b *testing.B) {
	withInstances(b, map[string]ClientConfig{DefaultName: DefaultClientConfig()})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Set("k", "v")
	}
}

func TestConfig_InvalidValuesFailAtLoad(t *testing.T) {
	// Caught while the config is read, with the instance named in the error; not discovered only at New.
	err := loadErr(t, "XCache:\n  Clients:\n    hot: {MaxCost: -1}\n")
	if err == nil {
		t.Fatal("a negative MaxCost must fail while the config is read")
	}
	if !strings.Contains(err.Error(), "hot") || !strings.Contains(err.Error(), "MaxCost") {
		t.Errorf("the error must name the instance and the field, got=%v", err)
	}
	if err := loadErr(t, "XCache:\n  DefaultTTL: -1s\n"); err == nil {
		t.Fatal("a negative DefaultTTL must fail while the config is read")
	}
}

func TestNew_MetricFlagPassedToRistretto(t *testing.T) {
	// ristretto does not count by default (Metrics is nil), and without passing it down even Metric: true exports nothing.
	for _, on := range []bool{true, false} {
		c := DefaultClientConfig()
		c.Metric = on
		cache, closer, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		if got := cache.Metrics != nil; got != on {
			t.Errorf("with Metric=%v ristretto's counting must be %v, got=%v", on, on, got)
		}
		closer.Close()
	}
}

func TestCacheCollector_ExportsRistrettoCounters(t *testing.T) {
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	cfg := DefaultClientConfig()
	cfg.MaxCost = 1000
	cache, cc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	for i := 0; i < 3; i++ {
		cache.SetWithTTL("k"+strconv.Itoa(i), i, 1, 0)
	}
	cache.Wait()
	cache.SetWithTTL("k0", "new", 1, 0) // update an existing key
	cache.Wait()
	cache.Get("k0")
	cache.Get("k1")
	cache.Get("nope")
	cache.Del("k2") // an explicit delete, which ristretto records in keys_evicted
	cache.Wait()

	m.Registry.MustRegister(newCacheCollector("demo", nil, func() map[string]*Cache { return map[string]*Cache{"hot": cache} }))
	out := testkit.Scrape(m.Handler)
	for _, want := range []string{
		`demo_cache_hits_total{name="hot"} 2`,
		`demo_cache_misses_total{name="hot"} 1`,
		`demo_cache_keys_added_total{name="hot"} 3`,
		`demo_cache_keys_updated_total{name="hot"} 1`,
		`demo_cache_keys_evicted_total{name="hot"} 1`,
		`demo_cache_sets_dropped_total{name="hot"} 0`,
		`demo_cache_sets_rejected_total{name="hot"} 0`,
		`demo_cache_cost{name="hot"} 2`,
		`demo_cache_max_cost{name="hot"} 1000`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the export must contain %s\ngot=\n%s", want, out)
		}
	}
}

func TestCacheCollector_ExpiredCountsAsKeysEvicted(t *testing.T) {
	// The documentation says keys_evicted includes TTL expiries; this pins it down, so a ristretto upgrade that changes the behavior goes red first.
	cache, cc, err := New(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	cache.SetWithTTL("k", 1, 1, 50*time.Millisecond)
	cache.Wait()
	// ristretto cleans up expiries by bucket (v2.4.2: 5s buckets, a sweep every 2.5s), so wait until it has swept.
	deadline := time.Now().Add(10 * time.Second)
	for cache.Metrics.KeysEvicted() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := cache.Metrics.KeysEvicted(); got != 1 {
		t.Errorf("a key cleaned up on expiry must count towards keys_evicted, got=%d", got)
	}
}

func TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall(t *testing.T) {
	// The collector is a single process-wide object that walks every instance when scraped: without consulting
	// each instance's own switch, Metric: false is an empty phrase. The second round is another lifecycle in the
	// same process -- xmetric has a new Registry, and the collector has to be attached to it or every metric is lost.
	on, off := DefaultClientConfig(), DefaultClientConfig()
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
		if !strings.Contains(out, `cache_hits_total{name="on"}`) {
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

func TestInstall_LogsCarryInstanceName(t *testing.T) {
	// With several caches configured, the parameters alone do not say which one it is.
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	hot := DefaultClientConfig()
	hot.MaxCost = 123
	if err := initComponent(t, Config{Clients: map[string]ClientConfig{"hot": hot}}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != "xcache created" {
			continue
		}
		found = true
		if rec["name"] != "hot" || rec["max_cost"] != float64(123) {
			t.Errorf("the log must write the instance name and its parameters, got=%v", rec)
		}
	}
	if !found {
		t.Errorf("there must be one xcache created record per instance built\ngot=\n%s", buf.String())
	}
}
