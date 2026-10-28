package xkafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xutil"
)

func ok(rec *recorder) func(context.Context, *kgo.Record) error {
	return func(_ context.Context, r *kgo.Record) error { rec.add(r); return nil }
}

func TestConsume_ProcessesAndCommits(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a", "b", "c")

	m := testManager(t, cfg)
	var rec recorder
	register(t, m, "orders", "g1", ok(&rec))
	start(t, m)
	eventually(t, func() bool { return rec.count() == 3 }, "3 records processed, got %v", rec.values())
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got != 3 {
		t.Fatalf("committed offset = %d, want 3", got)
	}
}

func TestConsume_OrderedWithinPartition(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	var values []string
	for i := range 200 {
		values = append(values, strconv.Itoa(i))
	}
	produce(t, rawClient(t, cfg), "orders", 0, values...)

	m := testManager(t, cfg)
	var rec recorder
	var concurrent, maxConcurrent atomic.Int32
	register(t, m, "orders", "g1", func(_ context.Context, r *kgo.Record) error {
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		if n > maxConcurrent.Load() {
			maxConcurrent.Store(n)
		}
		rec.add(r)
		return nil
	})
	start(t, m)
	eventually(t, func() bool { return rec.count() == 200 }, "200 records")
	for i, v := range rec.values() {
		if v != strconv.Itoa(i) {
			t.Fatalf("record %d is %s: out of order within a partition", i, v)
		}
	}
	if maxConcurrent.Load() != 1 {
		t.Fatalf("%d handlers ran at once on one partition", maxConcurrent.Load())
	}
}

// p0's handler returns only once p1 has finished: were the partitions serial, this would wait forever.
func TestConsume_ParallelAcrossPartitions(t *testing.T) {
	_, cfg := cluster(t, 2, "orders")
	cl := rawClient(t, cfg, manualPartitioner())
	produce(t, cl, "orders", 0, "p0")
	produce(t, cl, "orders", 1, "p1")

	m := testManager(t, cfg)
	p1Done := make(chan struct{})
	var rec recorder
	register(t, m, "orders", "g1", func(ctx context.Context, r *kgo.Record) error {
		if r.Partition == 0 {
			select {
			case <-p1Done:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			close(p1Done)
		}
		rec.add(r)
		return nil
	}, WithTimeout(wait))
	start(t, m)
	eventually(t, func() bool { return rec.count() == 2 }, "both partitions processed, got %v", rec.values())
}

// Simulating a crash: processing blocks on the second message, so what has been committed by then reaches only the first (all three were already polled);
// the stop budget runs out at once, the blocked message is never marked, and after a restart processing continues from the second.
func TestConsume_MarksOnlyAfterProcessing(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a", "b", "c")

	m := testManager(t, cfg)
	stuck := make(chan struct{})
	c := register(t, m, "orders", "g1", func(ctx context.Context, r *kgo.Record) error {
		if string(r.Value) == "b" {
			close(stuck)
			<-ctx.Done() // cancelled only once the stop budget runs out
			return ctx.Err()
		}
		return nil
	}, WithTimeout(wait), WithRetry(0))
	start(t, m)
	<-stuck
	if err := c.cl.CommitMarkedOffsets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got != 1 {
		t.Fatalf("committed = %d while b is in flight, want 1 (only a is done)", got)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.stop(expired); err == nil {
		t.Fatal("stop with no budget left should report the busy partition")
	}

	m2 := testManager(t, cfg)
	var rec recorder
	register(t, m2, "orders", "g1", ok(&rec))
	start(t, m2)
	eventually(t, func() bool { return rec.count() == 2 }, "b and c redelivered, got %v", rec.values())
	if got := rec.values(); got[0] != "b" || got[1] != "c" {
		t.Fatalf("after restart got %v, want [b c]", got)
	}
}

func TestConsume_RetriesThenSucceeds(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a")

	m := testManager(t, cfg)
	var calls atomic.Int32
	done := make(chan struct{})
	register(t, m, "orders", "g1", func(context.Context, *kgo.Record) error {
		if calls.Add(1) < 3 {
			return errors.New("downstream busy")
		}
		close(done)
		return nil
	})
	start(t, m)
	<-done
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("handler called %d times", calls.Load())
	}
	if got := committed(t, cfg, "g1", "orders", 0); got != 1 {
		t.Fatalf("committed = %d", got)
	}
	warns := withMsg(logs(), "kafka message failed")
	if len(warns) != 2 || warns[0]["attempt"] != float64(1) || warns[1]["attempt"] != float64(2) ||
		warns[0]["error"] != "downstream busy" || warns[0]["topic"] != "orders" || warns[0]["level"] != "WARN" {
		t.Fatalf("failure logs: %v", warns)
	}
	if n := len(withMsg(logs(), "kafka message dead-lettered")); n != 0 {
		t.Fatalf("succeeded on the third try but %d dead letters", n)
	}
}

// There is backoff between retries, and it is not waited for on shutdown. The backoff itself (doubling bounds with jitter) is xutil.Retry's and is pinned down by its tests.
// The first backoff bound is set to an hour: the jitter falls in [0, 1h], so the chance of a second attempt within 300ms is about 8 in 100 thousand.
func TestConsume_RetryBackoffWaitsAndStopInterruptsIt(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a")
	old := retryInterval
	retryInterval = time.Hour
	t.Cleanup(func() { retryInterval = old })

	m := testManager(t, cfg)
	var calls atomic.Int32
	first := make(chan struct{})
	register(t, m, "orders", "g1", func(context.Context, *kgo.Record) error {
		if calls.Add(1) == 1 {
			close(first)
		}
		return errors.New("again")
	})
	start(t, m)
	<-first
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("retried without waiting: %d calls", calls.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.stop(ctx); err != nil {
		t.Fatalf("stop waited for the retry backoff: %v", err)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got > 0 {
		t.Fatalf("a record interrupted mid-retry was committed: %d", got)
	}
}

func TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders", "orders.dlq")
	cl := rawClient(t, cfg)
	orig := &kgo.Record{Topic: "orders", Key: []byte("user-42"), Value: []byte("payload"),
		Headers: []kgo.RecordHeader{{Key: "tenant", Value: []byte("acme")}}}
	if err := cl.ProduceSync(context.Background(), orig).FirstErr(); err != nil {
		t.Fatal(err)
	}

	m := testManager(t, cfg)
	var calls atomic.Int32
	long := "db said: " + strings.Repeat("x", 1000)
	register(t, m, "orders", "g1", func(context.Context, *kgo.Record) error {
		calls.Add(1)
		return errors.New(long)
	}, WithRetry(2))
	start(t, m)

	dl := readAll(t, cfg, "orders.dlq", 1)[0]
	if calls.Load() != 3 {
		t.Fatalf("handler called %d times, want 1 + 2 retries", calls.Load())
	}
	if string(dl.Key) != "user-42" || string(dl.Value) != "payload" || header(dl, "tenant") != "acme" {
		t.Errorf("dead letter lost the original record: key=%s value=%s headers=%v", dl.Key, dl.Value, dl.Headers)
	}
	for k, want := range map[string]string{
		HeaderDLQTopic: "orders", HeaderDLQPartition: "0", HeaderDLQOffset: "0", HeaderDLQGroup: "g1",
	} {
		if got := header(dl, k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if e := header(dl, HeaderDLQError); len(e) != maxDLQError || !strings.HasPrefix(e, "db said: ") {
		t.Errorf("error header: %d bytes", len(e))
	}
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got != 1 {
		t.Fatalf("dead-lettered record not committed: %d", got)
	}
	w := withMsg(logs(), "kafka message dead-lettered")
	if len(w) != 1 || w[0]["dlq_topic"] != "orders.dlq" || w[0]["attempts"] != float64(3) || w[0]["level"] != "WARN" {
		t.Fatalf("dead letter log: %v", w)
	}
}

func TestConsume_PermanentErrorSkipsRetries(t *testing.T) {
	_, cfg := cluster(t, 1, "orders", "orders.dlq")
	produce(t, rawClient(t, cfg), "orders", 0, "bad")

	m := testManager(t, cfg)
	var calls atomic.Int32
	register(t, m, "orders", "g1", func(context.Context, *kgo.Record) error {
		calls.Add(1)
		return xutil.Permanent(errors.New("malformed payload"))
	})
	start(t, m)
	dl := readAll(t, cfg, "orders.dlq", 1)[0]
	if calls.Load() != 1 {
		t.Fatalf("permanent error retried: %d calls", calls.Load())
	}
	if header(dl, HeaderDLQError) != "malformed payload" {
		t.Errorf("error header = %q", header(dl, HeaderDLQError))
	}
}

func TestConsume_DeadLetterDisabledSkipsWithError(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "bad", "good")

	m := testManager(t, cfg)
	var rec recorder
	register(t, m, "orders", "g1", func(_ context.Context, r *kgo.Record) error {
		if string(r.Value) == "bad" {
			return errors.New("nope")
		}
		rec.add(r)
		return nil
	}, WithRetry(1), WithDeadLetter(""))
	start(t, m)
	eventually(t, func() bool { return rec.count() == 1 }, "the next record is processed after the skip")
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got != 2 {
		t.Fatalf("committed = %d, want 2", got)
	}
	e := withMsg(logs(), "kafka message skipped, retries exhausted and dead lettering is off")
	if len(e) != 1 || e[0]["level"] != "ERROR" || e[0]["attempts"] != float64(2) || e[0]["offset"] != float64(0) {
		t.Fatalf("skip log: %v", e)
	}
}

// The dead-letter topic does not exist (kfake does not create it automatically): a failed write is retried forever with nothing marked and nothing processed after it; on shutdown it gives up and the message is redelivered later.
func TestConsume_DeadLetterFailureBlocksWithoutMarking(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "bad", "next")

	m := testManager(t, cfg)
	var rec recorder
	register(t, m, "orders", "g1", func(_ context.Context, r *kgo.Record) error {
		rec.add(r)
		if string(r.Value) == "bad" {
			return xutil.Permanent(errors.New("nope"))
		}
		return nil
	}, WithDeadLetter("missing.dlq"), WithTimeout(300*time.Millisecond)) // each dead-letter write is bounded by it too, which makes the retries come faster
	start(t, m)
	eventually(t, func() bool { return len(withMsg(logs(), "kafka dead letter produce failed, retrying")) >= 3 },
		"dead letter produce retried")
	if got := rec.values(); len(got) != 1 {
		t.Fatalf("moved on past a record that is not dead-lettered yet: %v", got)
	}
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// bad was abandoned on shutdown: next, which comes after it, must not be processed either, since processing it would mark and commit an offset past bad.
	if got := rec.values(); len(got) != 1 {
		t.Fatalf("records after the abandoned one were processed: %v", got)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got > 0 {
		t.Fatalf("committed = %d, the record that never reached the dead letter topic was marked", got)
	}
}

func TestConsume_PanicIsRecoveredAndRetried(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a", "b")

	m := testManager(t, cfg)
	var calls atomic.Int32
	var rec recorder
	register(t, m, "orders", "g1", func(_ context.Context, r *kgo.Record) error {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		rec.add(r)
		return nil
	})
	start(t, m)
	eventually(t, func() bool { return rec.count() == 2 }, "both records processed after a panic")
	p := withMsg(logs(), "kafka handler panicked")
	if len(p) != 1 || p[0]["level"] != "ERROR" || !strings.Contains(fmt.Sprint(p[0]["stack"]), "consume_test.go") ||
		p[0]["error"] != "panicked: boom" {
		t.Fatalf("panic log: %v", p)
	}
}

func TestConsume_TimeoutCancelsHandlerCtx(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a")

	m := testManager(t, cfg)
	errs := make(chan error, 2)
	register(t, m, "orders", "g1", func(ctx context.Context, _ *kgo.Record) error {
		<-ctx.Done()
		errs <- ctx.Err()
		return ctx.Err()
	}, WithTimeout(50*time.Millisecond), WithRetry(1), WithDeadLetter(""))
	start(t, m)
	for range 2 { // each retry has a timeout of its own
		if err := <-errs; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("handler ctx ended with %v, want deadline exceeded", err)
		}
	}
}

// On shutdown: the ctx of the handler in flight is not cancelled, and it stops only once that has finished and been committed.
func TestStop_WaitsForInFlightAndCommits(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a", "b")

	m := testManager(t, cfg)
	started, release := make(chan struct{}), make(chan struct{})
	var ctxErr atomic.Value
	var calls atomic.Int32
	register(t, m, "orders", "g1", func(ctx context.Context, _ *kgo.Record) error {
		calls.Add(1)
		close(started)
		<-release
		ctxErr.Store(fmt.Sprint(ctx.Err()))
		return nil
	}, WithTimeout(wait))
	start(t, m)
	<-started

	stopped := make(chan error, 1)
	go func() { stopped <- m.stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop returned (%v) while a message was in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if got := ctxErr.Load(); got != "<nil>" {
		t.Fatalf("the in-flight handler's ctx was canceled at stop: %v", got)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got != 1 {
		t.Fatalf("committed = %d, want 1 (a done, b never started)", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("started %d messages after stop began", calls.Load())
	}
}

func TestStop_DeadlineNamesBusyPartitionsAndCancelsHandler(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a")

	m := testManager(t, cfg)
	started := make(chan struct{})
	canceled := make(chan struct{})
	register(t, m, "orders", "g1", func(ctx context.Context, _ *kgo.Record) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}, WithTimeout(time.Hour))
	start(t, m)
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := m.stop(ctx)
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xkafka" || xe.Op != "stop" || !strings.Contains(err.Error(), "orders/0@0 (group g1)") {
		t.Fatalf("stop error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(wait):
		t.Fatal("the handler's ctx was not canceled when the stop budget ran out")
	}
	w := withMsg(logs(), "xkafka consumers still stopping when the stop budget ran out")
	if len(w) != 1 || w[0]["level"] != "WARN" {
		t.Fatalf("busy log: %v", w)
	}
}

func TestStop_DeadlineCancelsHandlersDuringRebalance(t *testing.T) {
	_, cfg := cluster(t, 2, "orders")
	cl := rawClient(t, cfg, manualPartitioner())
	produce(t, cl, "orders", 0, "p0")
	produce(t, cl, "orders", 1, "p1")

	entered, canceled := make(chan struct{}, 2), make(chan struct{}, 2)
	m1 := testManager(t, cfg)
	c1 := register(t, m1, "orders", "g1", func(ctx context.Context, _ *kgo.Record) error {
		entered <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		return ctx.Err()
	}, WithTimeout(time.Hour))
	start(t, m1)
	defer c1.hardCancel() // release the handlers even if the shutdown regression returns
	for range 2 {
		select {
		case <-entered:
		case <-time.After(wait):
			t.Fatal("both partitions must have a handler in flight")
		}
	}
	m2 := testManager(t, cfg)
	register(t, m2, "orders", "g1", func(context.Context, *kgo.Record) error { return nil })
	start(t, m2)
	eventually(t, func() bool {
		c1.mu.Lock()
		defer c1.mu.Unlock()
		return len(c1.assigned) < 2
	}, "the first member is revoking a partition while its handler is in flight")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m1.stop(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "orders/") {
			t.Fatalf("shutdown must name the unfinished partition and deadline: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop remained blocked on polling after its deadline")
	}
	for range 2 {
		select {
		case <-canceled:
		case <-time.After(2 * time.Second):
			t.Fatal("the stop deadline did not cancel every handler")
		}
	}
	select {
	case <-c1.pollDone:
	case <-time.After(2 * time.Second):
		t.Fatal("polling did not exit after the handlers were canceled")
	}
}

// Two group members: the first gets both partitions and finishes processing; the second joins and cooperative-sticky revokes one partition for it.
// The marks were committed before the revocation, so the second continues from the committed position and does not reprocess what the first already did
// (ResetOffset is earliest, so without the commit the second would start over).
func TestRebalance_CommitsBeforeRevoke(t *testing.T) {
	_, cfg := cluster(t, 2, "orders")
	cl := rawClient(t, cfg, manualPartitioner())
	for p := range int32(2) {
		produce(t, cl, "orders", p, fmt.Sprintf("p%d-1", p), fmt.Sprintf("p%d-2", p))
	}

	var rec1, rec2 recorder
	m1 := testManager(t, cfg)
	c1 := register(t, m1, "orders", "g1", ok(&rec1))
	start(t, m1)
	waitAssigned(t, c1, 2)
	eventually(t, func() bool { return rec1.count() == 4 }, "first member processed everything")

	m2 := testManager(t, cfg)
	c2 := register(t, m2, "orders", "g1", ok(&rec2))
	start(t, m2)
	waitAssigned(t, c2, 1)
	waitAssigned(t, c1, 1)

	for p := range int32(2) {
		produce(t, cl, "orders", p, fmt.Sprintf("p%d-3", p))
	}
	eventually(t, func() bool { return rec1.count()+rec2.count() >= 6 }, "new records processed")
	time.Sleep(200 * time.Millisecond) // give a duplicate time to surface if there is one
	seen := map[string]int{}
	for _, v := range append(rec1.values(), rec2.values()...) {
		seen[v]++
	}
	for v, n := range seen {
		if n > 1 {
			t.Fatalf("%s processed %d times: the revoked partition was not committed before the handover (m1=%v m2=%v)",
				v, n, rec1.values(), rec2.values())
		}
	}
}

// A revoked partition is handed over only once the message in flight has been processed: the new member neither processes it at the same time nor processes it again.
func TestRebalance_WaitsForInFlightBeforeRevoke(t *testing.T) {
	_, cfg := cluster(t, 2, "orders")
	cl := rawClient(t, cfg, manualPartitioner())
	produce(t, cl, "orders", 0, "p0")
	produce(t, cl, "orders", 1, "p1")

	var inFlight atomic.Int32
	release := make(chan struct{})
	m1 := testManager(t, cfg)
	c1 := register(t, m1, "orders", "g1", func(context.Context, *kgo.Record) error {
		inFlight.Add(1)
		<-release
		return nil
	}, WithTimeout(wait))
	start(t, m1)
	eventually(t, func() bool { return inFlight.Load() == 2 }, "both partitions in flight on the first member")

	var rec2 recorder
	m2 := testManager(t, cfg)
	c2 := register(t, m2, "orders", "g1", ok(&rec2))
	start(t, m2)
	// The rebalance waits for the first member: revoking a partition waits for the message in flight to be processed, so the second member is not assigned it in the meantime, let alone processes it.
	// Without that wait, the second member would be assigned the partition within 1s and start over (a rebalance on kfake is far faster than 3s).
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if rec2.count() != 0 {
			t.Fatalf("second member processed %v while the first was still handling it", rec2.values())
		}
	}
	close(release)
	waitAssigned(t, c2, 1)
	waitAssigned(t, c1, 1)
	time.Sleep(300 * time.Millisecond)
	if rec2.count() != 0 {
		t.Fatalf("the in-flight record was handed over and processed again: %v", rec2.values())
	}
}

func TestSessionTimeout_ReachesTheClientAndZeroKeepsFranzsDefault(t *testing.T) {
	// How long a partition stops being consumed after a process is killed outright, so what
	// matters is that the number a caller wrote is the number franz-go holds -- and that leaving
	// the field out keeps franz-go's own, rather than passing a zero on, which franz-go refuses.
	for _, c := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"unset keeps franz-go's default", 0, 45 * time.Second},
		{"set reaches the client", 10 * time.Second, 10 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, cfg := cluster(t, 1, "orders")
			cfg.Consumer.SessionTimeout = c.set
			m := testManager(t, cfg)
			con := register(t, m, "orders", "g1", ok(nil))
			start(t, m)
			waitAssigned(t, con, 1)
			if got := con.cl.OptValue(kgo.SessionTimeout); got != c.want {
				t.Errorf("SessionTimeout reaching franz-go = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResetOffset_LatestSkipsOldRecords(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	cfg.Consumer.ResetOffset = "latest"
	cl := rawClient(t, cfg)
	produce(t, cl, "orders", 0, "old")

	m := testManager(t, cfg)
	var rec recorder
	c := register(t, m, "orders", "g1", ok(&rec))
	start(t, m)
	waitAssigned(t, c, 1)
	// After being assigned a partition, franz-go still asks the broker where "latest" is: keep writing until it has received that.
	eventually(t, func() bool {
		produce(t, cl, "orders", 0, "new")
		time.Sleep(50 * time.Millisecond)
		return rec.count() > 0
	}, "a record written after the group joined is consumed")
	for _, v := range rec.values() {
		if v == "old" {
			t.Fatal("latest reset offset consumed a record written before the group existed")
		}
	}
}

func TestResetOffset_EarliestReadsOldRecords(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	cfg.Consumer.ResetOffset = "earliest"
	produce(t, rawClient(t, cfg), "orders", 0, "old")
	m := testManager(t, cfg)
	var rec recorder
	register(t, m, "orders", "g1", ok(&rec))
	start(t, m)
	eventually(t, func() bool { return rec.count() == 1 }, "old record consumed with earliest")
}

func TestResetOffset_DefaultIsLatest(t *testing.T) {
	if got := DefaultClientConfig().Consumer.ResetOffset; got != "latest" {
		t.Fatalf("default ResetOffset = %q", got)
	}
	if resetOffset("latest") != kgo.NewOffset().AtEnd() || resetOffset("earliest") != kgo.NewOffset().AtStart() {
		t.Fatal("resetOffset mapping")
	}
}

func TestConsume_TraceContinuesFromProducer(t *testing.T) {
	rec := tracing(t)
	_, cfg := cluster(t, 1, "orders")
	producer := newProducer(t, cfg)

	m := testManager(t, cfg)
	seen := make(chan trace.SpanContext, 1)
	c := register(t, m, "orders", "g1", func(ctx context.Context, _ *kgo.Record) error {
		seen <- trace.SpanContextFromContext(ctx)
		return nil
	})
	start(t, m)
	waitAssigned(t, c, 1)

	ctx, parent := otel.Tracer("test").Start(context.Background(), "http request")
	if err := producer.ProduceSync(ctx, &kgo.Record{Topic: "orders", Key: []byte("user-42"), Value: []byte("v")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	parent.End()
	sc := <-seen
	if sc.TraceID() != parent.SpanContext().TraceID() {
		t.Fatalf("consumer trace %s != producer trace %s", sc.TraceID(), parent.SpanContext().TraceID())
	}
	eventually(t, func() bool { return len(spansNamed(rec, "orders process")) == 1 }, "process span ended")
	s := spansNamed(rec, "orders process")[0]
	pub := spansNamed(rec, "orders publish")[0]
	if s.SpanKind() != trace.SpanKindConsumer || s.Parent().SpanID() != pub.SpanContext().SpanID() {
		t.Errorf("process span kind %v parent %s, want consumer under %s", s.SpanKind(), s.Parent().SpanID(), pub.SpanContext().SpanID())
	}
	attrs := map[string]string{}
	for _, a := range s.Attributes() {
		attrs[string(a.Key)] = a.Value.Emit()
	}
	if attrs["messaging.system"] != "kafka" || attrs["messaging.destination.name"] != "orders" ||
		attrs["messaging.consumer.group.name"] != "g1" || attrs["messaging.kafka.offset"] != "0" ||
		attrs["messaging.destination.partition.id"] != "0" {
		t.Errorf("process span attributes: %v", attrs)
	}
	for k, v := range attrs {
		if v == "user-42" || v == "v" {
			t.Errorf("key or value in span attribute %s", k)
		}
	}
}

func TestConsume_FailedMessageMarksSpanWithoutErrorText(t *testing.T) {
	rec := tracing(t)
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a")
	m := testManager(t, cfg)
	register(t, m, "orders", "g1", func(context.Context, *kgo.Record) error {
		return xutil.Permanent(errors.New("password=hunter2"))
	}, WithDeadLetter(""))
	start(t, m)
	eventually(t, func() bool { return len(spansNamed(rec, "orders process")) == 1 }, "process span ended")
	s := spansNamed(rec, "orders process")[0]
	if s.Status().Description != "kafka message failed" || strings.Contains(fmt.Sprint(s.Events()), "hunter2") {
		t.Fatalf("span status %+v events %v", s.Status(), s.Events())
	}
}

// A log written inside the handler carries topic / partition / offset / group (with xlog installed); this module's own logs hold each field exactly once.
func TestConsume_LogFields(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	cl := rawClient(t, cfg)
	if err := cl.ProduceSync(context.Background(), &kgo.Record{Topic: "orders", Key: []byte("user-42"), Value: []byte("secret-value")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	m := testManager(t, cfg)
	var calls atomic.Int32
	done := make(chan struct{})
	register(t, m, "orders", "g1", func(ctx context.Context, _ *kgo.Record) error {
		slog.InfoContext(ctx, "inside handler")
		if calls.Add(1) == 1 {
			return errors.New("first try fails")
		}
		close(done)
		return nil
	})
	start(t, m)
	<-done
	eventually(t, func() bool { return len(withMsg(logs(), "kafka message processed")) == 1 }, "success log")

	in := withMsg(logs(), "inside handler")[0]
	if in["topic"] != "orders" || in["partition"] != float64(0) || in["offset"] != float64(0) || in["group"] != "g1" {
		t.Errorf("handler log lacks the scope fields: %v", in)
	}
	for _, msg := range []string{"kafka message failed", "kafka message processed"} {
		l := withMsg(logs(), msg)[0]
		if l["topic"] != "orders" || l["group"] != "g1" || l["key"] != "user-42" || l["offset"] != float64(0) {
			t.Errorf("%s: %v", msg, l)
		}
	}
	if l := withMsg(logs(), "kafka message processed")[0]; l["level"] != "DEBUG" || l["elapsed_ms"] == nil {
		t.Errorf("success log: %v", l)
	}
	for _, l := range logs() {
		for k, v := range l {
			if strings.Contains(fmt.Sprint(v), "secret-value") {
				t.Errorf("record value in log field %s: %v", k, l)
			}
		}
	}
}

func TestConsume_LogOffKeepsSkipLogs(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "orders")
	cfg.Log = false
	produce(t, rawClient(t, cfg), "orders", 0, "bad", "good")
	m := testManager(t, cfg)
	var rec recorder
	register(t, m, "orders", "g1", func(_ context.Context, r *kgo.Record) error {
		if string(r.Value) == "bad" {
			return errors.New("nope")
		}
		rec.add(r)
		return nil
	}, WithRetry(0), WithDeadLetter(""))
	start(t, m)
	eventually(t, func() bool { return rec.count() == 1 }, "good processed")
	if n := len(withMsg(logs(), "kafka message failed")) + len(withMsg(logs(), "kafka message processed")); n != 0 {
		t.Fatalf("Log off but %d per-message lines", n)
	}
	if n := len(withMsg(logs(), "kafka message skipped, retries exhausted and dead lettering is off")); n != 1 {
		t.Fatal("a skipped message must be logged even with Log off")
	}
}

func TestConsume_MetricObserved(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a", "bad")
	m := testManager(t, cfg)
	register(t, m, "orders", "g-metric", func(_ context.Context, r *kgo.Record) error {
		if string(r.Value) == "bad" {
			return errors.New("nope")
		}
		return nil
	}, WithRetry(0), WithDeadLetter(""))
	start(t, m)
	eventually(t, func() bool {
		h := consumeHist.Load()
		return h != nil && observations(h, map[string]string{"group": "g-metric"}) == 2
	}, "two messages observed")
	h := consumeHist.Load()
	for _, status := range []string{"ok", "skipped"} {
		if n := observations(h, map[string]string{"topic": "orders", "group": "g-metric", "status": status}); n != 1 {
			t.Errorf("status %s: %d observations", status, n)
		}
	}
}

func TestConsume_MetricOffRecordsNothing(t *testing.T) {
	registerMetrics() // so there is a histogram to record into when this one runs alone: a cluster with Metric off must not record into it
	_, cfg := cluster(t, 1, "orders")
	cfg.Metric = false
	produce(t, rawClient(t, cfg), "orders", 0, "a")
	m := testManager(t, cfg)
	var rec recorder
	register(t, m, "orders", "g-nometric", ok(&rec))
	start(t, m)
	eventually(t, func() bool { return rec.count() == 1 }, "processed")
	if h := consumeHist.Load(); h != nil {
		if n := observations(h, map[string]string{"group": "g-nometric"}); n != 0 {
			t.Fatalf("Metric off but %d observations", n)
		}
	}
}

func TestConsume_ValidationErrors(t *testing.T) {
	fn := func(context.Context, *kgo.Record) error { return nil }
	for _, c := range []struct {
		name, topic, group string
		fn                 func(context.Context, *kgo.Record) error
		opts               []Option
		want               string
	}{
		{"empty topic", "", "g", fn, nil, "topic is empty"},
		{"blank group", "orders", "  ", fn, nil, `consume "orders": group is empty`},
		{"nil handler", "orders", "g", nil, nil, "handler is nil"},
		{"zero timeout", "orders", "g", fn, []Option{WithTimeout(0)}, "timeout must be > 0"},
		{"negative retry", "orders", "g", fn, []Option{WithRetry(-1)}, "retry count must be >= 0"},
		{"dlq is the topic", "orders", "g", fn, []Option{WithDeadLetter("orders")}, "must differ"},
		{"blank dlq", "orders", "g", fn, []Option{WithDeadLetter(" ")}, "dead letter topic is blank"},
		{"blank client", "orders", "g", fn, []Option{WithClient("")}, "client name is empty"},
	} {
		err := Consume(c.topic, c.group, c.fn, c.opts...)
		var xe *xerror.Error
		if !errors.As(err, &xe) || xe.Module != "xkafka" || xe.Op != "config" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestOptions_Defaults(t *testing.T) {
	o := buildOptions("orders", nil)
	if o.client != "default" || o.timeout != 30*time.Second || o.retries != 3 || o.dlq != "orders.dlq" {
		t.Fatalf("defaults: %+v", o)
	}
}

func TestConsume_DuplicateIsConfigError(t *testing.T) {
	m := &manager{lookup: func(string) (instance, bool) { return instance{}, false }}
	fn := func(context.Context, *kgo.Record) error { return nil }
	if err := m.add(&consumer{topic: "orders", group: "g", fn: fn, o: buildOptions("orders", nil)}); err != nil {
		t.Fatal(err)
	}
	err := m.add(&consumer{topic: "orders", group: "g", fn: fn, o: buildOptions("orders", nil)})
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "config" || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate: %v", err)
	}
	// Another group may do it as usual.
	if err := m.add(&consumer{topic: "orders", group: "g2", fn: fn, o: buildOptions("orders", nil)}); err != nil {
		t.Fatal(err)
	}
}

func TestConsume_AfterStopIsRegisterError(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	m := testManager(t, cfg)
	start(t, m)
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := m.add(&consumer{topic: "orders", group: "g", fn: func(context.Context, *kgo.Record) error { return nil }, o: buildOptions("orders", nil)})
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "register" {
		t.Fatalf("after stop: %v", err)
	}
}

func TestConsume_AfterStartStartsImmediately(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfg), "orders", 0, "a")
	m := testManager(t, cfg)
	start(t, m)
	var rec recorder
	register(t, m, "orders", "g1", ok(&rec))
	eventually(t, func() bool { return rec.count() == 1 }, "registered after start, consumed right away")
}

func TestConsume_UnknownClientFailsStart(t *testing.T) {
	_, cfg := cluster(t, 1, "orders")
	m := testManager(t, cfg)
	register(t, m, "orders", "g1", func(context.Context, *kgo.Record) error { return nil }, WithClient("audit"))
	err := m.start(context.Background())
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "config" || !strings.Contains(err.Error(), `no Kafka client named "audit"`) {
		t.Fatalf("start: %v", err)
	}
	// Once it cannot come up, no further registration is accepted.
	if err := m.add(&consumer{topic: "x", group: "g", fn: func(context.Context, *kgo.Record) error { return nil }, o: buildOptions("x", nil)}); err == nil {
		t.Fatal("add after a failed start should fail")
	}
}

func TestConsume_WithClientPicksCluster(t *testing.T) {
	_, cfgA := cluster(t, 1, "orders")
	_, cfgB := cluster(t, 1, "orders")
	produce(t, rawClient(t, cfgB), "orders", 0, "from-b")
	m := testManager(t, cfgA, map[string]ClientConfig{"audit": cfgB})
	var rec recorder
	register(t, m, "orders", "g1", ok(&rec), WithClient("audit"))
	start(t, m)
	eventually(t, func() bool { return rec.count() == 1 }, "consumed from the audit cluster")
}

func TestConsume_MissingTopicWarnsAndWaits(t *testing.T) {
	logs := captureLogs(t)
	_, cfg := cluster(t, 1, "other")
	m := testManager(t, cfg)
	register(t, m, "later", "g1", func(context.Context, *kgo.Record) error { return nil })
	start(t, m)
	w := withMsg(logs(), "kafka topic does not exist yet, the consumer waits for it to be created")
	if len(w) != 1 || w[0]["topic"] != "later" || w[0]["level"] != "WARN" {
		t.Fatalf("missing topic log: %v", logs())
	}
}

func TestDLQRecord_TruncatesErrorAndKeepsHeaders(t *testing.T) {
	r := &kgo.Record{Topic: "orders", Partition: 3, Offset: 42, Key: []byte("k"), Value: []byte("v"),
		Headers: []kgo.RecordHeader{{Key: "traceparent", Value: []byte("00-x")}}}
	d := dlqRecord(r, "orders.dlq", "g", errors.New(strings.Repeat("é", 300)+"\xff"))
	if d.Topic != "orders.dlq" || header(d, HeaderDLQPartition) != "3" || header(d, HeaderDLQOffset) != "42" ||
		header(d, "traceparent") != "00-x" {
		t.Fatalf("dlq record: %+v", d)
	}
	if e := header(d, HeaderDLQError); len(e) > maxDLQError || !strings.HasPrefix(e, "é") || strings.ContainsRune(e, '�') {
		t.Fatalf("error header %d bytes", len(e))
	}
	if len(r.Headers) != 1 {
		t.Fatal("building the dead letter mutated the original record's headers")
	}
}

// uncomparableError is an error type whose dynamic type cannot be compared with == , as
// validator.ValidationErrors ([]FieldError) and mongo's WriteException (a value with slice fields) are.
// Handing two of them to == panics, which is why the outcome of a message is decided by the stopping ctx
// and never by comparing error values.
type uncomparableError []string

func (e uncomparableError) Error() string { return "invalid: " + strings.Join([]string(e), ", ") }

// An error type that cannot be compared used to take the whole process down the moment a message ran out of
// retries: deciding "was this interrupted" compared the error xutil.Retry returned against the last one the
// handler gave, and that comparison panics on such a type.
func TestConsume_UncomparableHandlerErrorStillDeadLetters(t *testing.T) {
	_, cfg := cluster(t, 1, "orders", "orders.dlq")
	produce(t, rawClient(t, cfg), "orders", 0, "bad", "good")

	m := testManager(t, cfg)
	rec := &recorder{}
	register(t, m, "orders", "g1", func(_ context.Context, r *kgo.Record) error {
		if string(r.Value) == "bad" {
			return uncomparableError{"amount", "currency"}
		}
		rec.add(r)
		return nil
	}, WithRetry(1))
	start(t, m)

	dl := readAll(t, cfg, "orders.dlq", 1)[0]
	if string(dl.Value) != "bad" {
		t.Fatalf("dead letter carries %q", dl.Value)
	}
	if got := header(dl, HeaderDLQError); got != "invalid: amount, currency" {
		t.Errorf("error header = %q", got)
	}
	// The partition has to keep going: the goroutine behind it used to die with the process.
	eventually(t, func() bool { return rec.count() == 1 }, "the record after the failed one was handled")
}

// The same type behind xutil.Permanent: it must reach the dead letter rather than be taken for a shutdown,
// which would leave it unmarked and the partition's goroutine gone.
func TestConsume_UncomparablePermanentErrorDeadLetters(t *testing.T) {
	_, cfg := cluster(t, 1, "orders", "orders.dlq")
	produce(t, rawClient(t, cfg), "orders", 0, "bad")

	m := testManager(t, cfg)
	var calls atomic.Int32
	c := register(t, m, "orders", "g1", func(context.Context, *kgo.Record) error {
		calls.Add(1)
		return xutil.Permanent(uncomparableError{"payload"})
	})
	start(t, m)

	dl := readAll(t, cfg, "orders.dlq", 1)[0]
	if calls.Load() != 1 {
		t.Fatalf("permanent error retried: %d calls", calls.Load())
	}
	if got := header(dl, HeaderDLQError); got != "invalid: payload" {
		t.Errorf("error header = %q", got)
	}
	if err := m.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := committed(t, cfg, "g1", "orders", 0); got != 1 {
		t.Fatalf("dead-lettered record not committed: %d, it would be redelivered forever", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.live) != 0 {
		t.Fatalf("%d partition goroutine(s) left behind", len(c.live))
	}
}

// A panic in this module's own consuming code (not the handler's -- call catches that one) must not take the
// process down, and the partition it was on has to leave the table: poll dispatches by that table, and a
// goroutine nobody reads would take workerQueue batches and then block poll for good, stopping every
// partition of this consumer.
//
// The panic is provoked with a nil client, which MarkCommitRecords dereferences. Any crash in this module's
// code between the handler and the mark would do; this one needs no hook into the production path.
func TestConsume_DroppedRecordsKeepSayingSo(t *testing.T) {
	// The goroutine exiting is one ERROR at the moment it happens, and what follows has no sign of
	// its own: poll goes on fetching this partition and dropping every batch, nothing is marked so
	// the committed offset stays put, and nothing here is counted -- the process keeps its
	// heartbeat and the other partitions carry on. It mends itself only on the next rebalance,
	// which may be days away, so it has to be sayable at any later moment and not only at the
	// start. Once per batch would be a flood of its own, hence one per partition per interval.
	logs := captureLogs(t)
	c := &consumer{topic: "orders", group: "g1", o: buildOptions("orders", nil)}
	c.assigned, c.live = map[int32]*worker{}, map[*worker]string{}
	recs := []*kgo.Record{{Topic: "orders", Partition: 7, Offset: 42}}

	for range 3 {
		c.dropped(context.Background(), 7, recs)
	}
	c.dropped(context.Background(), 8, recs) // another partition is another thing to fix

	const msg = "kafka records dropped, this partition has no goroutine of its own until the next rebalance"
	got := withMsg(logs(), msg)
	if len(got) != 2 {
		t.Fatalf("three batches of one partition are one line, and a second partition is its own, got %d: %v", len(got), got)
	}
	if got[0]["level"] != "WARN" || got[0]["partition"] != float64(7) || got[0]["group"] != "g1" ||
		got[0]["records"] != float64(1) || got[0]["from_offset"] != float64(42) {
		t.Errorf("the line should name what is being dropped and where, got=%v", got[0])
	}
	if got[1]["partition"] != float64(8) {
		t.Errorf("the second line should be the other partition, got=%v", got[1])
	}

	// Once the interval has passed it says so again: the state is still there and may be looked at later.
	c.mu.Lock()
	c.lastDropWarn[7] = time.Now().Add(-dropWarnInterval)
	c.mu.Unlock()
	c.dropped(context.Background(), 7, recs)
	if got := withMsg(logs(), msg); len(got) != 3 {
		t.Errorf("after the interval the same partition says so again, got %d", len(got))
	}
}

func TestConsume_PanicInConsumingCodeReleasesThePartition(t *testing.T) {
	logs := captureLogs(t)
	c := &consumer{topic: "orders", group: "g1", o: buildOptions("orders", nil),
		fn: func(context.Context, *kgo.Record) error { return nil }}
	c.base = context.Background()
	c.hard, c.hardCancel = context.WithCancel(c.base)
	c.assigned, c.live = map[int32]*worker{}, map[*worker]string{}
	t.Cleanup(c.hardCancel)

	w := newWorker(c, 7)
	c.assigned[7] = w
	c.live[w] = partitionStr(c.topic, 7)
	go w.run()
	w.recs <- []*kgo.Record{{Topic: "orders", Partition: 7, Offset: 1}}

	select {
	case <-w.done:
	case <-time.After(wait):
		t.Fatal("the partition goroutine did not exit")
	}
	c.mu.Lock()
	left, live := c.assigned[7], len(c.live)
	c.mu.Unlock()
	if left != nil {
		t.Error("the partition is still in the table, so poll would fill its queue and then block for good")
	}
	if live != 0 {
		t.Errorf("%d goroutine(s) still counted as live", live)
	}
	e := withMsg(logs(), "kafka partition worker panicked, this partition stops until the next rebalance")
	if len(e) != 1 || e[0]["level"] != "ERROR" || e[0]["partition"] != float64(7) || e[0]["stack"] == nil {
		t.Fatalf("panic log: %v", e)
	}
}

// observations is how many times the series in the histogram whose labels all match want were recorded.
func observations(h *prometheus.HistogramVec, want map[string]string) uint64 {
	ch := make(chan prometheus.Metric, 100)
	go func() { h.Collect(ch); close(ch) }()
	var n uint64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		match := 0
		for _, l := range pb.GetLabel() {
			if v, ok := want[l.GetName()]; ok && v == l.GetValue() {
				match++
			}
		}
		if match == len(want) {
			n += pb.GetHistogram().GetSampleCount()
		}
	}
	return n
}
