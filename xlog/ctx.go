package xlog

import (
	"context"
	"sync/atomic"

	"github.com/jupiter-eng/xone/internal/logext"
)

// CtxWithScope 开启一个字段作用域。
//
// 之后在这条 context 上任何位置调用 AddKV 写入的字段，都会出现在该 context
// 记录的每一条日志里——包括请求入口在 handler 返回之后写的那条访问日志。
//
// 幂等：已经有作用域时原样返回，重复调用（比如中间件被注册了两次）不会
// 清空已经写入的字段。
func CtxWithScope(ctx context.Context) context.Context { return logext.WithScope(ctx) }

// CtxWithKV 派生一个带着 kvs 的新 context：父 context 已有的字段照样带着，
// 再叠上 kvs（同名以 kvs 为准）。
//
// 和 AddKV 的分工在影响范围：AddKV 原地写，同一个请求里后面的每一行日志都带上；
// CtxWithKV 只影响它返回的那个 context，适合给一段调用打标——批量处理里每一条
// 带上自己的 order_id、起一个 goroutine 带上 worker 名字——兄弟之间互不串。
//
//	for _, o := range orders {
//		ctx := xlog.CtxWithKV(ctx, map[string]any{"order_id": o.ID})
//		process(ctx, o) // 这里面的日志都带着 order_id，下一轮不带上一轮的
//	}
//
// 派生出来的 context 自带一个作用域：在它上面 AddKV 只写进它自己，
// 不回流到父 context——入口处的访问日志因此看不到这些字段。
// 派生之后父 context 再 AddKV 的字段，它也看不到：它拿的是派生那一刻的副本。
func CtxWithKV(ctx context.Context, kvs map[string]any) context.Context {
	return logext.WithKV(ctx, kvs)
}

// AddKV 往当前作用域写一个字段。
//
// 没有作用域时丢弃并计数——否则「字段就是没出现在日志里」不留任何痕迹。
func AddKV(ctx context.Context, k string, v any) {
	s := scopeFrom(ctx)
	if s == nil {
		droppedKV.Add(1)
		return
	}
	s.Add(k, v)
}

// AddKVs 往当前作用域批量写字段
func AddKVs(ctx context.Context, kvs map[string]any) {
	if len(kvs) == 0 {
		return
	}
	s := scopeFrom(ctx)
	if s == nil {
		droppedKV.Add(int64(len(kvs)))
		return
	}
	s.AddAll(kvs)
}

// droppedKV 在没有作用域的 context 上被丢弃的字段数。
// 供排查「我明明 AddKV 了但日志里没有」——多半是漏了 CtxWithScope。
var droppedKV atomic.Int64

// DroppedKVCount 返回被丢弃的字段数，不为零说明有 AddKV 调用没有对应的作用域
func DroppedKVCount() int64 { return droppedKV.Load() }

// scopeFrom 作用域本身在 internal/logext 里，理由见那里
func scopeFrom(ctx context.Context) *logext.Scope { return logext.ScopeFrom(ctx) }
