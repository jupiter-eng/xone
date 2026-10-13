package middleware

import (
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/internal/web"
	"github.com/jupiter-eng/xone/xmetric"
)

// Metric 记录请求数和耗时，按方法、路由、状态码分。
//
// collector 在第一个请求到来时才建，不在装配时建：装配可能发生在 xmetric
// 初始化之前（使用者调一下 Engine() 就会），那时抓到的是兜底 registry，
// 于是指标记得好好的、却永远不会出现在 /metrics 里——没有任何迹象。
// 第一个请求一定在服务起来之后，那时什么都就绪了。
//
// once 是每个中间件实例一个而不是包级的：包级的会把 collector 绑死在
// 第一次建中间件时的那个 registry 上，换 registry 之后记的值同样导不出去。
func Metric() gin.HandlerFunc {
	var (
		once    sync.Once
		total   *prometheus.CounterVec
		latency *prometheus.HistogramVec
	)

	return func(c *gin.Context) {
		once.Do(func() { total, latency = newCollectors() })

		start, ctx := time.Now(), c.Request.Context()

		// 用 defer 记：即使 panic 穿过本层（比如用户自定义的 RecoveryFunc 自己炸了），
		// 这个请求也仍然会被计入，不会在错误率里凭空消失
		defer func() {
			route, method, code := routeOf(c), web.NormalizeMethod(c.Request.Method), strconv.Itoa(status(c, ctx))

			total.WithLabelValues(method, route, code).Inc()
			// 用秒而不是毫秒：毫秒取整会把 0.4ms 的请求记成 0
			latency.WithLabelValues(method, route, code).Observe(time.Since(start).Seconds())
		}()

		c.Next()
	}
}

// newCollectors 建并注册两个指标。重复注册由 xmetric.Register 处理——
// 它返回已有的那个实例。
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

// register 注册一个指标，出错只记日志：指标导不出去是可观测性问题，
// 不该让一个 HTTP 服务起不来
func register[T prometheus.Collector](c T, what string) T {
	registered, err := xmetric.RegisterAs(c)
	if err != nil {
		slog.Error("xgin failed to register the "+what+" metric, values recorded through it will not be exported", "error", err)
	}
	return registered
}
