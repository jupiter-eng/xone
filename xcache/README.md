# xcache

本地缓存：拿到的是原生 `*ristretto.Cache`（ristretto v2），框架按配置建好。

- 包级 `Get` / `Set` 走 `default` 实例、用配置里的 `DefaultTTL`，cost 固定为 1
- `Get[V]` 按类型取出来，类型对不上当作没命中
- 单实例、多实例都行，`xcache.C("name")` 按名字取原生对象
- `MaxCost` 只算你给的 cost，包级写入时就是最多存多少条
- 命中率等指标 `cache_*`，按实例开关

## 快速上手

```yaml
# conf/application.yml
XCache:
  MaxCost: 10000           # 包级 Set 的 cost 固定为 1，所以就是最多 1 万条
  NumCounters: 100000      # 建议取预期条目数的 10 倍
  DefaultTTL: 1m
```

```go
type User struct{ ID, Name string }

// GetUser 先查本地缓存，没有再回源，回源的结果按 DefaultTTL 缓存
func GetUser(ctx context.Context, id string, load func(context.Context, string) (*User, error)) (*User, error) {
	key := "user:" + id
	if u, ok := xcache.Get[*User](key); ok { // 类型对不上当作没命中
		return u, nil
	}
	u, err := load(ctx, id)
	if err != nil {
		return nil, err
	}
	xcache.Set(key, u)
	return u, nil
}

// 要自己定 cost 和 TTL 就用原生的 ristretto：SetWithTTL(key, value, cost, ttl)
func PutHot(key string, v any) {
	xcache.C().SetWithTTL(key, v, 1, 10*time.Second)
}
```

## 配置

```yaml
XCache:
  NumCounters: 1000000     # 频率计数器个数，建议取预期条目数的 10 倍
  MaxCost: 100000          # 总成本上限；包级 Set 的 cost 固定为 1，所以等于条目数
  BufferItems: 64          # Get 的内部缓冲区大小，官方建议值
  DefaultTTL: 5m           # 包级 Set 用的过期时间；0 = 永不过期，负数启动失败
  Metric: true             # 命中率等指标 cache_*，按实例生效
```

**多实例**写在 `Clients` 下，每个实例的字段同上，没写的用上面的默认值：

```yaml
XCache:
  Clients:
    default: {MaxCost: 100000}
    session: {MaxCost: 10000, DefaultTTL: 30m}
```

`xcache.C()` 取 `default`，`xcache.C("session")` 取另一个；包级 `Get` / `Set` 只作用于 `default`。两种写法不能混用。
没写 `XCache` 这一块就一个实例都不建。

## API

| 函数 | 说明 |
|---|---|
| `Get[V](key) (V, bool)` | 从 `default` 读，按 `V` 取出来；存的类型和 `V` 对不上当作没命中（`XONE_DEBUG` 开着时往 stderr 打一行 `[xone debug]`） |
| `Set(key, value) bool` | 写进 `default`，用配置的 `DefaultTTL`，cost 为 1。返回的是「有没有被收下」，不是「存进去没有」 |
| `SetWithTTL(key, value, ttl) bool` | 同上，自己指定过期时间 |
| `Del(key)` | 从 `default` 删一个键 |
| `C(name ...string) *Cache` | 取原生 `*ristretto.Cache`，不带参数取 `default`。取不到直接 panic |
| `Has(name ...string) bool` | 实例配了没有，可选依赖先判断 |
| `Names() []string` | 配了哪些实例 |
| `DefaultTTL(name ...string) time.Duration` | 实例配置的默认过期时间；名字写错时和 `C` 一样 panic，不返回 0（0 是永不过期） |
| `New(cfg) (*Cache, io.Closer, error)` | 纯构造器：不碰全局，离开框架也能用；返回的 Closer 调的是 `Clear` |

## 注意事项

- **`MaxCost` 只算你给的 cost**，不含 ristretto 每条 56 字节的内部开销。按字节记 cost 时把这部分算进自己的预算。
- **停止时只 `Clear` 不 `Close`**：`Close` 和并发读写一起跑会 panic。自己 `xcache.New` 建的、确定没人在用了，可以自己 `Close()`。
- **TTL 到期不是到点就移除**，要过几秒；`DefaultTTL` 为负启动失败，0 是永不过期。
- **`Set` 返回 true 之后立刻 `Get` 也可能读不到**：写入异步生效，准入策略也可能悄悄丢掉它。缓存本来就允许丢，正常业务忽略返回值即可。
- **`Metric`（默认开）有内存开销**：每实例约 84KB，写过 10 万个以上不同的键后多 8–12MB。

数字和理由见[「行为与实测」](#行为与实测)。

## 可观测

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `xcache created` | INFO | `name`、`max_cost`、`default_ttl`、`metric` |
| `xcache ready` | INFO | `instances` |

日志的全局约定（`trace_id` 注入、`xlog.AddKV`、框架的启停日志）见 [`docs/observability.md`](../docs/observability.md#日志)。

### 指标

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `cache_hits_total` / `cache_misses_total` / `cache_keys_added_total` / `cache_keys_updated_total` / `cache_keys_evicted_total` / `cache_sets_dropped_total` / `cache_sets_rejected_total` | counter | `name` | xcache，`XCache.Metric`，按实例 |
| `cache_cost` / `cache_max_cost` | gauge | `name` | xcache |

- **`cache_keys_evicted_total`** 不只是容量满了被挤掉：显式 `Del` 和 TTL 到期被清理的也算在里面。
- 命中率是 `hits / (hits + misses)`；调原生的 `C().Clear()` 会把计数清零，Prometheus 当成计数器重置。

指标名的前缀、常量标签和几条通用规则见 [`docs/observability.md`「指标」](../docs/observability.md#指标)。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

ristretto v2.4.2。

**`MaxCost` 默认含内部开销**：ristretto 每条另计 56 字节，`MaxCost: 2000`、cost 为 1 时实际只存得下 35 条。
这里关掉（`IgnoreInternalCost`），统计的只是你给的 cost。

**`DefaultTTL` 为负**：ristretto 会把 ttl < 0 的写入直接丢掉，一条都存不进去，所以读配置时就失败。

**停止时不调 `Close`，只调 `Clear`。** `Close` 先关内部 channel、最后才标记已关闭，和它并发的 `Set` / `Del` / `Get`
会直接 panic（实测 100 轮并发读写里 750 个协程 panic）。使用者拿到的是原生 `*ristretto.Cache`，停止钩子跑的时候
谁还攥着它，框架不知道。`Clear` 对并发读写是安全的；代价是两个后台协程和计数器内存（默认 `NumCounters` 约 4.3MB）
留到进程退出。

**`Metric` 的开销**：ristretto 的计数默认关着。打开之后每个实例常驻多约 84KB；另有一张记录写入时间的表
（最多 10 万条），写过 10 万个以上不同的键之后多约 8–12MB。本包的 Get / Set 基准前后差异不显著，
ristretto 自己的 4 协程并发 Get 每次约多 20ns（110ns → 131ns）。

**TTL 到期不是到点就移除**：ristretto 按 5s 一个桶、每 2.5s 扫一轮，到期之后几秒才计进 `cache_keys_evicted_total`。
