package xmetric

import (
	"context"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xlog"
)

// 快捷方法是业务代码每个请求都会调好几次的地方，这里量的是缓存命中之后的稳态：
// 第一次调用建 collector、注册，只发生一次，不算进去。

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

// 标签没按名字写，要先排序才能命中缓存
func BenchmarkCounterInc_TwoLabels_Reversed(b *testing.B) {
	newMetrics(b, nil)
	CounterInc("bench_total", T("status", "ok"), T("route", "/order"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CounterInc("bench_total", T("status", "ok"), T("route", "/order"))
	}
}

// 线上是很多请求协程同时打点，全局状态上的争用只在并发下才看得出来
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

// 经 xlog 写出的 Error 日志会走 observeLog 计数，这是日志热路径上额外的那一段
func BenchmarkObserveLog_ErrorLogCount(b *testing.B) {
	newMetrics(b, func(c *Config) { c.LogErrorMetric = true })
	c := xlog.DefaultConfig()
	c.Console = false // 一个输出都不开，只剩格式化和观察者
	l, closer, err := xlog.New(c)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { closer.Close() })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.ErrorContext(ctx, "出事了")
	}
}
