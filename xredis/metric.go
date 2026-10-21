package xredis

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// poolMetric is the definition of one connection-pool metric. The table-driven form has the same reason as in xgorm:
// spread across struct fields, adding a metric would mean changing four places in step.
type poolMetric struct {
	name  string
	help  string
	typ   prometheus.ValueType
	value func(*redis.PoolStats) float64
}

var poolMetrics = []poolMetric{
	{"redis_pool_connections", "Connections in the pool right now (in use + idle)", prometheus.GaugeValue,
		func(s *redis.PoolStats) float64 { return float64(s.TotalConns) }},
	{"redis_pool_connections_idle", "Idle connections right now", prometheus.GaugeValue,
		func(s *redis.PoolStats) float64 { return float64(s.IdleConns) }},
	{"redis_pool_connections_stale_total", "Total connections removed for being stale", prometheus.CounterValue,
		func(s *redis.PoolStats) float64 { return float64(s.StaleConns) }},
	{"redis_pool_hits_total", "Total times an idle connection was reused", prometheus.CounterValue,
		func(s *redis.PoolStats) float64 { return float64(s.Hits) }},
	{"redis_pool_misses_total", "Total times no idle connection was available", prometheus.CounterValue,
		func(s *redis.PoolStats) float64 { return float64(s.Misses) }},
	{"redis_pool_timeouts_total", "Total times waiting for a connection timed out", prometheus.CounterValue,
		func(s *redis.PoolStats) float64 { return float64(s.Timeouts) }},
}

// poolCollector reads the pool state only when scraped, for the same reason as in xgorm: the state is an instantaneous
// value, and in a push model a scrape interval out of step with the push interval reads a stale one, at the cost of an extra background goroutine.
type poolCollector struct {
	descs []*prometheus.Desc
	stats func() map[string]*redis.PoolStats
}

// newPoolCollector takes the prefix and the constant labels from the caller and reads no globals here.
func newPoolCollector(ns string, labels prometheus.Labels, stats func() map[string]*redis.PoolStats) *poolCollector {
	descs := make([]*prometheus.Desc, len(poolMetrics))
	for i, m := range poolMetrics {
		descs[i] = prometheus.NewDesc(prometheus.BuildFQName(ns, "", m.name), m.help, []string{"name"}, labels)
	}
	return &poolCollector{descs: descs, stats: stats}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	for name, s := range c.stats() {
		for i, m := range poolMetrics {
			ch <- prometheus.MustNewConstMetric(c.descs[i], m.typ, m.value(s), name)
		}
	}
}
