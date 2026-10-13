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

// Cache 就是原生的 ristretto 缓存，这里只是给它起个短名字。
//
// 值类型是 any：配置驱动的全局实例没法带上业务类型。
// 想要类型安全就自己 ristretto.NewCache[string, *User] 建一个，本包不挡路。
type Cache = ristretto.Cache[string, any]

// New 按配置建一个缓存实例，不触碰任何全局变量
func New(cfg ClientConfig) (*Cache, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, xerror.Newf("xcache", "config", "invalid config: %w", err)
	}

	c, err := ristretto.NewCache(&ristretto.Config[string, any]{
		NumCounters: cfg.NumCounters,
		MaxCost:     cfg.MaxCost,
		BufferItems: cfg.BufferItems,
		// ristretto 默认会把每条的内部开销（56 字节）加进 cost，
		// 于是 cost=1 的写入实际占 57。MaxCost: 100000 配出来的缓存
		// 只能存下一千七百多条，配置里写的数字和实际容量差着五十多倍，
		// 而且没有任何地方会提到这件事。关掉它，cost 才是 cost
		IgnoreInternalCost: true,
		// ristretto 默认不计数，Metrics 字段是 nil，命中率无从谈起
		Metrics: cfg.Metric,
	})
	if err != nil {
		return nil, nil, xerror.Newf("xcache", "new", "create cache: %w", err)
	}
	return c, closerFunc(c.Clear), nil
}

// 返回的 io.Closer 调的是 Clear，不是 Close，这是有意的。
//
// ristretto v2.4.2 的 Close 要求此刻没有任何并发调用：它先 close(setBuf)、
// 最后才置 isClosed，于是和它并发的 Set / Del 会 send on closed channel；
// policy 的 itemsCh 也是这么关的，并发的 Get 同样会炸（实测 100 轮并发读写
// 里 750 个协程 panic，-race 同时报数据竞争）。
//
// 我们交出去的是原生 *Cache，谁还攥着它、它的后台协程什么时候停，本包都
// 不知道——停止钩子跑的时候，一个没来得及退出的消费协程再写一次，进程就
// 在关闭途中崩掉，排在后面的组件一个都关不成。包一层带锁的类型能拦住，
// 但那就不再是原生对象了。
//
// Clear 对并发读写是安全的（同一组并发下 0 次 panic、-race 干净），
// 而且把条目全部放掉。代价是 ristretto 的两个后台协程和一个 ticker
// 留到进程退出，连同计数器占的内存（默认 NumCounters 实测约 4.3MB）。
// 框架的停止钩子本来就只在进程退出前跑，这个代价换的是「关闭不会崩」。
// 自己用 New 建、并且确定所有调用方都已经停下的，可以直接调 cache.Close()
// 把这部分也收回来。

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// ---- 全局实例 ----

// instance 一个缓存实例连同它自己的默认 TTL 和 Metric 开关。
// 带着 Metric 的理由同 xredis：collector 是进程级的一个，不带着这一位
// 就分不出哪个实例配了 Metric: false。
type instance struct {
	cache  *Cache
	ttl    time.Duration
	metric bool
}

// C 取一个缓存实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由见 xclient.Registry.Get。
func C(name ...string) *Cache { return reg.Get(name...).cache }

// Has 报告指定实例是否已配置，供可选依赖判断
func Has(name ...string) bool { return reg.Has(name...) }

// Names 返回已配置的实例名
func Names() []string { return reg.Names() }

// DefaultTTL 返回指定实例配置的默认过期时间。取不到实例时 panic，与 C() 一致。
//
// 不返回 0：0 在 ristretto 里是「永不过期」，名字写错时静默拿到它，
// 写进去的每一条就都不会过期了。
func DefaultTTL(name ...string) time.Duration { return reg.Get(name...).ttl }

// ---- 默认实例上的便利写法 ----
//
// 只作用于名为 default 的实例。具名实例用 C("name") 拿原生对象操作。

// Get 从默认实例读一个值，按 V 取出来，不用自己断言：
//
//	u, ok := xcache.Get[*User]("user:1")
//
// 存的类型和 V 对不上时当作没命中，返回零值和 false——缓存本来就允许不命中，
// 调用方照常回源，不会拿到一个错类型的值。典型的是存的 User、取的 *User：
// 这种写法永远不命中，没有别的迹象，所以 XONE_DEBUG 开着时打一行出来。
// 什么类型都收就写 Get[any]。
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

// Set 往默认实例写一个值，用配置里的 DefaultTTL，cost 为 1。
//
// 返回值是「有没有被收下」，不是「有没有存进去」，这两件事在 ristretto 里不一样：
//
//   - 返回 true 只说明写入请求进了缓冲区。准入策略仍可能判定这个键不值得留，
//     然后悄悄丢掉它，不会有任何返回值或日志提到。
//   - 返回 true 之后立刻 Get 也可能读不到：写入走环形缓冲异步生效，
//     要确定性地读到刚写的值（多半是测试里）得先 Wait。
//   - 返回 false 说明缓冲区满了、这次写入被直接丢弃，是瞬时状态，可以重试。
//
// 所以它适合用来观察「缓存是不是在丢写入」，不适合判断某个键此刻在不在缓存里
// ——那只有 Get 能回答。缓存本来就允许丢，正常业务路径忽略返回值即可。
func Set(key string, value any) bool {
	inst := reg.Get()
	return inst.cache.SetWithTTL(key, value, 1, inst.ttl)
}

// SetWithTTL 往默认实例写一个值并指定过期时间，cost 为 1。返回值含义同 Set。
func SetWithTTL(key string, value any, ttl time.Duration) bool {
	return C().SetWithTTL(key, value, 1, ttl)
}

// Del 从默认实例删一个键
func Del(key string) { C().Del(key) }

// ---- 登记 ----

// reg 具名实例注册表。取实例、找不到时的报错、关闭时摘干净，
// 这些语义在 xgorm / xredis / xcache 之间必须一致，所以共用一份实现。
var reg = xclient.NewRegistry[instance]("xcache", ConfigKey)

func init() {
	xhook.BeforeStart(initXCache, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXCache) // 档位跟着上面那个启动钩子
}

// initXCache 读配置，按名字把实例挨个建出来。
//
// 没配这一块就一个都不建：xcache 是可选依赖，没配不该让服务起不来。
// 但 Build 照样走一遍，注册表由此知道启动钩子跑过了——之后 C() 取不到时
// 报的是「没配」，而不是「调早了」。
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

// install 按配置把实例挨个建出来
func install(ctx context.Context, c Config) error {
	if err := xclient.Build(ctx, reg, c.Clients, build); err != nil {
		return err
	}
	installMetrics() // 一个实例都没开 Metric 时它什么都不导出，不必特判
	slog.Info("xcache ready", "instances", reg.Names())
	return nil
}

// closeXCache 摘掉全部实例并逆序关闭。
//
// 启动钩子失败时它不会被调到——停止钩子只在和它配对的启动钩子成功之后才执行，
// 所以这里不必处理「还没建起来」。
func closeXCache(context.Context) error { return reg.Close() }

// build 建一个实例。New 不收 ctx（本地缓存不建连、不会把人卡住），
// 所以这里补一个形参把它接上；同时把这个实例的默认 TTL 和 Metric 开关一起带上。
// 日志写上实例名：配了好几个缓存时，只写参数分不出是哪一个
func build(_ context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	cache, closer, err := New(c)
	if err != nil {
		return instance{}, nil, err
	}
	slog.Info("xcache created", "name", name, "max_cost", c.MaxCost, "default_ttl", c.DefaultTTL, "metric", c.Metric)
	return instance{cache: cache, ttl: c.DefaultTTL, metric: c.Metric}, closer, nil
}

// installMetrics 把缓存 collector 挂到当前的 Registry 上。
//
// 每次 install 都挂、不用 sync.Once，理由同 xredis：xmetric 重装之后是新的
// Registry，Once 会把这第二次挡掉；同一个 Registry 上重复挂由 xmetric.Register
// 复用已有的那个。
//
// 不让启动失败：指标导不出去是可观测性问题，不该拦住服务起来。
func installMetrics() {
	if _, err := xmetric.RegisterAs(newCacheCollector(xmetric.Namespace(), xmetric.ConstLabels(), metricCaches)); err != nil {
		slog.Error("xcache failed to register cache metrics", "error", err)
	}
}

// metricCaches 开了 Metric 的各个实例
func metricCaches() map[string]*Cache {
	out := map[string]*Cache{}
	for name, inst := range reg.All() {
		if inst.metric {
			out[name] = inst.cache
		}
	}
	return out
}
