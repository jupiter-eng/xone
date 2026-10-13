package xmetric

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// collector 缓存的类型前缀，确保同名不同类型的指标各占一个 key
const (
	kindCounter   = "c"
	kindGauge     = "g"
	kindHistogram = "h"
)

// Tag 指标标签键值对
type Tag struct {
	Name  string
	Value string
}

// T 创建标签
func T(name, value string) Tag { return Tag{Name: name, Value: value} }

// 下面这些快捷方法都先取一次 active()，随后整次操作都用这一个实例：
// 中途换实例时，这次打点要么完整落在旧的上、要么完整落在新的上，
// 不会出现「collector 从旧实例取、值往新实例记」这种两边都不对的情况。

// CounterInc 计数器 +1
func CounterInc(name string, tags ...Tag) {
	names, values := parseTags(tags)
	active().counterOf(name, names).WithLabelValues(values...).Inc()
}

// CounterAdd 计数器 +v（v 必须 >= 0）
func CounterAdd(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	active().counterOf(name, names).WithLabelValues(values...).Add(v)
}

// GaugeSet 设置仪表盘值
func GaugeSet(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	active().gaugeOf(name, names).WithLabelValues(values...).Set(v)
}

// GaugeInc 仪表盘 +1
func GaugeInc(name string, tags ...Tag) {
	names, values := parseTags(tags)
	active().gaugeOf(name, names).WithLabelValues(values...).Inc()
}

// GaugeDec 仪表盘 -1
func GaugeDec(name string, tags ...Tag) {
	names, values := parseTags(tags)
	active().gaugeOf(name, names).WithLabelValues(values...).Dec()
}

// HistogramObserve 直方图观测，单位秒
func HistogramObserve(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	active().histogramOf(name, names).WithLabelValues(values...).Observe(v)
}

// parseTags 取出标签名和值，按名字排序，让不同书写顺序命中同一个缓存
func parseTags(tags []Tag) (names, values []string) {
	if len(tags) == 0 {
		return nil, nil
	}
	// 已经按名字排好的（只有一个标签时总是）直接用；
	// 要排序时先拷贝再排，不动调用方的切片
	byName := func(a, b Tag) int { return cmp.Compare(a.Name, b.Name) }
	if !slices.IsSortedFunc(tags, byName) {
		tags = slices.Clone(tags)
		slices.SortStableFunc(tags, byName)
	}

	names = make([]string, len(tags))
	values = make([]string, len(tags))
	for i, t := range tags {
		names[i], values[i] = t.Name, t.Value
	}
	return names, values
}

// cacheKey 拼出 collector 缓存键，一次分配完成
func cacheKey(kind, name string, labelNames []string) string {
	size := len(kind) + 1 + len(name) + 1
	for i, l := range labelNames {
		if i > 0 {
			size++
		}
		size += len(l)
	}

	var b strings.Builder
	b.Grow(size)
	b.WriteString(kind)
	b.WriteByte(':')
	b.WriteString(name)
	b.WriteByte(':')
	for i, l := range labelNames {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l)
	}
	return b.String()
}

// collectorOf 按 key 复用 collector，未命中时由 build 创建并注册。
//
// 注册回来的类型与预期不符，说明这个指标名已经被注册成别的类型了
// （比如先 Counter 后 Gauge）。此时本实例不在 registry 里，
// 记录的值永远导不出去——必须说出来，否则是一次完全静默的数据丢失。
func collectorOf[T prometheus.Collector](m *Metrics, key, name string, build func() T) T {
	if v, ok := m.collectors.Load(key); ok {
		if typed, ok := v.(T); ok {
			return typed
		}
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()
	if v, ok := m.collectors.Load(key); ok {
		if typed, ok := v.(T); ok {
			return typed
		}
	}

	c := build()
	registered, err := register(m.Registry, c)
	switch typed, ok := registered.(T); {
	case err != nil:
		// 只能记一笔：打点是运行期调用，这里没有「让启动失败」这个选项
		slog.Error("xmetric metric registration failed, values recorded through it will not be exported", "metric", name, "error", err)
	case ok:
		c = typed
	default:
		logNameConflict(name, registered)
	}

	m.collectors.Store(key, c)
	return c
}

func (m *Metrics) counterOf(name string, labelNames []string) *prometheus.CounterVec {
	return collectorOf(m, cacheKey(kindCounter, name, labelNames), name, func() *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   m.cfg.Namespace,
			Name:        name,
			Help:        name,
			ConstLabels: m.cfg.ConstLabels,
		}, labelNames)
	})
}

func (m *Metrics) gaugeOf(name string, labelNames []string) *prometheus.GaugeVec {
	return collectorOf(m, cacheKey(kindGauge, name, labelNames), name, func() *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   m.cfg.Namespace,
			Name:        name,
			Help:        name,
			ConstLabels: m.cfg.ConstLabels,
		}, labelNames)
	})
}

func (m *Metrics) histogramOf(name string, labelNames []string) *prometheus.HistogramVec {
	return collectorOf(m, cacheKey(kindHistogram, name, labelNames), name, func() *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   m.cfg.Namespace,
			Name:        name,
			Help:        name,
			Buckets:     slices.Clone(m.cfg.HistogramBuckets),
			ConstLabels: m.cfg.ConstLabels,
		}, labelNames)
	})
}

// cachedCount 缓存里有多少个 collector。
// Install 用它判断「换实例之前记过点没有」，那些值留在上一个 Registry 里导不出去
func (m *Metrics) cachedCount() int {
	n := 0
	m.collectors.Range(func(any, any) bool { n++; return true })
	return n
}

// logNameConflict 同一个指标名被注册成了不同类型
func logNameConflict(name string, registered prometheus.Collector) {
	slog.Error("xmetric metric name conflict, values recorded through it will not be exported",
		"metric", name, "registered_type", fmt.Sprintf("%T", registered))
}
