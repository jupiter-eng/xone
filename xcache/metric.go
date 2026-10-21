package xcache

import (
	"github.com/prometheus/client_golang/prometheus"
)

// cacheMetric is the definition of one cache metric. The table-driven form has the same reason as in
// xgorm / xredis: spread across struct fields, adding a metric would mean changing four places in step.
type cacheMetric struct {
	name  string
	help  string
	typ   prometheus.ValueType
	value func(*Cache) float64
}

// cacheMetrics is the metric table. The counts all come from ristretto's Metrics (v2.4.2) and are cumulative
// over the instance's whole lifetime; the few that do not mean what they appear to have all been measured:
//
//   - keys_evicted is not only "pushed out because the capacity was full": an explicit Del and a TTL expiry
//     cleaned up go through the same count in ristretto (the policy's del) and are included in it.
//   - Calling Clear() resets every count to zero -- Prometheus reads that as a counter reset and rate() still works.
//   - cost is the cost currently occupied. This package always writes with cost 1, so it is the number of
//     entries; the few written into the buffer and not yet processed are not counted.
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

// cacheCollector reads each instance's counts only when scraped, for the same reason as xredis's poolCollector:
// nothing is pushed, no extra goroutine is held, and what is read is always the value at the moment of the scrape.
type cacheCollector struct {
	descs  []*prometheus.Desc // one per entry in cacheMetrics
	caches func() map[string]*Cache
}

// newCacheCollector takes the prefix and the constant labels from the caller and reads no globals here.
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
