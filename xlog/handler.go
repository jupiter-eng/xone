package xlog

import (
	"context"
	"log/slog"

	"github.com/jupiter-eng/xone/internal/logext"
)

// TraceExtractor 从 context 里取出链路标识。
//
// 这是本包唯一的扩展点，由 xtrace 在初始化时注入。契约：
//   - 传 nil 表示取消注入
//   - 后注入的覆盖先注入的（链路系统本来就只该有一个）
//   - 未注入时日志不含 trace 字段，不报错
//
// 这样 xlog 不必依赖 OpenTelemetry。注入点本身放在核心的 internal/logext 里，
// xtrace 往那里写、不 import xlog：只用 xgorm 这类集成的程序不会因此被装上 xlog。
type TraceExtractor = logext.TraceExtractor

// SetTraceExtractor 注入链路标识提取器
func SetTraceExtractor(f TraceExtractor) { logext.SetTraceExtractor(f) }

// Observer 观察每一条实际写出的日志。由 xmetric 之类的包注入。
//
// 只在日志通过级别过滤、真的要写出去时才被调用，收到的记录与写进输出的是同一条。
// 观察者里 panic 会被隔离：观测出问题不该把日志本身打断。
//
// 这是个只增不减的列表——观察者随进程存活，没有注销一说。
type Observer = logext.Observer

// AddObserver 注入一个日志观察者。
//
// xmetric 用它统计 Error 级别的日志条数，而不必反过来让 xlog 认识 Prometheus。
//
// 为什么不是在外面包一层 slog.Handler：slog.SetDefault 会把标准库 log 包的输出
// 也接到新 handler 上，于是「包一层再设回去」可能绕成环——
// 记录经 log.Output 又流回同一个 handler，卡死在 log 包那把不可重入的锁上。
// 让 xlog 自己持有扩展点就没有这个问题。
func AddObserver(o Observer) { logext.AddObserver(o) }

// notify 把记录交给所有观察者，逐个隔离 panic
func notify(ctx context.Context, r slog.Record) {
	for _, o := range logext.Observers() {
		callObserver(o, ctx, r)
	}
}

func callObserver(o Observer, ctx context.Context, r slog.Record) {
	defer func() {
		if rec := recover(); rec != nil {
			warnf("observer panic: %v", rec)
		}
	}()
	o(ctx, r)
}

// TraceIDs 返回当前 ctx 对应的链路标识，即本包会往日志里写的那两个值。
//
// 给需要链路标识、但不想依赖 OpenTelemetry 的包用。
// 没有注入提取器、或 ctx 里没有有效 Span 时返回两个空串。
func TraceIDs(ctx context.Context) (traceID, spanID string) { return logext.TraceIDs(ctx) }

// groupOrAttrs 记录一次 WithGroup 或 WithAttrs 调用，用于在开过分组时重放调用链
type groupOrAttrs struct {
	group string      // 非空表示这是一次 WithGroup
	attrs []slog.Attr // group 为空时有效
}

// ctxHandler 在标准库 handler 之上补充 context 里携带的字段。
//
// 只做「加字段」这一件事，格式化、级别过滤、分组全部交给被包装的
// slog.JSONHandler / slog.TextHandler —— 它们已经做得很好，没有理由重写。
//
// 分组是唯一的麻烦：标准库会把记录上的属性归到当前打开的分组里，
// 于是 logger.WithGroup("g") 之后 trace_id 会变成 g.trace_id。
// 但 trace_id 是整条记录的身份，不属于用户划的任何一组，必须留在顶层。
// 所以开过分组后走「在 base 上挂 ctx 字段、再重放调用链」的慢路径；
// 没开过分组时属性本来就落在顶层，沿用零额外开销的快路径。
type ctxHandler struct {
	next    slog.Handler   // 已应用 With/WithGroup 的 handler
	base    slog.Handler   // 未应用任何 With/WithGroup 的原始 handler
	chain   []groupOrAttrs // base → next 的调用链，仅 grouped 时有意义
	grouped bool           // 是否调用过 WithGroup
}

func newCtxHandler(h slog.Handler) *ctxHandler {
	return &ctxHandler{next: h, base: h}
}

func (h *ctxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	notify(ctx, r)

	attrs := ctxAttrs(ctx)
	if len(attrs) == 0 {
		return h.next.Handle(ctx, r) // 常见情况：没有任何 ctx 字段，零额外开销
	}
	if !h.grouped {
		r.AddAttrs(attrs...) // 没有分组，记录上的属性就在顶层
		return h.next.Handle(ctx, r)
	}
	return h.replay(attrs).Handle(ctx, r)
}

// replay 把 ctx 字段挂在最外层，再按原顺序重放 With/WithGroup 调用链。
// 每条记录重建一次 handler，只有用过 WithGroup 的 logger 才会付这份开销。
func (h *ctxHandler) replay(attrs []slog.Attr) slog.Handler {
	out := h.base.WithAttrs(attrs)
	for _, g := range h.chain {
		if g.group != "" {
			out = out.WithGroup(g.group)
		} else {
			out = out.WithAttrs(g.attrs)
		}
	}
	return out
}

func (h *ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.fork(groupOrAttrs{attrs: attrs}, h.next.WithAttrs(attrs), h.grouped)
}

func (h *ctxHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.fork(groupOrAttrs{group: name}, h.next.WithGroup(name), true)
}

// fork 派生一个新 handler。调用链必须无条件记全：分组可能在若干次 With 之后才出现，
// 那时先前挂的属性同样要参与重放，漏掉就会在慢路径上凭空丢字段。
// 每次都新建切片而不是 append 复用，避免两个 logger 从同一父节点派生时互相覆盖。
func (h *ctxHandler) fork(g groupOrAttrs, next slog.Handler, grouped bool) *ctxHandler {
	chain := make([]groupOrAttrs, len(h.chain), len(h.chain)+1)
	copy(chain, h.chain)
	return &ctxHandler{next: next, base: h.base, chain: append(chain, g), grouped: grouped}
}

// ctxAttrs 收齐 context 里携带的所有字段。
// 一次性收齐再 AddAttrs：slog.Record 只内联前 5 个属性，逐个添加会让底层切片反复扩容。
func ctxAttrs(ctx context.Context) []slog.Attr {
	s := scopeFrom(ctx)
	n := 0
	if s != nil {
		n = s.Len()
	}

	// 先把链路标识取出来，再决定要不要分配这个切片。
	// 「装了提取器、但这条 ctx 里没有 Span」是很常见的情况——启动日志、
	// 后台任务、定时任务都是。先分配的话，这些日志每行都白白多一次分配，
	// 建出来的还是个空切片。
	traceID, spanID := TraceIDs(ctx)
	if n == 0 && traceID == "" {
		return nil
	}

	attrs := make([]slog.Attr, 0, n+2)
	if traceID != "" {
		attrs = append(attrs, slog.String("trace_id", traceID))
		if spanID != "" {
			attrs = append(attrs, slog.String("span_id", spanID))
		}
	}
	if s != nil {
		s.Each(func(k string, v any) { attrs = append(attrs, slog.Any(k, v)) })
	}
	return attrs
}
