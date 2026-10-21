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

// observerOnce injects the observer only once.
//
// xlog's observers are append-only, and injecting twice would count the same error log several times.
// The observer does not hold the counter itself and takes it from whichever setup is in effect, so once is enough.
var observerOnce sync.Once

// newLogCounter builds log_errors_total on m's registry and attaches the log observer.
//
// The "error rate" is the most common alert and should not have to wait for the business to instrument it.
//
// It goes through xlog's observer rather than wrapping slog.Default() from outside: the latter would form a cycle
// with the standard library. slog.SetDefault also routes the log package's output into the new handler, and if that
// chain ends up back at slog's own handler the record flows back through log.Output and deadlocks on the log
// package's non-reentrant lock -- an application not using xlog would hang on its first log line.
//
// The cost is that this feature depends on xlog: logs not going through xlog are not counted. That is written in the config field's comment.
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

// observeLog counts the logs at Error level and above.
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

// callerOf takes the record's call site as file:line.
//
// slog always records the call site's PC, whether or not AddSource is on -- AddSource decides whether to write it
// into the log, not whether to collect it.
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

// trimPath keeps only the last two path elements: a full path carries the build machine's directories,
// which is useless and would give the same code different label values on different machines.
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

// exemplarOf takes the trace identifiers as an exemplar, so a dashboard can go from a metric point back to the trace.
//
// It goes through logext.TraceIDs (the extractor xtrace injects) rather than reading OpenTelemetry directly: this
// module therefore does not depend on OTel and compiles and runs with or without tracing.
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

// addWithExemplar counts with an exemplar, falling back to a plain +1 when exemplars are not supported.
func addWithExemplar(c prometheus.Counter, labels prometheus.Labels) {
	if len(labels) > 0 {
		if ea, ok := c.(prometheus.ExemplarAdder); ok {
			ea.AddWithExemplar(1, labels)
			return
		}
	}
	c.Inc()
}
