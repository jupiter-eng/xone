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

// Sample is one sample, named as it is in the /metrics text: a histogram comes apart into
// xxx_bucket{le=...}, xxx_sum and xxx_count, and a summary into xxx{quantile=...}, xxx_sum and
// xxx_count
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Metrics is the result of one /metrics scrape
type Metrics struct {
	Samples []Sample
}

// ParseMetrics parses the Prometheus text format
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
		return out // the parser puts the +Inf bucket in Bucket like any other, with le rendered as +Inf
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

// formatBound renders a bucket's upper bound and a quantile the way the /metrics text writes them:
// 0.005, 1, +Inf
func formatBound(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Find returns the samples whose name matches exactly and that carry the given labels. labels are
// key and value in pairs, and a sample only has to hold them (a subset match); further labels on
// the sample are ignored
func (m Metrics) Find(name string, labels ...string) []Sample {
	var out []Sample
	for _, s := range m.Samples {
		if s.Name == name && hasLabels(s.Labels, labels) {
			out = append(out, s)
		}
	}
	return out
}

// Sum adds up the values Find returns, and is 0 when there is none
func (m Metrics) Sum(name string, labels ...string) float64 {
	var sum float64
	for _, s := range m.Find(name, labels...) {
		sum += s.Value
	}
	return sum
}

// Has says whether there is a sample under this name
func (m Metrics) Has(name string) bool { return len(m.Find(name)) > 0 }

func hasLabels(have map[string]string, want []string) bool {
	for i := 0; i+1 < len(want); i += 2 {
		if have[want[i]] != want[i+1] {
			return false
		}
	}
	return true
}

// ScrapeMetrics scrapes the Prometheus metrics at url
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

// Metrics scrapes this process's /metrics
func (p *Process) Metrics(t testing.TB) Metrics {
	t.Helper()
	return ScrapeMetrics(t, p.URL("/metrics"))
}
