package xcron

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// Option 定时任务的可选设置，用法见各个 With* / RunOnStart*。
type Option func(*options)

// firstRun 调度器起来时要不要先跑一次
type firstRun int

const (
	firstNone  firstRun = iota // 等第一个时间点
	firstAsync                 // RunOnStart：立刻在后台跑一次，不等它
	firstWait                  // RunOnStartAndWait：立刻跑一次，启动等它跑完，失败启动就失败
)

type options struct {
	name    string
	named   bool // 显式写了 WithName，空串要报错，不能当成「没写」
	timeout time.Duration
	overlap bool
	loc     *time.Location
	first   firstRun

	// firsts 写了几个 RunOnStart / RunOnStartAndWait：两个都写是自相矛盾，报错而不是谁后写听谁的
	firsts int
}

// WithName 任务名：日志的 job 字段、Span 名（cron <name>）、报错里都用它，同一个进程里不能重复。
//
// 不写就取函数名，去掉 import path 前缀和方法值的 -fm 后缀（Go 1.25 实测）：
//
//	普通函数 cleanup            → main.cleanup
//	方法值 svc.Sync（值接收者）  → main.Service.Sync
//	方法值 svc.Sync（指针接收者）→ main.(*Service).Sync
//	闭包                        → main.main.func1
//
// 闭包的名字是编译器按出现顺序编的号，挪一下代码就变，日志和告警里也看不出是哪个任务——
// 传闭包时写上 WithName。
func WithName(name string) Option {
	return func(o *options) { o.name, o.named = name, true }
}

// WithTimeout 每一次执行的超时：到点取消 fn 收到的 ctx。默认不限时。
//
// 只是取消 ctx：fn 不看 ctx 的话照样跑完，Go 没有从外面停下一个协程的办法。
// 也管 RunOnStart / RunOnStartAndWait 的那第一次。
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// AllowOverlap 允许上一次还没跑完时开始下一次，两次并发执行。
//
// 默认不允许：到了时间点而上一次还在跑，这一次跳过并记一条 WARN（cron job skipped），
// 下一个时间点照常。一个比间隔还慢的任务因此不会越积越多。
func AllowOverlap() Option {
	return func(o *options) { o.overlap = true }
}

// WithLocation 按哪个时区解释 spec。默认 UTC。
//
//	xcron.Add("30 2 * * *", backup, xcron.WithLocation(berlin)) // 柏林时间每天 02:30
//
// 有夏令时的时区里，拨快那天不存在的时刻整天跳过，拨回那天重复的时刻跑两次，
// 见 README「行为与实测」。spec 里不收 CRON_TZ= / TZ= 前缀，时区只在这里写。
func WithLocation(loc *time.Location) Option {
	return func(o *options) { o.loc = loc }
}

// RunOnStart 调度器起来时先在后台跑一次，不等它，之后按 spec 照常。
// 这一次失败只记日志，不影响启动。
func RunOnStart() Option {
	return func(o *options) {
		o.first = firstAsync
		o.firsts++
	}
}

// RunOnStartAndWait 调度器起来时先跑一次，启动等它跑完；它失败，启动就失败。
//
// 适合「服务起来之前数据必须先就绪」的任务：先拉一次配置、先刷一次本地缓存。
// 几个这样的任务并行跑，全部跑完启动才继续；收到退出信号时它们的 ctx 被取消。
// 启动之后再 Add 的，Add 等这一次跑完，返回它的错误。
func RunOnStartAndWait() Option {
	return func(o *options) {
		o.first = firstWait
		o.firsts++
	}
}

// buildOptions 套上全部 Option，没写名字的取函数名
func buildOptions(fn any, opts []Option) options {
	o := options{loc: time.UTC}
	for _, opt := range opts {
		opt(&o)
	}
	if !o.named {
		o.name = funcName(fn)
	}
	return o
}

// validate Add 收到的设置是否成立。返回普通 error，由 Add 在边界上包成 xerror
func (o options) validate() error {
	var errs []error
	if strings.TrimSpace(o.name) == "" {
		errs = append(errs, errors.New("name is empty"))
	}
	if o.timeout < 0 {
		errs = append(errs, fmt.Errorf("timeout must be >= 0, got %v", o.timeout))
	}
	if o.loc == nil {
		errs = append(errs, errors.New("location is nil"))
	}
	if o.firsts > 1 {
		errs = append(errs, errors.New("RunOnStart and RunOnStartAndWait are mutually exclusive"))
	}
	return errors.Join(errs...)
}

// funcName 函数名去掉 import path 前缀和方法值的 -fm 后缀，见 WithName
func funcName(fn any) string {
	f := runtime.FuncForPC(reflect.ValueOf(fn).Pointer())
	if f == nil {
		return ""
	}
	name := strings.TrimSuffix(f.Name(), "-fm")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}
