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

// Kafka 的用例：服务的 XKafka 只在 Options.Kafka 时有（service/application-kafka.yml 那份 profile），
// 每个用例自己建 topic（KafkaTopic），用完删掉，所以可以 t.Parallel。
//
//	XONE_E2E_KAFKA_ADDR  默认 127.0.0.1:9092
//	XONE_E2E_KAFKA       scripts/e2e.sh 起不来 Kafka 时设成 0，Kafka 的用例跳过（RequireKafka）

// KafkaAddr Kafka broker 的 host:port，不经代理
func KafkaAddr() string { return env("XONE_E2E_KAFKA_ADDR", "127.0.0.1:9092") }

// RequireKafka Kafka 用例的第一行：e2e 没开、或者 Kafka 不可用时跳过。
//
// Kafka 跑在 Docker 里，不是每台机器都有：scripts/e2e.sh 起不来它时设 XONE_E2E_KAFKA=0，
// 这里跳过并说清为什么；没经过脚本直接跑的，向 broker 要一次元数据，要不到同样跳过。
// 只拨端口不够：docker 的端口转发在 broker 起来之前就在听了
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

// KafkaClient 直连 Kafka 的原生客户端，测试里写消息、核对用，不带 xkafka 的任何钩子。
// 写消息时按 Record.Partition 指定分区。测试结束时关掉
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

// KafkaTopic 建一个 partitions 个分区的新 topic，名字是 e2e_<随机>，测试结束时删掉
func KafkaTopic(t testing.TB, partitions int32) string {
	t.Helper()
	name := "e2e_" + NewID()
	CreateKafkaTopic(t, name, partitions)
	return name
}

// CreateKafkaTopic 建一个指定名字的 topic，测试结束时删掉。已经有了算失败
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

// KafkaProduce 往 topic 的 partition 里写一条，key 可以是 nil
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

// KafkaRead 从头读 topic，直到读到 n 条或者 timeout 到点；返回读到的（可能不足 n 条）
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

// KafkaCommitted 消费组在 topic/partition 上提交的 offset，没提交过是 -1
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

// KafkaHeader 消息头里 key 的值
func KafkaHeader(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
