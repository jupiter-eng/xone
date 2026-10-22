package middleware

import (
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xmetric"
)

// Metric records request counts and durations, split by method, route and status code.
//
// The metric names, labels and buckets match xgin's word for word
// (http_requests_total, http_request_duration_seconds), so dashboards and alerts work for both;
// when one process uses both, their requests land on the same set of metrics.
//
// The collectors are created when the first request arrives rather than at assembly time:
// assembly can happen before xmetric is initialized (a single call to Engine() does it), and the
// registry grabbed then is the fallback one, so the metrics would be recorded perfectly and never
// appear in /metrics — with nothing to show for it. The first request necessarily comes after the
// server is up, by which time everything is ready.
//
// once is per middleware instance rather than package-level: a package-level one would pin the
// collectors to whichever registry existed when the first middleware was built, and after a
// registry change the recorded values would again never be exported.
//
// One value serves one *echo.Echo: the route table behind the route label is taken from the first
// request's c.Echo() and kept (see routes). Call Metric again for a second echo.
func Metric() echo.MiddlewareFunc {
	var (
		once    sync.Once
		total   *prometheus.CounterVec
		latency *prometheus.HistogramVec
		rs      routes
	)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			once.Do(func() { total, latency = newCollectors() })

			start, ctx := time.Now(), contextOf(c)

			// Record in a defer: even when a panic passes through this layer (a custom recover
			// function blowing up, say), the request is still counted and does not vanish from
			// the error rate.
			defer func() {
				route, method, code := rs.of(c), web.NormalizeMethod(c.Request().Method), strconv.Itoa(status(c))

				total.WithLabelValues(method, route, code).Inc()
				// Seconds rather than milliseconds: rounding to milliseconds records a 0.4ms
				// request as 0.
				latency.WithLabelValues(method, route, code).Observe(time.Since(start).Seconds())
			}()

			finish(c, ctx, next(c))
			return nil
		}
	}
}

// newCollectors creates and registers the two metrics. A duplicate registration is handled by
// xmetric.Register, which returns the existing instance: when xgin already registered one with
// the same name and labels, that is the one returned here. The name, Help, labels and buckets
// must therefore match xgin's exactly — one character off is a registration conflict.
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
		slog.Error("xecho failed to register the "+what+" metric, values recorded through it will not be exported", "error", err)
	}
	return registered
}
