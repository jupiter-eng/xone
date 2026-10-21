package xgorm

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// poolMetric is the definition of one connection pool metric.
//
// A table rather than a field per metric: the latter means writing each one four times — the
// constant, the field, the constructor and Collect — so adding a metric means changing four
// places in step.
type poolMetric struct {
	name  string
	help  string
	typ   prometheus.ValueType
	value func(sql.DBStats) float64
}

// poolMetrics is the metric table, with names carrying the base unit as Prometheus conventions ask.
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

// poolCollector reads the connection pool's state when it is scraped.
//
// It implements prometheus.Collector rather than pushing values into a Gauge on a timer: a pool's
// state is instantaneous, and under a push model the moment the scrape interval and the push
// interval drift apart a stale value is read — plus it needs one more background goroutine.
type poolCollector struct {
	descs []*prometheus.Desc // one per entry of poolMetrics, in the same order
	stats func() map[string]sql.DBStats
}

// newPoolCollector takes the prefix and the constant labels from the caller rather than reading
// the globals here: reading globals would leave a test that builds its own metrics setup unable to
// verify the prefix took effect.
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
