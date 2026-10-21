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

// This group pins down franz-go v1.21.7's own behavior, not this module's: this module's choices (replacing the automatic commit, defaulting to latest,
// Flushing before stopping, probing with a deadline) were all made against these defaults and are written into the comments and the README's "Behavior (measured)".
// Whatever goes red here after a franz-go upgrade is a number in those comments and docs that has to be measured again.

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
		// Comparing text: franz-go stores the same number as an int in some places and an int64 in others.
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

// Why this module switches to AutoCommitMarks: the default automatic commit commits "whatever the last poll returned", processed or not.
// With polling and processing in different goroutines (one per partition), another poll commits the messages still queued.
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
	// Nothing was processed and it polled again (which is exactly what the dispatch goroutine does).
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	cl.PollFetches(short)
	cancelShort()
	cl.Close() // the default OnPartitionsRevoked commits on the way out

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

// The topic does not exist and automatic creation is off (AllowAutoTopicCreation is off by default): ProduceSync retries UnknownTopicRetries times (4) and then reports an error,
// rather than waiting forever. About 1s on kfake.
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

// Why this module turns idempotent writes off on the caller's behalf when Producer.Acks is leader or none.
func TestFranzDefault_NonAllAcksRequireIdempotenceOff(t *testing.T) {
	if _, err := kgo.NewClient(kgo.RequiredAcks(kgo.LeaderAck())); err == nil {
		t.Fatal("franz-go now accepts leader acks with idempotent writes on, the explicit DisableIdempotentWrite may be unnecessary")
	}
}

// refusedAddr is a local address nobody is listening on: connecting to it is refused immediately.
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

// silentAddr is an address that accepts the connection and never answers.
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
