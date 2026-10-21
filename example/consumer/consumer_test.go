package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/example/consumer/conf"
	"github.com/jupiter-eng/xone/xhook"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testSettings(workers int, timeout time.Duration) Settings {
	return Settings{Workers: workers, Timeout: timeout}
}

func TestConsumer_StartReturnsAfterInFlightMessagesFinish(t *testing.T) {
	// The framework closes the database and the cache only after Start has returned. Returning early,
	// a message still being handled would reach a connection pool that is already closed
	q := newMemQueue(4)
	q.publish("m1", nil)

	entered := make(chan struct{})
	var finished atomic.Bool

	c := &Consumer{
		q:        q,
		settings: testSettings(1, time.Second),
		handle: func(context.Context, Message) error {
			close(entered)
			time.Sleep(200 * time.Millisecond)
			finished.Store(true)
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	<-entered
	cancel() // the shutdown signal

	select {
	case <-done:
		if !finished.Load() {
			t.Error("Start returned while a message was still being handled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start never returned")
	}
}

func TestConsumer_InFlightMessageCtxNotCanceled(t *testing.T) {
	// Reusing an already cancelled ctx, every write to the database, every call downstream and even
	// the final Ack in this message would be refused the moment it begins -- the message unfinished
	// and the queue never acknowledged
	q := newMemQueue(4)
	q.publish("m1", nil)

	entered := make(chan struct{})
	var errAfterCancel error

	c := &Consumer{
		q:        q,
		settings: testSettings(1, time.Second),
		handle: func(ctx context.Context, _ Message) error {
			close(entered)
			time.Sleep(150 * time.Millisecond) // the shutdown signal arrives in the meantime
			errAfterCancel = ctx.Err()         // the ctx state the handler sees at this moment
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	<-entered
	cancel()
	<-done

	if errAfterCancel != nil {
		t.Errorf("the ctx of a message in flight was cancelled (%v), so this message's writes and its Ack would both fail on the spot", errAfterCancel)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.acked) != 1 {
		t.Errorf("a message in flight must be Acked, acked=%v nacked=%v", q.acked, q.nacked)
	}
}

func TestConsumer_PoisonMessageDoesNotKillWorker(t *testing.T) {
	q := newMemQueue(8)
	q.publish("bad", nil)
	q.publish("good", nil)

	var handled atomic.Int32
	c := &Consumer{
		q:        q,
		settings: testSettings(1, time.Second),
		handle: func(_ context.Context, m Message) error {
			handled.Add(1)
			if m.ID == "bad" {
				panic("deliberate panic in a message handler")
			}
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	slog.SetDefault(quiet())
	_ = c.Start(ctx)

	if handled.Load() != 2 {
		t.Errorf("the worker must keep consuming after a poison message, it handled only %d", handled.Load())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.nacked) != 1 || q.nacked[0] != "bad" {
		t.Errorf("the one that panicked must be Nacked, nacked=%v", q.nacked)
	}
	if len(q.acked) != 1 || q.acked[0] != "good" {
		t.Errorf("the ordinary one must be Acked, acked=%v", q.acked)
	}
}

func TestConsumer_FailedMessageIsNacked(t *testing.T) {
	q := newMemQueue(4)
	q.publish("m1", nil)

	c := &Consumer{q: q, settings: testSettings(1, time.Second),
		handle: func(context.Context, Message) error { return errors.New("downstream is down") }}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	slog.SetDefault(quiet())
	_ = c.Start(ctx)

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.nacked) != 1 {
		t.Errorf("a message that failed to be handled must be Nacked, nacked=%v acked=%v", q.nacked, q.acked)
	}
}

// TestRun_ComponentsCloseAfterConsumerStops is the only test in this directory that goes through the
// whole framework: the ones above prove the Consumer's own behavior, and this one proves it joins up
// with xone.Run
func TestRun_ComponentsCloseAfterConsumerStops(t *testing.T) {
	q := newMemQueue(4)
	q.publish("m1", nil)

	var mu sync.Mutex
	var closedAt, drainedAt time.Time

	// This is how a component of the business's own joins the framework: two hooks with ordinary Go
	// code inside
	xhook.BeforeStart(func(context.Context) error { return nil })
	xhook.BeforeStop(func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		closedAt = time.Now()
		return nil
	})

	var once sync.Once
	entered := make(chan struct{})
	c := &Consumer{
		q:        q,
		settings: testSettings(1, time.Second),
		handle: func(context.Context, Message) error {
			once.Do(func() { close(entered) })
			time.Sleep(150 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			drainedAt = time.Now()
			return nil
		},
	}

	go func() {
		<-entered
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(os.Interrupt)
	}()

	// The business configuration is read first, as in main: the application.yml in this directory has
	// a MyApp, and with nobody reading it startup would fail as "unclaimed"
	if err := conf.Load(); err != nil {
		t.Fatal(err)
	}
	if err := xone.Run(c, xone.WithLogger(quiet())); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if drainedAt.IsZero() || closedAt.IsZero() {
		t.Fatal("both sides must have been reached")
	}
	if closedAt.Before(drainedAt) {
		t.Errorf("the component was closed while a message was still being handled: closed=%v drained=%v", closedAt, drainedAt)
	}
}

func TestQueue_CtxCanceledStopsFetching(t *testing.T) {
	// select picks at random among the cases that are ready at the same time: on select alone, with
	// the ctx cancelled and messages still in the queue, there is a one in two chance of taking one
	// anyway
	q := newMemQueue(64)
	for i := range 64 {
		q.publish(strconv.Itoa(i), nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for range 64 {
		if m, ok := q.Next(ctx); ok {
			t.Fatalf("the ctx is cancelled and no message may be taken any more, got=%s", m.ID)
		}
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestQueue_NoPanicWhenProducerStillSendingOnExit(t *testing.T) {
	// A regression case. Close used to be close(ch) while feed in this example publishes all along;
	// somebody publishing at exactly the moment it closes panics with send on closed channel.
	// It only reproduces inside that window, so ten runs may all look fine -- and in production it
	// blows up on the first run
	q := newMemQueue(4)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 4 { // four producers publishing together, to widen the window
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				q.publish(strconv.Itoa(n*1000+j), nil)
			}
		}(i)
	}

	c := &Consumer{q: q, settings: testSettings(2, time.Second),
		handle: func(context.Context, Message) error { return nil }}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	slog.SetDefault(quiet())

	if err := c.Start(ctx); err != nil { // Start Closes the queue when it finishes
		t.Fatal(err)
	}
	// The producers keep publishing for a while after the close, to confirm that publishing to a
	// closed queue does not blow up either
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()

	if !q.closed.Load() {
		t.Error("Start must close the queue when it finishes")
	}
}
