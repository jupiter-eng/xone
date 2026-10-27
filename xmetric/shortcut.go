package xmetric

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// collector cache type prefixes, so that metrics with the same name but different types occupy different keys.
const (
	kindCounter   = "c"
	kindGauge     = "g"
	kindHistogram = "h"
)

// Tag is a metric label key-value pair.
type Tag struct {
	Name  string
	Value string
}

// T creates a label.
func T(name, value string) Tag { return Tag{Name: name, Value: value} }

// The shortcuts below each take active() once and use that one instance for the whole operation: when the instance is
// swapped midway, this recording lands entirely on the old one or entirely on the new one, and never ends up with the
// collector taken from the old instance while the value goes to the new one, which would be wrong on both sides.

// CounterInc increments a counter by 1.
func CounterInc(name string, tags ...Tag) {
	names, values := parseTags(tags)
	active().counterOf(name, names).WithLabelValues(values...).Inc()
}

// CounterAdd adds v to a counter (v must be >= 0).
//
// A negative v is recorded as an error and nothing else: client_golang panics on it (counter cannot decrease in
// value), and that panic would come out of whichever business call happened to make the mistake. A metric that
// cannot be recorded is an observability problem and not a reason to take the request down with it, which is how
// every other failure in this package is treated.
func CounterAdd(name string, v float64, tags ...Tag) {
	if v < 0 {
		slog.Error("xmetric CounterAdd ignored a negative value, a counter only goes up", "metric", name, "value", v)
		return
	}
	names, values := parseTags(tags)
	active().counterOf(name, names).WithLabelValues(values...).Add(v)
}

// GaugeSet sets a gauge value.
func GaugeSet(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	active().gaugeOf(name, names).WithLabelValues(values...).Set(v)
}

// GaugeInc increments a gauge by 1.
func GaugeInc(name string, tags ...Tag) {
	names, values := parseTags(tags)
	active().gaugeOf(name, names).WithLabelValues(values...).Inc()
}

// GaugeDec decrements a gauge by 1.
func GaugeDec(name string, tags ...Tag) {
	names, values := parseTags(tags)
	active().gaugeOf(name, names).WithLabelValues(values...).Dec()
}

// HistogramObserve records a histogram observation, in seconds.
func HistogramObserve(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	active().histogramOf(name, names).WithLabelValues(values...).Observe(v)
}

// parseTags takes the label names and values and sorts them by name, so different writing orders hit the same cache entry.
func parseTags(tags []Tag) (names, values []string) {
	if len(tags) == 0 {
		return nil, nil
	}
	// Already sorted by name (always the case with a single label) is used as it is;
	// when sorting is needed it is copied first, leaving the caller's slice untouched.
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

// cacheKey builds the collector cache key in a single allocation.
//
// Every piece carries its own length, which makes the key one canonical spelling of
// (kind, name, label names): two different triples cannot produce the same one. Joining the names
// with a separator instead leaves that open, because nothing here checks what a name holds -- a
// label name with a comma in it (a tag spelled "method,path" rather than two tags) would land on
// the entry of the two-label metric, and the recording would reach a collector built for a
// different number of labels. That is a WithLabelValues with the wrong count, which panics inside
// whichever business call made it, while every other malformed input in this package is one ERROR
// line and a recording that goes nowhere (see CounterAdd and collectorOf).
func cacheKey(kind, name string, labelNames []string) string {
	size := len(kind) + 1 + decLen(len(name)) + 1 + len(name)
	for _, l := range labelNames {
		size += decLen(len(l)) + 1 + len(l)
	}

	var b strings.Builder
	b.Grow(size)
	b.WriteString(kind)
	b.WriteByte(':')
	writeSized(&b, name)
	for _, l := range labelNames {
		writeSized(&b, l)
	}
	return b.String()
}

// writeSized writes s preceded by its length, so that whatever follows cannot be read as part of it.
func writeSized(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
}

// decLen is how many characters the decimal spelling of n takes, for sizing the key exactly.
func decLen(n int) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// collectorOf reuses a collector by key, and on a miss build creates and registers one.
//
// A type coming back that does not match the expectation means this metric name is already registered as another type
// (a Counter first and a Gauge later, say). This instance is then not in the registry and the values it records can
// never be exported -- which has to be said out loud, or it is a completely silent loss of data.
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
		// All that can be done is record it: recording is a runtime call, and "fail startup" is not an option here.
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

// cachedCount is how many collectors are in the cache.
// Install uses it to decide "was anything recorded before the instance was swapped": those values stay in the previous Registry and can never be exported.
func (m *Metrics) cachedCount() int {
	n := 0
	m.collectors.Range(func(any, any) bool { n++; return true })
	return n
}

// logNameConflict means the same metric name was registered as different types.
func logNameConflict(name string, registered prometheus.Collector) {
	slog.Error("xmetric metric name conflict, values recorded through it will not be exported",
		"metric", name, "registered_type", fmt.Sprintf("%T", registered))
}
