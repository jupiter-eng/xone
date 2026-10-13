package xkafka

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/xmetric"
)

// consumeHist 当前生效的消费耗时直方图，没开 Metric 的集群不往里记
var consumeHist atomic.Pointer[prometheus.HistogramVec]

// newConsumeHistogram 一条消息从开始处理到有结果（含重试、写死信）的耗时。
//
// status 是这条消息最后怎样了：ok、dead_letter、skipped、aborted（退出或分区被收回时没处理完，之后重新投递）。
// 不带分区：分区数乘上去，序列数就跟着 topic 的分区数涨。桶边界用 prometheus 的默认（5ms 到 10s），
// 与 xmetric 快捷方法建出来的业务直方图同一把刻度
func newConsumeHistogram() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "kafka_consume_duration_seconds",
		Help:        "Time to process one Kafka message, including retries and dead lettering",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"topic", "group", "status"})
}

// registerMetrics 把直方图挂到当前的 Registry 上。每次起消费者都挂、不用 sync.Once，理由同 xredis：
// xmetric 重装之后是新的 Registry；同一个 Registry 上重复挂由 RegisterAs 交回已有的那个。
// 不让启动失败：指标导不出去是可观测性问题
func registerMetrics() {
	h, err := xmetric.RegisterAs(newConsumeHistogram())
	if err != nil {
		slog.Error("xkafka failed to register the consume duration metric, values recorded through it will not be exported", "error", err)
	}
	consumeHist.Store(h)
}

// observe 记一条消息的耗时，这个集群没开 Metric 就不记
func (c *consumer) observe(topic string, res outcome, d time.Duration) {
	if !c.metric {
		return
	}
	if h := consumeHist.Load(); h != nil {
		h.WithLabelValues(topic, c.group, string(res)).Observe(d.Seconds())
	}
}
