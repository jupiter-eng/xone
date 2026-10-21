package xkafka

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/xmetric"
)

// consumeHist is the consume duration histogram currently in effect; a cluster without Metric does not record into it.
var consumeHist atomic.Pointer[prometheus.HistogramVec]

// newConsumeHistogram measures one message from the start of processing to a result (retries and a dead-letter write included).
//
// status is how that message ended up: ok, dead_letter, skipped, or aborted (unfinished when shutting down or when the partition was revoked, redelivered later).
// It carries no partition: multiplying by the partition count would make the series count grow with the topic's partitions. The bucket boundaries are prometheus's defaults (5ms to 10s),
// the same scale as the business histograms the xmetric shortcuts build.
func newConsumeHistogram() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "kafka_consume_duration_seconds",
		Help:        "Time to process one Kafka message, including retries and dead lettering",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"topic", "group", "status"})
}

// registerMetrics attaches the histogram to the current Registry. It attaches every time a consumer starts rather than using a sync.Once, for the same reason as in xredis:
// after xmetric is reinstalled the Registry is a new one, and attaching twice to the same Registry is handled by RegisterAs handing back the existing one.
// It does not fail startup: metrics not being exported is an observability problem.
func registerMetrics() {
	h, err := xmetric.RegisterAs(newConsumeHistogram())
	if err != nil {
		slog.Error("xkafka failed to register the consume duration metric, values recorded through it will not be exported", "error", err)
	}
	consumeHist.Store(h)
}

// observe records one message's duration, and records nothing when this cluster has Metric off.
func (c *consumer) observe(topic string, res outcome, d time.Duration) {
	if !c.metric {
		return
	}
	if h := consumeHist.Load(); h != nil {
		h.WithLabelValues(topic, c.group, string(res)).Observe(d.Seconds())
	}
}
