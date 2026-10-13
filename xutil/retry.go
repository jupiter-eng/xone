package xutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// maxBackoff 两次尝试之间最多等这么久。
//
// 做成常量而不是第六个参数：几乎没有调用方需要改它，而多一个参数是每个
// 调用方都要付的理解成本。30s 比任何合理的建连等待都长，又比任何合理的
// 启动预算都短，所以它只在「翻倍翻到离谱」时才起作用。
const maxBackoff = 30 * time.Second

// Retry 反复调用 fn 直到成功，每次单独限时。
//
// 两次之间的等待从 interval 开始、每次翻倍，上限 maxBackoff，并且带抖动：
// 实际等待是 [0, 当前退避] 之间的一个随机值。
//
// 翻倍是为了不把一个正在恢复的下游一直按同一个节奏敲；抖动是为了一群副本
// 同时重启时不要在同一个瞬间一起敲过去——没有抖动的话，退避只是把
// 惊群从一个时刻挪到了另一个时刻。
//
// 整轮有一个总预算（attempts × timeout + 各次退避的上界之和），到了就不再重试。
// 预算按退避的上界算而不是按抖动后的实际值：预算是个天花板，
// 它得在最坏情况下也成立。
//
// parent 被取消时整轮立即中止，剩下的尝试和等待都不再进行。
// 启动期的建连重试靠这一条：收到退出信号时进程不必等满整轮才肯退出。
// 传 nil 等同于 context.Background()。
//
// fn 返回 Permanent(err) 时不再重试，立即返回 err 本身（去掉 Permanent 那一层）：
// 密码错、库不存在这类错误，重试多少次都是同一个结果，只会白白拖长启动。
//
// 返回最后一次的错误；一次都没成功且预算先耗尽时，返回的是耗尽前那次的错误。
// parent 在两次尝试之间被取消时，返回的错误同时满足 errors.Is(err, parent.Err())
// 和 errors.Is(err, 最后一次的错误)：取消是这一轮停下来的原因，前面那些失败是背景。
func Retry(parent context.Context, attempts int, timeout, interval time.Duration, fn func(context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	if parent == nil {
		parent = context.Background()
	}
	// 进来时就已经取消的话，一次都不用试。少了这一句，
	// 启动到一半收到退出信号时，每个连不上的实例还要再发一次注定失败的网络请求
	if err := parent.Err(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(parent, retryBudget(attempts, timeout, interval))
	defer cancel()

	var last error
	backoff := min(interval, maxBackoff) // 第一次也封顶：interval 配得比上限还大时，文档说的「最多等 maxBackoff」照样成立
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				// 两种原因走到这里，要分开报：调用方叫停了，就得如实说是取消——
				// 只报上一次的「连不上」，启动路径会把按要求退出当成故障；
				// 只是整轮预算耗尽，那就没有取消可言，报最后一次的错误
				if err := parent.Err(); err != nil {
					return fmt.Errorf("%w, last attempt failed: %w", err, last)
				}
				return last
			case <-time.After(jitter(backoff)):
			}
			backoff = nextBackoff(backoff)
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, timeout)
		last = fn(attemptCtx)
		attemptCancel()
		if last == nil {
			return nil
		}
		if p := (*permanentError)(nil); errors.As(last, &p) {
			return p.err
		}
	}
	return last
}

// Permanent 把 err 标记为「重试也没用」：Retry 遇到它立即返回 err，不再尝试。
// 标记只给 Retry 看，Retry 返回的是去掉标记之后的 err；err 为 nil 时返回 nil。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

// permanentError Permanent 的标记。Error 和 Unwrap 都透传，
// 标记本身不改变错误的文本，也不挡 errors.Is / errors.As
type permanentError struct{ err error }

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

// retryBudget 整轮的上限：每次尝试的超时，加上各次退避的上界
func retryBudget(attempts int, timeout, interval time.Duration) time.Duration {
	budget := timeout * time.Duration(attempts)
	backoff := min(interval, maxBackoff) // 与 Retry 一致：第一次也封顶
	for i := 1; i < attempts; i++ {
		budget += backoff
		backoff = nextBackoff(backoff)
	}
	return budget
}

// nextBackoff 翻倍，封顶 maxBackoff
func nextBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d >= maxBackoff/2 {
		return maxBackoff
	}
	return d * 2
}

// jitter 在 [0, d] 之间取一个随机值。
//
// 取「0 到 d」而不是「d 的正负一点」：前者才真的把一群同时重启的副本摊开，
// 后者只是让它们在同一个时刻附近抖一下。
//
// 是变量而不是函数，只为让测试能换成恒等映射，好把「退避确实在翻倍」
// 这件事测准——随机数一掺进来，等待时长就只能做统计断言了
var jitter = func(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}
