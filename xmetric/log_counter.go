package xmetric

import (
	"context"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/internal/logext"
)

// observerOnce 观察者只注入一次。
//
// xlog 的观察者是只增不减的，重复注入会让同一条错误日志被数好几次。
// 观察者本身不持有 counter，每次从当前生效的设施上取，所以注入一次就够。
var observerOnce sync.Once

// newLogCounter 在 m 的 registry 上建出 log_errors_total，并接上日志观察者。
//
// 「错误率」是最常用的告警，不该等业务先埋点。
//
// 走 xlog 的观察者而不是在 slog.Default() 外面包一层：后者会和标准库绕成环。
// slog.SetDefault 顺带把 log 包的输出接到新 handler 上，若这条链最终落回
// slog 自带的 handler，记录就会经 log.Output 再流回来，卡死在 log 包那把
// 不可重入的锁上——不用 xlog 的应用第一条日志就会挂住。
//
// 代价是这个特性依赖 xlog：日志不走 xlog 就统计不到。这写在配置项的注释里。
func newLogCounter(m *Metrics) *prometheus.CounterVec {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   m.cfg.Namespace,
		Subsystem:   "log",
		Name:        "errors_total",
		Help:        "Number of log records at Error level or above",
		ConstLabels: m.cfg.ConstLabels,
	}, []string{"level", "caller"})

	registered, err := register(m.Registry, counter)
	if err != nil {
		slog.Error("xmetric failed to register the log error counter", "error", err)
		return nil
	}
	cv, ok := registered.(*prometheus.CounterVec)
	if !ok {
		logNameConflict("log_errors_total", registered)
		return nil
	}

	observerOnce.Do(func() { logext.AddObserver(observeLog) })
	return cv
}

// observeLog 数 Error 及以上级别的日志
func observeLog(ctx context.Context, r slog.Record) {
	if r.Level < slog.LevelError {
		return
	}
	counter := active().logCounter
	if counter == nil {
		return
	}
	addWithExemplar(counter.WithLabelValues(r.Level.String(), callerOf(r)), exemplarOf(ctx))
}

// callerOf 把记录的调用位置取成 file:line
//
// slog 总会记下调用点的 PC，与 AddSource 是否开启无关——
// AddSource 管的是要不要把它写进日志，不管要不要采集。
func callerOf(r slog.Record) string {
	if r.PC == 0 {
		return "unknown"
	}
	f, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
	if f.File == "" {
		return "unknown"
	}
	return trimPath(f.File) + ":" + strconv.Itoa(f.Line)
}

// trimPath 只留最后两段路径：完整路径带着构建机的目录，
// 既没用又会让同一份代码在不同机器上产生不同的标签值
func trimPath(p string) string {
	last := strings.LastIndexByte(p, '/')
	if last < 0 {
		return p
	}
	if prev := strings.LastIndexByte(p[:last], '/'); prev >= 0 {
		return p[prev+1:]
	}
	return p
}

// exemplarOf 取链路标识做 exemplar，让面板能从指标点回到链路。
//
// 走 logext.TraceIDs（xtrace 注入的提取器）而不是直接读 OpenTelemetry：本模块因此不依赖 OTel，
// 用不用链路都能编译、都能跑。
func exemplarOf(ctx context.Context) prometheus.Labels {
	traceID, spanID := logext.TraceIDs(ctx)
	if traceID == "" && spanID == "" {
		return nil
	}
	out := make(prometheus.Labels, 2)
	if traceID != "" {
		out["trace_id"] = traceID
	}
	if spanID != "" {
		out["span_id"] = spanID
	}
	return out
}

// addWithExemplar 带 exemplar 计数，不支持时退化成普通 +1
func addWithExemplar(c prometheus.Counter, labels prometheus.Labels) {
	if len(labels) > 0 {
		if ea, ok := c.(prometheus.ExemplarAdder); ok {
			ea.AddWithExemplar(1, labels)
			return
		}
	}
	c.Inc()
}
