package xgorm

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// poolMetric 一个连接池指标的定义
//
// 用表驱动而不是给每个指标开一个字段：后者要在常量、字段、构造函数、
// Collect 四处各写一遍，加一个指标就得同步改四个地方。
type poolMetric struct {
	name  string
	help  string
	typ   prometheus.ValueType
	value func(sql.DBStats) float64
}

// poolMetrics 指标表，名字按 Prometheus 约定带基准单位
var poolMetrics = []poolMetric{
	{"db_pool_open", "Open connections right now (in use + idle)", prometheus.GaugeValue,
		func(s sql.DBStats) float64 { return float64(s.OpenConnections) }},
	{"db_pool_in_use", "Connections currently in use", prometheus.GaugeValue,
		func(s sql.DBStats) float64 { return float64(s.InUse) }},
	{"db_pool_idle", "Idle connections right now", prometheus.GaugeValue,
		func(s sql.DBStats) float64 { return float64(s.Idle) }},
	{"db_pool_max_open", "Connection limit, 0 means unlimited", prometheus.GaugeValue,
		func(s sql.DBStats) float64 { return float64(s.MaxOpenConnections) }},
	{"db_pool_wait_total", "Total number of waits for a connection", prometheus.CounterValue,
		func(s sql.DBStats) float64 { return float64(s.WaitCount) }},
	{"db_pool_wait_duration_seconds_total", "Total time spent waiting for a connection", prometheus.CounterValue,
		func(s sql.DBStats) float64 { return s.WaitDuration.Seconds() }},
	{"db_pool_closed_max_idle_total", "Total connections closed for exceeding the idle limit", prometheus.CounterValue,
		func(s sql.DBStats) float64 { return float64(s.MaxIdleTimeClosed) }},
	{"db_pool_closed_max_lifetime_total", "Total connections closed for exceeding their lifetime", prometheus.CounterValue,
		func(s sql.DBStats) float64 { return float64(s.MaxLifetimeClosed) }},
}

// poolCollector 被抓取时才读连接池状态
//
// 实现 prometheus.Collector 而不是定时把值推进 Gauge：连接池状态是瞬时量，
// 推模式下采集间隔和推送间隔一错开就会读到过期值，还得多一个后台协程。
type poolCollector struct {
	descs []*prometheus.Desc // 与 poolMetrics 一一对应
	stats func() map[string]sql.DBStats
}

// newPoolCollector 前缀和常量标签由调用方传进来，不在这里读全局：
// 读全局的话，测试里造一套独立的指标设施就没法验证前缀有没有生效。
func newPoolCollector(ns string, labels prometheus.Labels, stats func() map[string]sql.DBStats) *poolCollector {
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
