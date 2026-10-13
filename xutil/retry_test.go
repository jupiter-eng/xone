package xutil

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestRetry_NoRetryOnFirstSuccess(t *testing.T) {
	n := 0
	err := Retry(context.Background(), 3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		return nil
	})
	if err != nil || n != 1 {
		t.Errorf("首次成功就该返回，n=%d err=%v", n, err)
	}
}

func TestRetry_RetriesAfterFailure(t *testing.T) {
	n := 0
	err := Retry(context.Background(), 3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		if n < 3 {
			return errors.New("还不行")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Errorf("应重试到成功，n=%d err=%v", n, err)
	}
}

func TestRetry_ReturnsLastErrorWhenExhausted(t *testing.T) {
	last := errors.New("第三次也不行")
	n := 0
	err := Retry(context.Background(), 3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		if n == 3 {
			return last
		}
		return errors.New("不行")
	})
	if !errors.Is(err, last) {
		t.Errorf("应返回最后一次的错误，got=%v", err)
	}
	if n != 3 {
		t.Errorf("应当尝试 3 次，got=%d", n)
	}
}

func TestRetry_PerAttemptTimeout(t *testing.T) {
	// 一次卡住不该把整轮预算吃光。
	// 退避换成 0、interval 给大：整轮预算里留给退避的那一段（1s+2s）真的不花，
	// 成了每次尝试的调度余量。否则预算只比 3×timeout 多 3ms，机器一忙，
	// 前两次各晚醒几毫秒就把第三次挤掉——那是调度的问题，不是这条承诺的问题
	withJitter(t, func(time.Duration) time.Duration { return 0 })
	const timeout = 20 * time.Millisecond
	var deadlines int
	err := Retry(context.Background(), 3, timeout, time.Second, func(ctx context.Context) error {
		if d, ok := ctx.Deadline(); !ok || time.Until(d) > timeout {
			t.Errorf("每次尝试的截止时间该是自己的 timeout，而不是整轮预算：还剩 %v", time.Until(d))
		}
		<-ctx.Done()
		deadlines++
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("每次都超时应当返回错误")
	}
	if deadlines != 3 {
		t.Errorf("每次都该有自己的 deadline，got=%d 次", deadlines)
	}
}

func TestRetry_TotalBudgetCapsWholeRun(t *testing.T) {
	// 带总预算是为了让启动期收到的退出信号能及时生效：
	// 不可中断的重试会让进程必须等满整轮才肯退出。
	// 退避换成一小时：没有总预算兜着，这一轮要等四个小时；有的话约 90ms
	// （5×10ms + 4×10ms）。上界给 10s，离两头都远，慢机器上也不会误报
	withJitter(t, func(time.Duration) time.Duration { return time.Hour })
	start := time.Now()
	Retry(context.Background(), 5, 10*time.Millisecond, 10*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("整轮应在总预算内结束，用了 %v", elapsed)
	}
}

func TestRetry_RunsOnceWhenAttemptsBelowOne(t *testing.T) {
	n := 0
	Retry(context.Background(), 0, time.Second, time.Millisecond, func(context.Context) error { n++; return nil })
	if n != 1 {
		t.Errorf("至少该跑一次，got=%d", n)
	}
}

func TestRetry_AbortsImmediatelyOnParentCancel(t *testing.T) {
	// 启动期的建连重试靠这一条：收到退出信号时，进程不该被迫等满整轮。
	// 三次尝试 × 每次 5s，不中断就是 10 秒起步，而信号已经来了
	// 退避换成一小时：取消之后要是还在等退避，这里就挂一小时；上界 10s 离两头都远
	withJitter(t, func(time.Duration) time.Duration { return time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()

	err := Retry(ctx, 3, 5*time.Second, 5*time.Second, func(context.Context) error {
		calls++
		cancel() // 第一次尝试进行中收到退出信号
		return errors.New("连不上")
	})

	if err == nil {
		t.Fatal("应当返回最后一次的错误")
	}
	if calls != 1 {
		t.Errorf("取消之后不该再尝试，got=%d 次", calls)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("取消之后不该还在等重试间隔，耗时=%v", elapsed)
	}
}

func TestRetry_ReportsCancelBetweenAttempts(t *testing.T) {
	// 取消发生在退避期间时，报出去的若只是上一次的业务错误，调用方看到的是
	// 「连不上」，而真实原因是「收到退出信号不再试了」——启动路径据此判断
	// 这是故障还是按要求退出。上一次的错误也不能丢：它是之前一直失败的原因
	withJitter(t, func(time.Duration) time.Duration { return time.Hour }) // 必然还在等退避

	ctx, cancel := context.WithCancel(context.Background())
	last := errors.New("连不上")
	err := Retry(ctx, 3, time.Second, time.Hour, func(context.Context) error {
		time.AfterFunc(10*time.Millisecond, cancel) // 在等退避的时候取消
		return last
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("该说清是被取消了，got=%v", err)
	}
	if !errors.Is(err, last) {
		t.Errorf("上一次的错误也该留着，got=%v", err)
	}
}

func TestRetry_ReturnsLastErrorWhenBudgetExhausted(t *testing.T) {
	// 预算耗尽不是取消：调用方没有叫停，只是一直没连上。这时照文档返回
	// 最后一次的业务错误，不能混进一个 context 的错误让人以为被取消了
	// 把退避换成远超预算的等待，于是预算必然在两次尝试之间耗尽
	withJitter(t, func(time.Duration) time.Duration { return time.Hour })

	last := errors.New("连不上")
	err := Retry(context.Background(), 3, 10*time.Millisecond, time.Millisecond, func(context.Context) error {
		return last
	})
	if !errors.Is(err, last) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("预算耗尽时该只报最后一次的错误，got=%v", err)
	}
}

func TestRetry_NilParentCtxActsAsBackground(t *testing.T) {
	n := 0
	//nolint:staticcheck // 显式验证 nil 的兼容行为
	if err := Retry(nil, 2, time.Second, time.Millisecond, func(context.Context) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if n != 1 {
		t.Errorf("成功就不该重试，got=%d", n)
	}
}

func TestRetry_NoAttemptWhenParentAlreadyCanceled(t *testing.T) {
	// 启动到一半收到退出信号时，每个连不上的实例不该再发一次注定失败的网络请求
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := Retry(ctx, 3, time.Second, time.Second, func(context.Context) error {
		calls++
		return errors.New("连不上")
	})

	if calls != 0 {
		t.Errorf("一次都不该试，got=%d 次", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("该如实说是被取消了，而不是报最后一次的网络错误，got=%v", err)
	}
}

func TestNextBackoff_DoublesAndCaps(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, 0},
		{time.Second, 2 * time.Second},
		{10 * time.Second, 20 * time.Second},
		{maxBackoff / 2, maxBackoff},
		{maxBackoff, maxBackoff}, // 封顶之后不再涨
		{2 * maxBackoff, maxBackoff},
	} {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestJitter_BetweenZeroAndUpperBound(t *testing.T) {
	// 取「0 到 d」而不是「d 附近抖一下」：前者才真的把一群同时重启的副本摊开
	const d = 100 * time.Millisecond
	var sawSmall, sawLarge bool
	for range 2000 {
		got := jitter(d)
		if got < 0 || got > d {
			t.Fatalf("jitter 越界：%v 不在 [0,%v]", got, d)
		}
		if got < d/4 {
			sawSmall = true
		}
		if got > d*3/4 {
			sawLarge = true
		}
	}
	if !sawSmall || !sawLarge {
		t.Error("两千次取样该覆盖到区间两头，看起来没有真的在抖")
	}
	if jitter(0) != 0 || jitter(-time.Second) != 0 {
		t.Error("非正的退避不该等待")
	}
}

func TestRetryBudget_SumsBackoffUpperBounds(t *testing.T) {
	// 预算是天花板，所以按退避的上界算，不按抖动后的实际值
	const timeout, interval = 10 * time.Millisecond, 10 * time.Millisecond

	// 3 次尝试：等待上界是 10 + 20 = 30ms
	if got, want := retryBudget(3, timeout, interval), 3*timeout+30*time.Millisecond; got != want {
		t.Errorf("3 次尝试的预算 got=%v want=%v", got, want)
	}
	// 固定间隔的话只有 20ms，退避之后更宽——预算必须跟着涨，
	// 否则最后一次尝试会被自己的预算掐掉
	if retryBudget(3, timeout, interval) <= 3*timeout+2*interval {
		t.Error("退避之后预算该比固定间隔时更宽")
	}
	// 一次尝试没有等待
	if got, want := retryBudget(1, timeout, interval), timeout; got != want {
		t.Errorf("单次尝试的预算 got=%v want=%v", got, want)
	}
}

func TestRetry_EveryAttemptRunsAfterBackoff(t *testing.T) {
	// 回归用例。预算若还按固定间隔算，退避把等待撑长之后，
	// 最后一次尝试会被自己的预算掐掉，表现是「配了 5 次只试了 4 次」
	const attempts = 5
	calls := 0
	err := Retry(context.Background(), attempts, 5*time.Millisecond, time.Millisecond,
		func(context.Context) error { calls++; return errors.New("nope") })
	if err == nil {
		t.Fatal("该失败")
	}
	if calls != attempts {
		t.Errorf("该跑满 %d 次，实际 %d 次", attempts, calls)
	}
}

// recordWaits 把抖动换成「记下请求的退避、不真的等」，返回记下的那些。
//
// 断言的是 Retry 请求等多久，而不是墙钟上等了多久：后者在负载高的机器上
// 抖得厉害，翻倍这种比例断言迟早会误报
func recordWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	withJitter(t, func(d time.Duration) time.Duration { waits = append(waits, d); return 0 })
	return &waits
}

func TestRetry_WaitDoublesBetweenAttempts(t *testing.T) {
	// 这是退避的全部意义：固定间隔会一直按同一个节奏敲一个正在恢复的下游。
	// 抖动本身由 TestJitter_BetweenZeroAndUpperBound 单独盯着
	waits := recordWaits(t)

	const interval = 20 * time.Millisecond
	_ = Retry(context.Background(), 4, time.Millisecond, interval,
		func(context.Context) error { return errors.New("nope") })

	want := []time.Duration{interval, 2 * interval, 4 * interval}
	if !slices.Equal(*waits, want) {
		t.Errorf("4 次尝试之间的退避该是 %v，got %v", want, *waits)
	}
}

func TestRetry_FirstWaitAlsoCapped(t *testing.T) {
	// 文档说两次之间最多等 maxBackoff。interval 配得比它还大时，
	// 从前第一次等待原样用 interval，配 1h 就真的等 1h
	waits := recordWaits(t)

	_ = Retry(context.Background(), 3, time.Millisecond, time.Hour,
		func(context.Context) error { return errors.New("nope") })

	if len(*waits) != 2 {
		t.Fatalf("3 次尝试之间该等 2 次，got %v", *waits)
	}
	for _, w := range *waits {
		if w > maxBackoff {
			t.Errorf("每次等待的上界都不该超过 %v，got %v", maxBackoff, *waits)
		}
	}
}

func TestRetryBudget_FirstBackoffAlsoCapped(t *testing.T) {
	if got, want := retryBudget(3, time.Millisecond, time.Hour), 3*time.Millisecond+2*maxBackoff; got != want {
		t.Errorf("预算按封顶后的退避算，got=%v want=%v", got, want)
	}
}

func TestRetry_NoRetryOnPermanentError(t *testing.T) {
	// 密码错、库不存在：重试多少次都一样，只会白白拖长启动
	root := errors.New("auth failed")
	calls := 0
	err := Retry(context.Background(), 5, time.Second, time.Hour, func(context.Context) error {
		calls++
		return Permanent(root)
	})

	if calls != 1 {
		t.Errorf("永久错误之后不该再试，got=%d 次", calls)
	}
	if err != root {
		t.Errorf("该返回去掉标记之后的原错误，got=%#v", err)
	}
}

func TestRetry_RecognizesWrappedPermanentError(t *testing.T) {
	root := errors.New("auth failed")
	calls := 0
	err := Retry(context.Background(), 5, time.Second, time.Hour, func(context.Context) error {
		calls++
		return fmt.Errorf("connect: %w", Permanent(root))
	})
	if calls != 1 || err != root {
		t.Errorf("该只试 1 次并返回原错误，got 次数=%d err=%#v", calls, err)
	}
}

func TestPermanent_PreservesMessageAndChain(t *testing.T) {
	root := errors.New("auth failed")
	p := Permanent(root)
	if p.Error() != root.Error() {
		t.Errorf("标记不该改变错误文本，got=%q", p.Error())
	}
	if !errors.Is(p, root) {
		t.Error("标记不该挡住 errors.Is")
	}
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) 该是 nil，否则 fn 成功了也会被当成失败")
	}
}

// withJitter 在这个测试里把退避的抖动换成 f，结束时还原。
// 时长相关的断言靠它把等待从随机数和调度里拿出来
func withJitter(t *testing.T, f func(time.Duration) time.Duration) {
	t.Helper()
	old := jitter
	jitter = f
	t.Cleanup(func() { jitter = old })
}
