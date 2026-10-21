package xmetric

import (
	"context"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xlog"
)

// The shortcuts are what business code calls several times per request, so what is measured here is the steady state
// after a cache hit: the first call builds and registers the collector, happens once, and is not counted.

func BenchmarkCounterInc_NoLabels(b *testing.B) {
	newMetrics(b, nil)
	CounterInc("bench_total")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CounterInc("bench_total")
	}
}

func BenchmarkCounterInc_OneLabel(b *testing.B) {
	newMetrics(b, nil)
	CounterInc("bench_total", T("status", "ok"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CounterInc("bench_total", T("status", "ok"))
	}
}

func BenchmarkCounterInc_TwoLabels(b *testing.B) {
	newMetrics(b, nil)
	CounterInc("bench_total", T("route", "/order"), T("status", "ok"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CounterInc("bench_total", T("route", "/order"), T("status", "ok"))
	}
}

// The tags are not written in name order, so they have to be sorted before the cache can hit.
func BenchmarkCounterInc_TwoLabels_Reversed(b *testing.B) {
	newMetrics(b, nil)
	CounterInc("bench_total", T("status", "ok"), T("route", "/order"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CounterInc("bench_total", T("status", "ok"), T("route", "/order"))
	}
}

// In production many request goroutines record at once, and contention on global state only shows up under concurrency.
func BenchmarkCounterInc_TwoLabels_Parallel(b *testing.B) {
	newMetrics(b, nil)
	CounterInc("bench_total", T("route", "/order"), T("status", "ok"))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			CounterInc("bench_total", T("route", "/order"), T("status", "ok"))
		}
	})
}

func BenchmarkHistogramObserve_TwoLabels(b *testing.B) {
	newMetrics(b, nil)
	HistogramObserve("bench_seconds", 0.1, T("route", "/order"), T("status", "ok"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		HistogramObserve("bench_seconds", 0.1, T("route", "/order"), T("status", "ok"))
	}
}

func BenchmarkObserveDuration_TwoLabels(b *testing.B) {
	newMetrics(b, nil)
	ObserveDuration("bench", time.Millisecond, T("route", "/order"), T("status", "ok"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ObserveDuration("bench", time.Millisecond, T("route", "/order"), T("status", "ok"))
	}
}

// An Error log written through xlog goes through observeLog to be counted; this is the extra stretch on the logging hot path.
func BenchmarkObserveLog_ErrorLogCount(b *testing.B) {
	newMetrics(b, func(c *Config) { c.LogErrorMetric = true })
	c := xlog.DefaultConfig()
	c.Console = false // no output at all, leaving only formatting and the observers
	l, closer, err := xlog.New(c)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { closer.Close() })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.ErrorContext(ctx, "something went wrong")
	}
}
