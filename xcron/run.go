package xcron

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	// 不是匿名 import：job 字段靠 xlog.CtxWithKV 放进 ctx。顺带的效果和 xgin 一样——
	// 用了 xcron 就有 xlog，只跑定时任务的进程照样是 JSON 日志、带 trace_id
	"github.com/jupiter-eng/xone/xlog"
)

const tracerName = "github.com/jupiter-eng/xone/xcron"

// execute 跑一次任务，Add 的每一次执行都走这里。
//
// 替 fn 做的事：开一个根 Span（cron <name>），ctx 里带上 job=<name> 这个日志字段，
// 按 timeout 限时，记一行结果，接住 panic。返回 fn 的错误；panic 变成错误返回。
//
// 日志的级别：开始记 DEBUG，结束记 INFO，失败 WARN，panic ERROR。开始那一行不记 INFO，
// 是因为 @every 1s 的任务每秒就是两行，而结束那一行已经带着 elapsed_ms，开始时刻倒推得出来；
// 卡住不返回的任务看不到结束那一行，停止时的报错里点名还在跑的任务（见 scheduler.stop）。
// 和 xgin 的访问日志一样：一次执行一行。
//
// job 字段只放进 ctx 的日志作用域，不在这里再写一遍：两处都写，JSON 里就是两个同名的 key。
//
// Span 上只标状态、不记错误原文（不调 RecordError），同 xgorm / xredis：任务返回的错误常常包着
// 底层的 SQL 或 Redis 错误，原文里带着参数值，而 xgorm 刚刚费劲没把它们写进链路。原文在日志里
func execute(parent context.Context, name string, timeout time.Duration, fn func(context.Context) error) (err error) {
	ctx, span := otel.Tracer(tracerName).Start(parent, "cron "+name, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	ctx = xlog.CtxWithKV(ctx, map[string]any{"job": name})
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	start := time.Now()
	slog.DebugContext(ctx, "cron job started")
	defer func() {
		elapsed := ms(time.Since(start))
		if r := recover(); r != nil {
			err = panicked(r)
			span.SetStatus(codes.Error, "cron job panicked")
			slog.ErrorContext(ctx, "cron job panicked", "elapsed_ms", elapsed, "error", err, "stack", string(debug.Stack()))
			return
		}
		if err != nil {
			span.SetStatus(codes.Error, "cron job failed")
			slog.WarnContext(ctx, "cron job failed", "elapsed_ms", elapsed, "error", err)
			return
		}
		slog.InfoContext(ctx, "cron job finished", "elapsed_ms", elapsed)
	}()
	return fn(ctx)
}

// panicked 把 recover 到的值变成 error，本身是 error 的用 %w 接住，errors.Is / errors.As 还能用
func panicked(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", r)
}

// ms 耗时换成毫秒，保留到微秒。字段名带单位：slog 的 JSON 把 Duration 写成纳秒整数
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
