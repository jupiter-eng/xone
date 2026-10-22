package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xmetric"
)

// Metric records request counts and durations, split by method, route and status code.
//
// The collectors are created when the first request arrives rather than at assembly time:
// assembly can happen before xmetric is initialized, and the registry grabbed then is the fallback
// one, so the metrics would be recorded perfectly and never appear in /metrics -- with nothing to
// show for it. The first request necessarily comes after the server is up, by which time
// everything is ready.
//
// once is per middleware instance rather than package-level: a package-level one would pin the
// collectors to whichever registry existed when the first middleware was built, and after a
// registry change the recorded values would again never be exported.
//
// The route label is beego's route template, so the series count follows the number of routes
// rather than the number of distinct paths: without it, every /users/42 would be a series of its
// own and the metric would grow without bound.
func Metric() func(http.Handler) http.Handler {
	var (
		once    sync.Once
		total   *prometheus.CounterVec
		latency *prometheus.HistogramVec
	)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { total, latency = newCollectors() })

			w, r, st := enter(w, r)
			start, ctx := time.Now(), r.Context()

			// Record in a defer: even when a panic passes through this layer (a custom RecoverFunc
			// blowing up, say), the request is still counted and does not vanish from the error
			// rate.
			defer func() {
				route, method, code := routeOf(st), web.NormalizeMethod(r.Method), strconv.Itoa(status(st, ctx))

				total.WithLabelValues(method, route, code).Inc()
				// Seconds rather than milliseconds: rounding to milliseconds records a 0.4ms
				// request as 0.
				latency.WithLabelValues(method, route, code).Observe(time.Since(start).Seconds())
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// newCollectors creates and registers the two metrics. A duplicate registration is handled by
// xmetric.Register, which returns the existing instance -- which is also what keeps the names the
// same as xgin's and xecho's when two of them run in one process.
func newCollectors() (*prometheus.CounterVec, *prometheus.HistogramVec) {
	total := register(prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "http_requests_total",
		Help:        "Total number of HTTP requests",
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"method", "route", "status"}), "request count")

	latency := register(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "http_request_duration_seconds",
		Help:        "HTTP request duration",
		Buckets:     xmetric.HTTPDurationBuckets(),
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"method", "route", "status"}), "request duration")

	return total, latency
}

// register registers one metric and only logs on failure: metrics that cannot be exported are an
// observability problem, and should not keep an HTTP service from starting.
func register[T prometheus.Collector](c T, what string) T {
	registered, err := xmetric.RegisterAs(c)
	if err != nil {
		slog.Error("xbeego failed to register the "+what+" metric, values recorded through it will not be exported", "error", err)
	}
	return registered
}
