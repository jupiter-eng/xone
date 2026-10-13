package xcache

import (
	"github.com/prometheus/client_golang/prometheus"
)

// cacheMetric 一个缓存指标的定义。表驱动的理由同 xgorm / xredis：
// 分散成结构体字段的话，加一个指标要同步改四处。
type cacheMetric struct {
	name  string
	help  string
	typ   prometheus.ValueType
	value func(*Cache) float64
}

// cacheMetrics 指标表。计数都来自 ristretto 的 Metrics（v2.4.2），它们是整个实例
// 生命期的累计值；几处和字面意思不一样的，都是量过的：
//
//   - keys_evicted 不只是「容量满了被挤掉」：显式 Del 和 TTL 到期被清理的，
//     ristretto 走的是同一个计数（policy 的 del），一并算在里面。
//   - 调 Clear() 会把全部计数清零——Prometheus 把它当成计数器重置，rate() 照常能算。
//   - cost 是当前占用的成本。本包写入时 cost 固定为 1，所以它就是条目数；
//     写进缓冲还没被处理的那几条不算。
var cacheMetrics = []cacheMetric{
	{"cache_hits_total", "Total Get calls that found the key", prometheus.CounterValue,
		func(c *Cache) float64 { return float64(c.Metrics.Hits()) }},
	{"cache_misses_total", "Total Get calls that did not find the key", prometheus.CounterValue,
		func(c *Cache) float64 { return float64(c.Metrics.Misses()) }},
	{"cache_keys_added_total", "Total new keys admitted into the cache", prometheus.CounterValue,
		func(c *Cache) float64 { return float64(c.Metrics.KeysAdded()) }},
	{"cache_keys_updated_total", "Total writes that replaced the value of an existing key", prometheus.CounterValue,
		func(c *Cache) float64 { return float64(c.Metrics.KeysUpdated()) }},
	{"cache_keys_evicted_total", "Total keys removed: evicted for space, expired, or deleted", prometheus.CounterValue,
		func(c *Cache) float64 { return float64(c.Metrics.KeysEvicted()) }},
	{"cache_sets_dropped_total", "Total writes dropped because the write buffer was full", prometheus.CounterValue,
		func(c *Cache) float64 { return float64(c.Metrics.SetsDropped()) }},
	{"cache_sets_rejected_total", "Total writes rejected by the admission policy", prometheus.CounterValue,
		func(c *Cache) float64 { return float64(c.Metrics.SetsRejected()) }},
	{"cache_cost", "Cost in use right now (the entry count when every write has cost 1)", prometheus.GaugeValue,
		func(c *Cache) float64 { return float64(c.MaxCost() - c.RemainingCost()) }},
	{"cache_max_cost", "Configured cost limit", prometheus.GaugeValue,
		func(c *Cache) float64 { return float64(c.MaxCost()) }},
}

// cacheCollector 被抓取时才读各实例的计数，理由同 xredis 的 poolCollector：
// 不推、不额外占协程，读到的永远是抓取那一刻的值。
type cacheCollector struct {
	descs  []*prometheus.Desc // 与 cacheMetrics 一一对应
	caches func() map[string]*Cache
}

// newCacheCollector 前缀和常量标签由调用方传进来，不在这里读全局
func newCacheCollector(ns string, labels prometheus.Labels, caches func() map[string]*Cache) *cacheCollector {
	descs := make([]*prometheus.Desc, len(cacheMetrics))
	for i, m := range cacheMetrics {
		descs[i] = prometheus.NewDesc(prometheus.BuildFQName(ns, "", m.name), m.help, []string{"name"}, labels)
	}
	return &cacheCollector{descs: descs, caches: caches}
}

func (c *cacheCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs {
		ch <- d
	}
}

func (c *cacheCollector) Collect(ch chan<- prometheus.Metric) {
	for name, cache := range c.caches() {
		for i, m := range cacheMetrics {
			ch <- prometheus.MustNewConstMetric(c.descs[i], m.typ, m.value(cache), name)
		}
	}
}
