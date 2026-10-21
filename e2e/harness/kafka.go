package harness

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// The Kafka cases: the service has an XKafka only with Options.Kafka (the
// service/application-kafka.yml profile), and every case creates a topic of its own (KafkaTopic)
// and deletes it afterwards, so they can t.Parallel.
//
//	XONE_E2E_KAFKA_ADDR  127.0.0.1:9092 by default
//	XONE_E2E_KAFKA       set to 0 when scripts/e2e.sh cannot bring Kafka up, and the Kafka cases skip (RequireKafka)

// KafkaAddr is the Kafka broker's host:port, not through the proxy
func KafkaAddr() string { return env("XONE_E2E_KAFKA_ADDR", "127.0.0.1:9092") }

// RequireKafka is the first line of a Kafka case: it skips when e2e is off or Kafka is
// unavailable.
//
// Kafka runs in Docker, which not every machine has: scripts/e2e.sh sets XONE_E2E_KAFKA=0 when it
// cannot bring it up, and this skips and says why; run without the script, it asks the broker for
// metadata once and skips the same way when none comes back. Dialing the port is not enough:
// docker's port forwarding listens before the broker is up
func RequireKafka(t testing.TB) {
	t.Helper()
	Require(t)
	if os.Getenv("XONE_E2E_KAFKA") == "0" {
		t.Skip("Kafka is unavailable (scripts/e2e.sh could not start the xone-kafka container): Kafka tests skipped")
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(KafkaAddr()))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = cl.Ping(ctx)
		cancel()
		cl.Close()
	}
	if err != nil {
		t.Skipf("Kafka is unavailable at %s (%v): Kafka tests skipped, start it with scripts/e2e.sh", KafkaAddr(), err)
	}
}

// KafkaClient is a native client connected to Kafka directly, for a test to write messages and
// check with, free of every xkafka hook. A write goes to the partition in Record.Partition. It is
// closed when the test ends
func KafkaClient(t testing.TB, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	RequireKafka(t)
	cl, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(KafkaAddr()), kgo.RecordPartitioner(kgo.ManualPartitioner())}, opts...)...)
	if err != nil {
		t.Fatalf("kafka client: %v", err)
	}
	t.Cleanup(cl.Close)
	return cl
}

// KafkaTopic creates a new topic with partitions partitions, named e2e_<random>, and deletes it
// when the test ends
func KafkaTopic(t testing.TB, partitions int32) string {
	t.Helper()
	name := "e2e_" + NewID()
	CreateKafkaTopic(t, name, partitions)
	return name
}

// CreateKafkaTopic creates a topic under the given name and deletes it when the test ends. One
// that exists already is a failure
func CreateKafkaTopic(t testing.TB, name string, partitions int32) {
	t.Helper()
	cl := KafkaClient(t)
	req := kmsg.NewPtrCreateTopicsRequest()
	rt := kmsg.NewCreateTopicsRequestTopic()
	rt.Topic, rt.NumPartitions, rt.ReplicationFactor = name, partitions, 1
	req.Topics = append(req.Topics, rt)
	resp, err := req.RequestWith(context.Background(), cl)
	if err == nil {
		err = kerr.ErrorForCode(resp.Topics[0].ErrorCode)
	}
	if err != nil {
		t.Fatalf("create topic %s: %v", name, err)
	}
	t.Cleanup(func() {
		cl, err := kgo.NewClient(kgo.SeedBrokers(KafkaAddr()))
		if err != nil {
			return
		}
		defer cl.Close()
		del := kmsg.NewPtrDeleteTopicsRequest()
		dt := kmsg.NewDeleteTopicsRequestTopic()
		dt.Topic = kmsg.StringPtr(name)
		del.Topics, del.TopicNames = append(del.Topics, dt), []string{name}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := del.RequestWith(ctx, cl); err != nil {
			t.Logf("cleanup: delete topic %s: %v", name, err)
		}
	})
}

// KafkaProduce writes one record into topic's partition; key may be nil
func KafkaProduce(t testing.TB, cl *kgo.Client, topic string, partition int32, key, value string) {
	t.Helper()
	r := &kgo.Record{Topic: topic, Partition: partition, Value: []byte(value)}
	if key != "" {
		r.Key = []byte(key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cl.ProduceSync(ctx, r).FirstErr(); err != nil {
		t.Fatalf("produce to %s/%d: %v", topic, partition, err)
	}
}

// KafkaRead reads topic from the start until n records have arrived or timeout is up, and returns
// what it read (which may be fewer than n)
func KafkaRead(t testing.TB, topic string, n int, timeout time.Duration) []*kgo.Record {
	t.Helper()
	cl := KafkaClient(t, kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n && ctx.Err() == nil {
		out = append(out, cl.PollFetches(ctx).Records()...)
	}
	return out
}

// KafkaCommitted is the offset the consumer group committed on topic/partition, or -1 when it has
// committed none
func KafkaCommitted(t testing.TB, group, topic string, partition int32) int64 {
	t.Helper()
	cl := KafkaClient(t)
	req := kmsg.NewPtrOffsetFetchRequest()
	req.Group = group
	rt := kmsg.NewOffsetFetchRequestTopic()
	rt.Topic, rt.Partitions = topic, []int32{partition}
	req.Topics = append(req.Topics, rt)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		t.Fatalf("offset fetch %s: %v", group, err)
	}
	for _, tp := range resp.Topics {
		for _, p := range tp.Partitions {
			if tp.Topic == topic && p.Partition == partition {
				return p.Offset
			}
		}
	}
	for _, g := range resp.Groups {
		for _, tp := range g.Topics {
			for _, p := range tp.Partitions {
				if tp.Topic == topic && p.Partition == partition {
					return p.Offset
				}
			}
		}
	}
	return -1
}

// KafkaHeader is the value of key in the message headers
func KafkaHeader(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
