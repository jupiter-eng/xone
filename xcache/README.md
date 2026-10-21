# xcache

A local cache: what you get is a native `*ristretto.Cache` (ristretto v2), built by the framework from the configuration.

- The package-level `Get` / `Set` act on the `default` instance with the configured `DefaultTTL` and a fixed cost of 1
- `Get[V]` takes the value as its type, and a type mismatch counts as a miss
- One instance or several; `xcache.C("name")` fetches the native object by name
- `MaxCost` counts only the cost given, so for package-level writes it is simply how many entries fit
- Hit-rate and other `cache_*` metrics, switchable per instance

## Quick start

```yaml
# conf/application.yml
XCache:
  MaxCost: 10000           # the package-level Set has a fixed cost of 1, so this is at most 10 thousand entries
  NumCounters: 100000      # ten times the expected number of entries is a good value
  DefaultTTL: 1m
```

```go
type User struct{ ID, Name string }

// GetUser checks the local cache first and falls back to the source, caching the result for DefaultTTL
func GetUser(ctx context.Context, id string, load func(context.Context, string) (*User, error)) (*User, error) {
	key := "user:" + id
	if u, ok := xcache.Get[*User](key); ok { // a type mismatch counts as a miss
		return u, nil
	}
	u, err := load(ctx, id)
	if err != nil {
		return nil, err
	}
	xcache.Set(key, u)
	return u, nil
}

// For a cost and a TTL of your own, use the native ristretto: SetWithTTL(key, value, cost, ttl)
func PutHot(key string, v any) {
	xcache.C().SetWithTTL(key, v, 1, 10*time.Second)
}
```

## Configuration

```yaml
XCache:
  NumCounters: 1000000     # the number of frequency counters; ten times the expected number of entries is a good value
  MaxCost: 100000          # the upper bound on total cost; the package-level Set has a fixed cost of 1, so this equals the number of entries
  BufferItems: 64          # the size of the internal buffer for Get, the value upstream recommends
  DefaultTTL: 5m           # the expiry used by the package-level Set; 0 = never expires, a negative value fails startup
  Metric: true             # hit-rate and other cache_* metrics, per instance
```

**Several instances** go under `Clients`, each with the same fields as above; anything left out uses the defaults above:

```yaml
XCache:
  Clients:
    default: {MaxCost: 100000}
    session: {MaxCost: 10000, DefaultTTL: 30m}
```

`xcache.C()` fetches `default` and `xcache.C("session")` the other one; the package-level `Get` / `Set` act on `default`
only. The two forms cannot be mixed. Without an `XCache` block, no instance is built at all.

## API

| Function | Description |
|---|---|
| `Get[V](key) (V, bool)` | Reads from `default` and takes the value as `V`; a stored type that does not match `V` counts as a miss (with `XONE_DEBUG` on, one `[xone debug]` line goes to stderr) |
| `Set(key, value) bool` | Writes to `default` with the configured `DefaultTTL` and a cost of 1. The return value is "was it accepted", not "was it stored" |
| `SetWithTTL(key, value, ttl) bool` | The same, with an expiry of your own |
| `Del(key)` | Deletes a key from `default` |
| `C(name ...string) *Cache` | Fetches the native `*ristretto.Cache`; without an argument, `default`. If there is none it panics |
| `Has(name ...string) bool` | Whether an instance is configured, for checking an optional dependency first |
| `Names() []string` | Which instances are configured |
| `DefaultTTL(name ...string) time.Duration` | The default expiry configured for an instance; a misspelled name panics as `C` does rather than returning 0 (0 means never expires) |
| `New(cfg) (*Cache, io.Closer, error)` | The pure constructor: touches no globals, usable outside the framework; the Closer it returns calls `Clear` |

## Notes

- **`MaxCost` counts only the cost given**, without ristretto's 56 bytes of internal overhead per entry. When recording
  cost in bytes, include that in the budget.
- **Shutdown only calls `Clear`, never `Close`**: `Close` running alongside concurrent reads and writes panics. An
  instance built with `xcache.New` whose callers have certainly stopped can be closed with `Close()` directly.
- **A TTL expiry does not remove the entry on the dot**, it takes a few seconds; a negative `DefaultTTL` fails startup,
  and 0 means never expires.
- **A `Get` immediately after `Set` returns true may still not find the key**: writes take effect asynchronously, and the
  admission policy may drop it quietly. A cache is allowed to drop, so ordinary business code can ignore the return value.
- **`Metric` (on by default) costs memory**: about 84KB per instance, and 8-12MB more after more than 100 thousand
  distinct keys have been written.

The numbers and the reasoning are in ["Behavior (measured)"](#behavior-measured).

## Observability

### Logs

| Message | Level | Fields |
|---|---|---|
| `xcache created` | INFO | `name`, `max_cost`, `default_ttl`, `metric` |
| `xcache ready` | INFO | `instances` |

The global logging conventions (`trace_id` injection, `xlog.AddKV`, the framework's start and stop logs) are in
[`docs/observability.md`](../docs/observability.md#logs).

### Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `cache_hits_total` / `cache_misses_total` / `cache_keys_added_total` / `cache_keys_updated_total` / `cache_keys_evicted_total` / `cache_sets_dropped_total` / `cache_sets_rejected_total` | counter | `name` | xcache, `XCache.Metric`, per instance |
| `cache_cost` / `cache_max_cost` | gauge | `name` | xcache |

- **`cache_keys_evicted_total`** is not only entries pushed out because the capacity was full: an explicit `Del` and a
  TTL expiry cleaned up are counted in it as well.
- The hit rate is `hits / (hits + misses)`; calling the native `C().Clear()` resets the counts, which Prometheus reads as
  a counter reset.

The metric name prefix, the constant labels and a few general rules are in
[`docs/observability.md` "Metrics"](../docs/observability.md#metrics).

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

ristretto v2.4.2.

**`MaxCost` includes the internal overhead by default**: ristretto counts 56 bytes extra per entry, so with
`MaxCost: 2000` and a cost of 1 only 35 entries really fit. It is turned off here (`IgnoreInternalCost`), so what is
counted is only the cost given.

**A negative `DefaultTTL`**: ristretto drops a write with ttl < 0 outright, storing nothing at all, so it fails while the
configuration is read.

**Shutdown does not call `Close`, only `Clear`.** `Close` closes the internal channels first and marks itself closed
last, so a `Set` / `Del` / `Get` running concurrently with it panics outright (measured: 750 goroutines panicking over
100 rounds of concurrent reads and writes). What the caller holds is the native `*ristretto.Cache`, and the framework
does not know who still holds it when the stop hook runs. `Clear` is safe against concurrent reads and writes; the cost
is that two background goroutines and the counter memory (about 4.3MB with the default `NumCounters`) live until the
process exits.

**What `Metric` costs**: ristretto's counting is off by default. Turning it on adds about 84KB resident per instance,
plus a table recording write times (at most 100 thousand entries), which adds about 8-12MB after more than 100 thousand
distinct keys have been written. This package's Get / Set benchmarks show no significant difference, and ristretto's own
4-goroutine concurrent Get costs about 20ns more per call (110ns → 131ns).

**A TTL expiry does not remove the entry on the dot**: ristretto uses 5s buckets and sweeps every 2.5s, so an expiry is
counted into `cache_keys_evicted_total` a few seconds later.
