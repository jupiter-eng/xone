package xredis

import (
	"context"
	"slices"
	"strings"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// redisotel v9.22.0 记错误就是 span.RecordError(err) 加 span.SetStatus(codes.Error, err.Error())
// （tracing.go recordError），服务端的原文由此进了 exception.message 和状态描述——
// 原文里带着参数（见 redactedError）。它没有改写错误的选项，这里在它拿到的 Span 外面包一层：
// 两处都换成命令日志里的那份文本。Span 的其余部分（属性、事件、结束）原样交给里面那个。

// redactingTracerProvider 交给 redisotel 的 TracerProvider，发出的 Span 都是 redactingSpan
type redactingTracerProvider struct{ trace.TracerProvider }

func (p redactingTracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return redactingTracer{p.TracerProvider.Tracer(name, opts...)}
}

type redactingTracer struct{ trace.Tracer }

// Start Span 名换成 spanName 给的；ctx 里放的仍是里面那个 Span：之后的钩子（命令日志的 trace_id / span_id）照旧从 ctx 取
func (t redactingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := t.Tracer.Start(ctx, spanName(ctx, name), opts...)
	return ctx, &redactingSpan{Span: span}
}

// redisotel v9.22.0 的 Span 名是 cmd.FullName()，pipeline 是 "redis.pipeline " 加各条命令的 FullName
// （去重、最多 10 个，rediscmd.CmdsString）。FullName 是第 1 个参数原样转小写、不校验：实测
// Do(ctx, "SET k1 <值>") 的 Span 名是 "set k1 <值>"，pipeline 里是 "redis.pipeline set k2 <值>"。
// 光看名字分不出 pipeline 里哪几截是一条命令，所以 spanCmdsHook 挂在 redisotel 外层，把命令放进 ctx，
// 这里按命令重新起名，规则同命令日志的 cmd 字段（cmdName）。

// spanCmdsKey ctx 里放的是 redis.Cmder（单条命令）或 []redis.Cmder（pipeline）
type spanCmdsKey struct{}

// spanCmdsHook 挂在 redisotel 的钩子之前（go-redis 先挂的在外层），给 redactingTracer 留下这次的命令
type spanCmdsHook struct{}

func (spanCmdsHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (spanCmdsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		return next(context.WithValue(ctx, spanCmdsKey{}, cmd), cmd)
	}
}

func (spanCmdsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		return next(context.WithValue(ctx, spanCmdsKey{}, cmds), cmds)
	}
}

// spanName 只改 redisotel 按命令起的名字；redis.dial 这类不是从命令来的原样留着
// （建连发生在命令的钩子里，ctx 里也有命令，所以要先认名字）
func spanName(ctx context.Context, name string) string {
	switch v := ctx.Value(spanCmdsKey{}).(type) {
	case redis.Cmder:
		if name == v.FullName() {
			return spanCmdName(v)
		}
	case []redis.Cmder:
		if strings.HasPrefix(name, "redis.pipeline ") {
			names := make([]string, 0, min(len(v), 10))
			for _, c := range v {
				if n := spanCmdName(c); !slices.Contains(names, n) && len(names) < 10 {
					names = append(names, n)
				}
			}
			return "redis.pipeline " + strings.Join(names, " ")
		}
	}
	return name
}

// spanCmdName 一条命令的 Span 名：命令名同 cmdName；FullName 带着的子命令（cluster info、command count）
// 也像命令名才留下
func spanCmdName(cmd redis.Cmder) string {
	name := cmdName(cmd)
	if _, sub, ok := strings.Cut(cmd.FullName(), " "); ok && name != invalidCmdName && validCmdName.MatchString(sub) {
		return name + " " + sub
	}
	return name
}

// redactingSpan 记错误时只记 redactedError 给出的文本。
// 一个 Span 只在一个命令的钩子里用，不会并发
type redactingSpan struct {
	trace.Span
	text string // 上一次 RecordError 的错误收过之后的文本，SetStatus 用它
}

// RecordError 原文可以照记的（网络错误、超时）照旧记成事件，其余不记：事件里的 exception.message 就是原文
func (s *redactingSpan) RecordError(err error, opts ...trace.EventOption) {
	s.text, _ = redactedError(err)
	if clientSafe(err) {
		s.Span.RecordError(err, opts...)
	}
}

// SetStatus 出错时的描述换成 RecordError 收过的文本；前面没有 RecordError 的，不知道描述里是什么，一律不记原文
func (s *redactingSpan) SetStatus(code codes.Code, desc string) {
	if code == codes.Error {
		desc = s.text
		if desc == "" {
			desc = "redis error (message omitted, it may contain argument values)"
		}
	}
	s.Span.SetStatus(code, desc)
}
