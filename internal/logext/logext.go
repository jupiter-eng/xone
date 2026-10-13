// Package logext 放 xlog 的三个扩展点：链路标识提取器、日志观察者、ctx 里的日志字段作用域（scope.go）。
//
// 它们原来就在 xlog 里。挪到这里，注入的一方（xtrace、xmetric、xkafka）就不必 import xlog：
// 只用 xgorm、xredis、xkafka 这类集成的程序不会因为它们顺带装上 xlog、被换掉 slog.Default。
// xlog 读这里的值，公开的 xlog.SetTraceExtractor / xlog.AddObserver / xlog.TraceIDs / xlog.CtxWithKV 转发到这里。
package logext

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
)

// TraceExtractor 从 context 里取出链路标识，契约见 xlog.TraceExtractor
type TraceExtractor func(ctx context.Context) (traceID, spanID string)

var traceExtractor atomic.Pointer[TraceExtractor]

// SetTraceExtractor 注入链路标识提取器，传 nil 表示取消注入
func SetTraceExtractor(f TraceExtractor) {
	if f == nil {
		traceExtractor.Store(nil)
		return
	}
	traceExtractor.Store(&f)
}

// TraceIDs 用注入的提取器取链路标识；没有注入、或 ctx 为 nil 时返回两个空串
func TraceIDs(ctx context.Context) (traceID, spanID string) {
	f := traceExtractor.Load()
	if f == nil || ctx == nil {
		return "", ""
	}
	return (*f)(ctx)
}

// Observer 观察每一条实际写出的日志，契约见 xlog.Observer
type Observer func(ctx context.Context, r slog.Record)

var observers atomic.Pointer[[]Observer]

// AddObserver 注入一个日志观察者，nil 忽略。只增不减
func AddObserver(o Observer) {
	if o == nil {
		return
	}
	for {
		old := observers.Load()
		var cur []Observer
		if old != nil {
			cur = *old
		}
		// Clip 之后 append 一定另起一个底层数组：别的协程可能正在读 *old
		next := append(slices.Clip(cur), o)
		if observers.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Observers 返回当前全部观察者。返回的切片不会再被改写，可以放心遍历
func Observers() []Observer {
	if p := observers.Load(); p != nil {
		return *p
	}
	return nil
}

// ResetObservers 清空观察者，返回的函数把原来的装回去。只给测试用
func ResetObservers() (restore func()) {
	old := observers.Swap(nil)
	return func() { observers.Store(old) }
}
