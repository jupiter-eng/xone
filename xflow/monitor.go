package xflow

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// StepEvent 一步执行完的事件，Process 和 Rollback 共用
type StepEvent struct {
	Flow       string
	Processor  string
	Dependency Dependency
	Rollback   bool // true 表示这是一次回滚
	Err        error
	Duration   time.Duration
}

// FlowEvent 整个流程执行完的事件，Duration 含回滚耗时
type FlowEvent struct {
	Flow     string
	Result   *Result
	Duration time.Duration
}

// Monitor 观测流程执行。
//
// 两个回调都被 panic 隔离：监控实现出错只丢一次观测，不会打断业务流程。
type Monitor interface {
	OnStep(ctx context.Context, e *StepEvent)
	OnFlow(ctx context.Context, e *FlowEvent)
}

// monitor 当前生效的实现，原子读写：Execute 是热点路径，不该为它加锁
var monitor atomic.Pointer[Monitor]

// SetMonitor 换掉默认的监控实现。传 nil 表示关掉监控。
func SetMonitor(m Monitor) {
	if m == nil {
		monitor.Store(nil)
		return
	}
	monitor.Store(&m)
}

func init() { SetMonitor(slogMonitor{}) }

// activeMonitor 本次执行用哪个 Monitor，关掉时返回 nil（零开销）
func activeMonitor() Monitor {
	if !cfg.Monitor {
		return nil
	}
	m := monitor.Load()
	if m == nil {
		return nil
	}
	return *m
}

// now 开着监控才读时钟：耗时只有监控看，关掉之后每步照读的话，
// 五步的流程一次执行要白读六次时钟，「零开销」就不成立了
func now(m Monitor) time.Time {
	if m == nil {
		return time.Time{}
	}
	return time.Now()
}

func (f *Flow[T]) notifyStep(ctx context.Context, m Monitor, rollback bool, s entry[T], err error, start time.Time) {
	if m == nil {
		return
	}
	defer recoverNotify()
	m.OnStep(ctx, &StepEvent{
		Flow: f.name, Processor: s.name, Dependency: s.dep,
		Rollback: rollback, Err: err, Duration: time.Since(start),
	})
}

func (f *Flow[T]) notifyFlow(ctx context.Context, m Monitor, res *Result, start time.Time) {
	if m == nil {
		return
	}
	defer recoverNotify()
	m.OnFlow(ctx, &FlowEvent{Flow: f.name, Result: res, Duration: time.Since(start)})
}

// recoverNotify 隔离监控实现的 panic
//
// 直接 defer 它，而不是 defer 一个调用 m.OnX 的闭包：闭包要捕获 m、ctx
// 和事件，于是每一步都多一次分配，而这是每步都走的路径。
func recoverNotify() {
	if r := recover(); r != nil {
		slog.Error("xflow monitor panicked, isolated", "error", r)
	}
}

// slogMonitor 默认实现，写到标准库 slog。
//
// 成功的步骤记 info：开着 XFlow.Monitor 就是想看每一步，默认级别下就该看得到。
// 一个五步的流程每次执行会写六行；不想要逐步日志的，关掉 Monitor 或者用 SetMonitor 换成自己的实现。
type slogMonitor struct{}

func (slogMonitor) OnStep(ctx context.Context, e *StepEvent) {
	// 成功的步骤记 info。级别没开（比如日志级别设成 warn）就在这里返回，不要先把这一行拼出来
	// 再交给 slog 丢掉——一个五步的流程每次执行要拼五次，全是白干
	if e.Err == nil && !slog.Default().Enabled(ctx, slog.LevelInfo) {
		return
	}

	action := "process"
	if e.Rollback {
		action = "rollback"
	}
	attrs := []any{"flow", e.Flow, "step", e.Processor, "dependency", e.Dependency.String(), "elapsed_ms", millis(e.Duration)}

	if e.Err != nil {
		attrs = append(attrs, "error", e.Err)
		// panic 的调用栈不在错误消息里（见 PanicError），在这里单独记一个字段
		var pe *PanicError
		if errors.As(e.Err, &pe) {
			attrs = append(attrs, "stack", string(pe.Stack))
		}
		slog.WarnContext(ctx, "xflow step "+action+" failed", attrs...)
		return
	}
	slog.InfoContext(ctx, "xflow step "+action+" done", attrs...)
}

func (slogMonitor) OnFlow(ctx context.Context, e *FlowEvent) {
	attrs := []any{"flow", e.Flow, "elapsed_ms", millis(e.Duration), "result", e.Result.String()}

	// 回滚有失败意味着有资源没补偿回来，需要人工介入——这一条必须醒目
	if len(e.Result.RollbackErrors) > 0 {
		slog.ErrorContext(ctx, "xflow rollback did not complete, resources may be left dangling",
			append(attrs, "uncompensated_steps", len(e.Result.RollbackErrors))...)
		return
	}
	if !e.Result.Success() {
		slog.WarnContext(ctx, "xflow flow failed", attrs...)
		return
	}
	slog.InfoContext(ctx, "xflow flow done", attrs...)
}

// millis 耗时换成毫秒，保留到微秒。字段名带单位：slog 的 JSON 把 Duration 写成纳秒整数
func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
