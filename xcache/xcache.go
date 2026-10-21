package xcache

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/dgraph-io/ristretto/v2"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
	"github.com/jupiter-eng/xone/xmetric"
)

// Cache is the native ristretto cache; this is only a short name for it.
//
// The value type is any: a configuration-driven global instance cannot carry a business type.
// For type safety, build one with ristretto.NewCache[string, *User]; this package does not stand in the way.
type Cache = ristretto.Cache[string, any]

// New builds a cache instance from the configuration and touches no globals.
func New(cfg ClientConfig) (*Cache, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xcache", "config", "invalid config: %w", err)
	}

	c, err := ristretto.NewCache(&ristretto.Config[string, any]{
		NumCounters: cfg.NumCounters,
		MaxCost:     cfg.MaxCost,
		BufferItems: cfg.BufferItems,
		// ristretto adds the per-entry internal overhead (56 bytes) to the cost by default, so a write with
		// cost=1 really occupies 57. A cache configured with MaxCost: 100000 would then hold only a little over
		// seventeen hundred entries -- the number in the configuration and the real capacity are more than fifty
		// times apart, and nothing anywhere mentions it. Turned off, cost is cost.
		IgnoreInternalCost: true,
		// ristretto does not count by default, its Metrics field is nil, and there is no hit rate to speak of.
		Metrics: cfg.Metric,
	})
	if err != nil {
		return nil, nil, xerror.Newf("xcache", "new", "create cache: %w", err)
	}
	return c, closerFunc(c.Clear), nil
}

// The io.Closer returned calls Clear, not Close, and that is deliberate.
//
// ristretto v2.4.2's Close requires that no call is in flight at that moment: it closes setBuf first and sets
// isClosed last, so a Set / Del running concurrently with it sends on a closed channel; the policy's itemsCh
// is closed the same way, so a concurrent Get blows up too (measured: 750 goroutines panicking over 100 rounds
// of concurrent reads and writes, with -race reporting data races at the same time).
//
// What is handed out is the native *Cache, and this package does not know who still holds it or when its
// background goroutines stop -- a consumer goroutine that has not finished writing once more while the stop
// hook runs would crash the process midway through shutdown, and nothing queued behind it would be closed at
// all. A wrapper type with a lock would stop that, but then it would no longer be the native object.
//
// Clear is safe against concurrent reads and writes (0 panics under the same concurrency, and -race clean) and
// releases every entry. The cost is that ristretto's two background goroutines and one ticker live until the
// process exits, together with the memory its counters occupy (measured at about 4.3MB with the default
// NumCounters). The framework's stop hooks only run just before the process exits anyway, and this cost buys
// "shutdown does not crash". Anyone building one with New who is sure every caller has stopped can call
// cache.Close() directly and reclaim that too.

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// ---- global instances ----

// instance is one cache instance together with its own default TTL and Metric switch.
// Carrying Metric has the same reason as in xredis: the collector is a single process-wide object, and without
// this bit there would be no way to tell which instance was configured with Metric: false.
type instance struct {
	cache  *Cache
	ttl    time.Duration
	metric bool
}

// C fetches a cache instance; without an argument, the one named default.
//
// If there is none it panics, for the reason given in xclient.Registry.Get.
func C(name ...string) *Cache { return reg.Get(name...).cache }

// Has reports whether the given instance is configured, for checking an optional dependency.
func Has(name ...string) bool { return reg.Has(name...) }

// Names returns the configured instance names.
func Names() []string { return reg.Names() }

// DefaultTTL returns the default expiry configured for the given instance. It panics when there is no such instance, as C() does.
//
// It does not return 0: in ristretto 0 means "never expires", and quietly getting it from a misspelled name
// would make every entry written never expire.
func DefaultTTL(name ...string) time.Duration { return reg.Get(name...).ttl }

// ---- conveniences on the default instance ----
//
// They act only on the instance named default. A named instance is operated through the native object from C("name").

// Get reads a value from the default instance and takes it as V, with no assertion needed:
//
//	u, ok := xcache.Get[*User]("user:1")
//
// When the stored type does not match V it counts as a miss and the zero value and false are returned -- a cache
// is allowed to miss, so the caller falls back to the source as usual and never gets a value of the wrong type.
// The classic case is storing a User and reading a *User: that never hits, with no other sign, which is why a
// line is printed when XONE_DEBUG is on. To accept any type, write Get[any].
func Get[V any](key string) (V, bool) {
	var zero V
	v, ok := C().Get(key)
	if !ok {
		return zero, false
	}
	typed, ok := v.(V)
	if !ok {
		config.Debugf("xcache: key %q holds %T, Get asked for %T, treated as a miss", key, v, zero)
		return zero, false
	}
	return typed, true
}

// Set writes a value to the default instance, with the DefaultTTL from the configuration and a cost of 1.
//
// The return value is "was it accepted", not "was it stored", and in ristretto those are two different things:
//
//   - true only means the write request entered the buffer. The admission policy may still decide the key is
//     not worth keeping and drop it quietly, with no return value or log mentioning it.
//   - A Get immediately after a true may still not find it: writes take effect asynchronously through a ring
//     buffer, and reading back what was just written deterministically (mostly in tests) needs a Wait first.
//   - false means the buffer was full and this write was dropped outright; that is transient and can be retried.
//
// So it is useful for watching whether the cache is dropping writes, and not for deciding whether a key is in
// the cache right now -- only Get can answer that. A cache is allowed to drop, so an ordinary business path can ignore the return value.
func Set(key string, value any) bool {
	inst := reg.Get()
	return inst.cache.SetWithTTL(key, value, 1, inst.ttl)
}

// SetWithTTL writes a value to the default instance with a given expiry and a cost of 1. The return value means the same as Set's.
func SetWithTTL(key string, value any, ttl time.Duration) bool {
	return C().SetWithTTL(key, value, 1, ttl)
}

// Del deletes a key from the default instance.
func Del(key string) { C().Del(key) }

// ---- registration ----

// reg is the registry of named instances. Fetching an instance, the error when there is none, and clearing out
// on close must mean the same across xgorm / xredis / xcache, so they share one implementation.
var reg = xclient.NewRegistry[instance]("xcache", ConfigKey)

func init() {
	xhook.BeforeStart(initXCache, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXCache) // the stage follows the start hook above
}

// initXCache reads the configuration and builds the instances one by one by name.
//
// Without this block configured it builds none: xcache is an optional dependency, and not configuring it must
// not keep the service from starting. Build still runs, so the registry learns that the start hook ran -- after
// which a C() that finds nothing reports "not configured" rather than "called too early".
func initXCache(ctx context.Context) error {
	if !xconfig.Has(ConfigKey) {
		return xclient.Build(ctx, reg, nil, build)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return install(ctx, c)
}

// install builds the instances one by one from the configuration.
func install(ctx context.Context, c Config) error {
	if err := xclient.Build(ctx, reg, c.Clients, build); err != nil {
		return err
	}
	installMetrics() // with no instance enabling Metric it exports nothing, so no special case is needed
	slog.Info("xcache ready", "instances", reg.Names())
	return nil
}

// closeXCache removes every instance and closes them in reverse.
//
// It is never called when the start hook failed -- a stop hook only runs after the start hook it pairs with
// succeeded, so it does not have to handle "it was never built".
func closeXCache(context.Context) error { return reg.Close() }

// build builds one instance. New takes no ctx (a local cache connects to nothing and blocks nobody), so a
// parameter is added here to fit the signature; it also carries this instance's default TTL and Metric switch.
// The log carries the instance name: with several caches configured, the parameters alone do not say which one it is.
func build(_ context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	cache, closer, err := New(c)
	if err != nil {
		return instance{}, nil, err
	}
	slog.Info("xcache created", "name", name, "max_cost", c.MaxCost, "default_ttl", c.DefaultTTL, "metric", c.Metric)
	return instance{cache: cache, ttl: c.DefaultTTL, metric: c.Metric}, closer, nil
}

// installMetrics attaches the cache collector to the current Registry.
//
// It attaches on every install rather than using a sync.Once, for the same reason as in xredis: after xmetric is
// reinstalled the Registry is a new one and a Once would block this second attachment, while attaching twice to
// the same Registry is handled by xmetric.Register reusing the existing one.
//
// It does not fail startup: metrics not being exported is an observability problem and must not keep the service from coming up.
func installMetrics() {
	if _, err := xmetric.RegisterAs(newCacheCollector(xmetric.Namespace(), xmetric.ConstLabels(), metricCaches)); err != nil {
		slog.Error("xcache failed to register cache metrics", "error", err)
	}
}

// metricCaches are the instances with Metric enabled.
func metricCaches() map[string]*Cache {
	out := map[string]*Cache{}
	for name, inst := range reg.All() {
		if inst.metric {
			out[name] = inst.cache
		}
	}
	return out
}
