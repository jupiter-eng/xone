package xkafka

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// 这一组钉住的是 franz-go v1.21.7 自己的行为，不是本模块的：本模块的选择（换掉自动提交、默认 latest、
// 停止时先 Flush、探测带截止时间）都是冲着这些默认值做的，写进了注释和 README「行为与实测」。
// 升级 franz-go 之后这里红了，就是那些注释和文档里的数字要重新量。

func TestFranzDefault_Values(t *testing.T) {
	cl, err := kgo.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, c := range []struct {
		name string
		opt  any
		want any
	}{
		{"ClientID", kgo.ClientID, "kgo"},
		{"RequiredAcks", kgo.RequiredAcks, kgo.AllISRAcks()},
		{"DisableIdempotentWrite", kgo.DisableIdempotentWrite, false},
		{"ProducerLinger", kgo.ProducerLinger, 10 * time.Millisecond},
		{"RecordRetries", kgo.RecordRetries, math.MaxInt64},
		{"RecordDeliveryTimeout", kgo.RecordDeliveryTimeout, time.Duration(0)},
		{"ProduceRequestTimeout", kgo.ProduceRequestTimeout, 10 * time.Second},
		{"UnknownTopicRetries", kgo.UnknownTopicRetries, 4},
		{"MaxBufferedRecords", kgo.MaxBufferedRecords, 10000},
		{"AllowAutoTopicCreation", kgo.AllowAutoTopicCreation, false},
		{"DialTimeout", kgo.DialTimeout, 10 * time.Second},
		{"RequestRetries", kgo.RequestRetries, 20},
		{"AutoCommitInterval", kgo.AutoCommitInterval, 5 * time.Second},
		{"SessionTimeout", kgo.SessionTimeout, 45 * time.Second},
		{"RebalanceTimeout", kgo.RebalanceTimeout, 60 * time.Second},
		{"HeartbeatInterval", kgo.HeartbeatInterval, 3 * time.Second},
		{"ConsumeResetOffset", kgo.ConsumeResetOffset, kgo.NewOffset().AtStart()},
	} {
		// 比文本：franz-go 里同一个数有的存成 int、有的存成 int64
		got := cl.OptValue(c.opt)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: franz-go default = %v, the docs say %v", c.name, got, c.want)
		}
	}
	codecs := cl.OptValues(kgo.ProducerBatchCompression)
	if len(codecs) != 1 {
		t.Fatalf("ProducerBatchCompression: %v", codecs)
	}
	if got := codecs[0].([]kgo.CompressionCodec); len(got) != 2 || got[0] != kgo.SnappyCompression() || got[1] != kgo.NoCompression() {
		t.Errorf("default compression = %v, want snappy then none", got)
	}
}

// 本模块换成 AutoCommitMarks 的理由：默认的自动提交提交「上一次 poll 取到的」，不管处理完没有。
// poll 和处理不在一个协程里时（每个分区一个协程），再 poll 一次之后，还排在队列里的消息就算提交了
func TestFranzDefault_AutocommitCommitsPolledButUnprocessed(t *testing.T) {
	_, cfg := cluster(t, 1, "t")
	produce(t, rawClient(t, cfg), "t", 0, "a", "b", "c")

	cl, _ := kgo.NewClient(kgo.SeedBrokers(cfg.Brokers...), kgo.ConsumerGroup("g"), kgo.ConsumeTopics("t"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if n := cl.PollFetches(ctx).NumRecords(); n != 3 {
		t.Fatalf("polled %d, want 3", n)
	}
	// 一条都没处理，又 poll 了一次（派发协程就是这么做的）
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	cl.PollFetches(short)
	cancelShort()
	cl.Close() // 默认的 OnPartitionsRevoked 在离开时提交

	if got := committed(t, cfg, "g", "t", 0); got != 3 {
		t.Fatalf("committed = %d: franz-go's default autocommit no longer commits polled-but-unprocessed records, revisit the docs", got)
	}
}

func TestFranzDefault_ResetOffsetIsEarliest(t *testing.T) {
	_, cfg := cluster(t, 1, "t")
	produce(t, rawClient(t, cfg), "t", 0, "old")
	cl := rawClient(t, cfg, kgo.ConsumerGroup("new-group"), kgo.ConsumeTopics("t"))
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if fs := cl.PollFetches(ctx); fs.NumRecords() != 1 {
		t.Fatalf("a new group read %d old records (err %v), franz-go's default reset offset changed", fs.NumRecords(), fs.Err0())
	}
}

func TestFranzDefault_CloseWithoutFlushFailsBufferedRecords(t *testing.T) {
	_, cfg := cluster(t, 1, "t")
	cl, _ := kgo.NewClient(kgo.SeedBrokers(cfg.Brokers...), kgo.ProducerLinger(time.Minute))
	errs := make(chan error, 10)
	for range 10 {
		cl.Produce(context.Background(), &kgo.Record{Topic: "t", Value: []byte("v")}, func(_ *kgo.Record, err error) { errs <- err })
	}
	cl.Close()
	for range 10 {
		if err := <-errs; !errors.Is(err, kgo.ErrClientClosed) {
			t.Fatalf("buffered record finished with %v, want ErrClientClosed", err)
		}
	}
}

func TestFranzDefault_ProduceSyncHonorsCtxDeadline(t *testing.T) {
	cl, _ := kgo.NewClient(kgo.SeedBrokers(refusedAddr(t)))
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := cl.ProduceSync(ctx, &kgo.Record{Topic: "t", Value: []byte("v")}).FirstErr()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("ProduceSync to a down broker: %v after %v", err, time.Since(start))
	}
}

// topic 不存在、不让自动建（AllowAutoTopicCreation 默认关）：ProduceSync 试满 UnknownTopicRetries（4 次）就报错，
// 不是一直等。kfake 上约 1s
func TestFranzDefault_ProduceToMissingTopicFails(t *testing.T) {
	_, cfg := cluster(t, 1, "t")
	cl := rawClient(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	err := cl.ProduceSync(ctx, &kgo.Record{Topic: "missing", Value: []byte("v")}).FirstErr()
	if !errors.Is(err, kerr.UnknownTopicOrPartition) {
		t.Fatalf("produce to a missing topic: %v", err)
	}
}

// Producer.Acks 配 leader / none 时本模块替使用者关掉幂等写的理由
func TestFranzDefault_NonAllAcksRequireIdempotenceOff(t *testing.T) {
	if _, err := kgo.NewClient(kgo.RequiredAcks(kgo.LeaderAck())); err == nil {
		t.Fatal("franz-go now accepts leader acks with idempotent writes on, the explicit DisableIdempotentWrite may be unnecessary")
	}
}

// refusedAddr 一个没人在听的本机地址：连上去立刻被拒绝
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// silentAddr 收下连接、从不回话的地址
func silentAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	return ln.Addr().String()
}
