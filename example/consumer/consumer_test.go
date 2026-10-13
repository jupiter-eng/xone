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
	// 框架是在 Start 返回之后才关数据库和缓存的。提前返回的话，
	// 还在处理的消息会摸到已经关掉的连接池
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
	cancel() // 退出信号

	select {
	case <-done:
		if !finished.Load() {
			t.Error("Start 在消息还没处理完的时候就返回了")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start 没返回")
	}
}

func TestConsumer_InFlightMessageCtxNotCanceled(t *testing.T) {
	// 沿用已取消的 ctx 的话，这条消息里每一次写库、每一次调下游、
	// 连最后那次 Ack 都会一进去就被拒绝——消息没做完，队列也没收到确认
	q := newMemQueue(4)
	q.publish("m1", nil)

	entered := make(chan struct{})
	var errAfterCancel error

	c := &Consumer{
		q:        q,
		settings: testSettings(1, time.Second),
		handle: func(ctx context.Context, _ Message) error {
			close(entered)
			time.Sleep(150 * time.Millisecond) // 这中间退出信号到了
			errAfterCancel = ctx.Err()         // 处理逻辑此刻看到的 ctx 状态
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
		t.Errorf("在途消息的 ctx 被取消了（%v），这条消息的写库和 Ack 都会当场失败", errAfterCancel)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.acked) != 1 {
		t.Errorf("在途消息该被 Ack，acked=%v nacked=%v", q.acked, q.nacked)
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
		t.Errorf("毒消息之后 worker 该继续消费，实际只处理了 %d 条", handled.Load())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.nacked) != 1 || q.nacked[0] != "bad" {
		t.Errorf("panic 的那条该被 Nack，nacked=%v", q.nacked)
	}
	if len(q.acked) != 1 || q.acked[0] != "good" {
		t.Errorf("正常的那条该被 Ack，acked=%v", q.acked)
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
		t.Errorf("处理失败的消息该被 Nack，nacked=%v acked=%v", q.nacked, q.acked)
	}
}

// TestRun_ComponentsCloseAfterConsumerStops 是这个目录里唯一走完整框架的测试：
// 上面几条证明 Consumer 自己的行为，这一条证明它和 xone.Run 接得上
func TestRun_ComponentsCloseAfterConsumerStops(t *testing.T) {
	q := newMemQueue(4)
	q.publish("m1", nil)

	var mu sync.Mutex
	var closedAt, drainedAt time.Time

	// 业务自己的组件就是这样接进框架的：两个钩子，里面写普通的 Go 代码
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

	// 和 main 一样先读业务配置：这个目录下的 application.yml 里写着 MyApp，
	// 没人读的话启动会因为「没人认领」而失败
	if err := conf.Load(); err != nil {
		t.Fatal(err)
	}
	if err := xone.Run(c, xone.WithLogger(quiet())); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if drainedAt.IsZero() || closedAt.IsZero() {
		t.Fatal("两边都该跑到")
	}
	if closedAt.Before(drainedAt) {
		t.Errorf("组件在消息还没处理完的时候就被关了：关闭=%v 消息处理完=%v", closedAt, drainedAt)
	}
}

func TestQueue_CtxCanceledStopsFetching(t *testing.T) {
	// select 在几路同时就绪时随机挑：只靠 select 的话，ctx 取消了、
	// 队列里又有消息，一半的概率照样取走一条
	q := newMemQueue(64)
	for i := range 64 {
		q.publish(strconv.Itoa(i), nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for range 64 {
		if m, ok := q.Next(ctx); ok {
			t.Fatalf("ctx 已经取消，不该再取到消息，got=%s", m.ID)
		}
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestQueue_NoPanicWhenProducerStillSendingOnExit(t *testing.T) {
	// 回归用例。原来 Close 是 close(ch)，而这个例子里 feed 一直在往里发；
	// 关的那一刻正好有人在投递就会 panic: send on closed channel。
	// 只在那个窗口里复现，所以跑十次八次都可能是好的——实际是一跑真就炸
	q := newMemQueue(4)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 4 { // 四个生产者一起投，把窗口撑开
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

	if err := c.Start(ctx); err != nil { // Start 收工时会 Close 队列
		t.Fatal(err)
	}
	// 关掉之后再让生产者投一会儿，确认投到已关闭的队列上也不炸
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()

	if !q.closed.Load() {
		t.Error("Start 收工时该把队列关掉")
	}
}
