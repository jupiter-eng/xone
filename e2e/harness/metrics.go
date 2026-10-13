package harness

import (
	"io"
	"math"
	"net/http"
	"strconv"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Sample 一条样本，名字和 /metrics 文本里的一致：直方图拆成
// xxx_bucket{le=...}、xxx_sum、xxx_count，Summary 拆成 xxx{quantile=...}、xxx_sum、xxx_count
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Metrics 抓一次 /metrics 的结果
type Metrics struct {
	Samples []Sample
}

// ParseMetrics 解析 Prometheus 文本格式
func ParseMetrics(r io.Reader) (Metrics, error) {
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(r)
	if err != nil {
		return Metrics{}, err
	}
	var m Metrics
	for name, f := range fams {
		for _, mt := range f.GetMetric() {
			labels := map[string]string{}
			for _, lp := range mt.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			m.Samples = append(m.Samples, flatten(name, f.GetType(), mt, labels)...)
		}
	}
	return m, nil
}

func flatten(name string, typ dto.MetricType, mt *dto.Metric, labels map[string]string) []Sample {
	with := func(k, v string) map[string]string {
		l := make(map[string]string, len(labels)+1)
		for a, b := range labels {
			l[a] = b
		}
		l[k] = v
		return l
	}
	switch typ {
	case dto.MetricType_COUNTER:
		return []Sample{{name, labels, mt.GetCounter().GetValue()}}
	case dto.MetricType_GAUGE:
		return []Sample{{name, labels, mt.GetGauge().GetValue()}}
	case dto.MetricType_HISTOGRAM:
		h := mt.GetHistogram()
		out := []Sample{
			{name + "_sum", labels, h.GetSampleSum()},
			{name + "_count", labels, float64(h.GetSampleCount())},
		}
		for _, b := range h.GetBucket() {
			out = append(out, Sample{name + "_bucket", with("le", formatBound(b.GetUpperBound())), float64(b.GetCumulativeCount())})
		}
		return out // +Inf 那一档解析器照常放在 Bucket 里，le 渲染成 +Inf
	case dto.MetricType_SUMMARY:
		s := mt.GetSummary()
		out := []Sample{
			{name + "_sum", labels, s.GetSampleSum()},
			{name + "_count", labels, float64(s.GetSampleCount())},
		}
		for _, q := range s.GetQuantile() {
			out = append(out, Sample{name, with("quantile", formatBound(q.GetQuantile())), q.GetValue()})
		}
		return out
	default:
		return []Sample{{name, labels, mt.GetUntyped().GetValue()}}
	}
}

// formatBound 按 /metrics 文本里的写法渲染桶的上界和分位数：0.005、1、+Inf
func formatBound(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Find 名字精确匹配、并且带着给定标签的样本。labels 是成对的 key、value，
// 只要求样本有这些标签（子集匹配），样本多出来的标签不管
func (m Metrics) Find(name string, labels ...string) []Sample {
	var out []Sample
	for _, s := range m.Samples {
		if s.Name == name && hasLabels(s.Labels, labels) {
			out = append(out, s)
		}
	}
	return out
}

// Sum Find 出来的样本值之和，一条都没有时是 0
func (m Metrics) Sum(name string, labels ...string) float64 {
	var sum float64
	for _, s := range m.Find(name, labels...) {
		sum += s.Value
	}
	return sum
}

// Has 有没有这个名字的样本
func (m Metrics) Has(name string) bool { return len(m.Find(name)) > 0 }

func hasLabels(have map[string]string, want []string) bool {
	for i := 0; i+1 < len(want); i += 2 {
		if have[want[i]] != want[i+1] {
			return false
		}
	}
	return true
}

// ScrapeMetrics 抓 url 上的 Prometheus 指标
func ScrapeMetrics(t testing.TB, url string) Metrics {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("scrape %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape %s: status %d", url, resp.StatusCode)
	}
	m, err := ParseMetrics(resp.Body)
	if err != nil {
		t.Fatalf("parse metrics from %s: %v", url, err)
	}
	return m
}

// Metrics 抓这个进程的 /metrics
func (p *Process) Metrics(t testing.TB) Metrics {
	t.Helper()
	return ScrapeMetrics(t, p.URL("/metrics"))
}
