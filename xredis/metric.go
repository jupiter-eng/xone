package xredis

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// poolMetric 一个连接池指标的定义。表驱动的理由同 xgorm：
// 分散成结构体字段的话，加一个指标要同步改四处。
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

// poolCollector 被抓取时才读连接池状态，理由同 xgorm：状态是瞬时量，
// 推模式下采集间隔和推送间隔一错开就读到过期值，还得多一个后台协程。
type poolCollector struct {
	descs []*prometheus.Desc
	stats func() map[string]*redis.PoolStats
}

// newPoolCollector 前缀和常量标签由调用方传进来，不在这里读全局
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
