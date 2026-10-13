package xkafka

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Option Consume 的可选设置，用法见各个 With*
type Option func(*options)

// dlqSuffix 默认死信 topic 是原 topic 加上这个后缀
const dlqSuffix = ".dlq"

const (
	// defaultTimeout 每一次调用处理函数的默认超时
	defaultTimeout = 30 * time.Second

	// defaultRetries 处理函数失败后默认再试几次（不算第一次）
	defaultRetries = 3
)

type options struct {
	client  string
	timeout time.Duration
	retries int
	dlq     string
}

// WithClient 从哪个集群消费，名字是 XKafka.Clients 下的那个。默认 default。
//
// 每个 Consume 用这个集群的配置另建一个自己的客户端（带着消费组），不和 C() 返回的那个共用。
func WithClient(name string) Option {
	return func(o *options) { o.client = name }
}

// WithTimeout 每一次调用处理函数的超时：到点取消 fn 收到的 ctx。默认 30s，必须大于 0。
//
// 只是取消 ctx：fn 不看 ctx 的话照样跑完，Go 没有从外面停下一个协程的办法。
// 每次重试各算各的。再均衡时要等在途的那一次跑完（见 README「再均衡」），
// 所以别配得比 franz-go 的再均衡超时（60s，v1.21.7 默认）还长。
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithRetry 处理函数返回错误（或 panic）之后再试几次，不算第一次。默认 3，即最多调 4 次；配 0 不重试。
//
// 两次之间的等待从 1s 起逐次翻倍、带抖动（同 xutil.Retry：实际等待是 [0, 上界] 里的随机值），
// 上界依次是 1s、2s、4s……最多 30s。返回 xutil.Permanent(err) 的不再重试，直接进死信。
// 退出或者分区被收回时不再发起下一次，这条消息不提交，之后重新投递。
func WithRetry(n int) Option {
	return func(o *options) { o.retries = n }
}

// WithDeadLetter 重试用完之后把消息写进哪个 topic（死信）。默认是原 topic 加 .dlq（orders → orders.dlq）。
//
// 传 "" 关掉死信：重试用完的消息记一条 ERROR（kafka message skipped）就跳过，提交 offset，不再处理。
// 死信写不进去时一直重试、不提交，见 README「死信」。
func WithDeadLetter(topic string) Option {
	return func(o *options) { o.dlq = topic }
}

// buildOptions 套上默认值和全部 Option
func buildOptions(topic string, opts []Option) options {
	o := options{client: DefaultName, timeout: defaultTimeout, retries: defaultRetries, dlq: topic + dlqSuffix}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// validate Consume 收到的设置是否成立。返回普通 error，由 Consume 在边界上包成 xerror
func (o options) validate(topic string) error {
	var errs []error
	if strings.TrimSpace(o.client) == "" {
		errs = append(errs, errors.New("client name is empty"))
	}
	if o.timeout <= 0 {
		errs = append(errs, fmt.Errorf("timeout must be > 0, got %v", o.timeout))
	}
	if o.retries < 0 {
		errs = append(errs, fmt.Errorf("retry count must be >= 0, got %d", o.retries))
	}
	if o.dlq != "" && strings.TrimSpace(o.dlq) == "" {
		errs = append(errs, errors.New("dead letter topic is blank, pass \"\" to disable dead lettering"))
	}
	// 死信写回自己消费的 topic：处理不了的消息转一圈又回来，永远处理不完
	if o.dlq == topic {
		errs = append(errs, fmt.Errorf("dead letter topic must differ from the consumed topic %q", topic))
	}
	return errors.Join(errs...)
}
