package logext

import (
	"context"
	"maps"
	"sync"
)

// 日志字段的作用域原来也在 xlog 里（xlog.CtxWithScope / CtxWithKV / AddKV 转发到这里）。
// 挪到这里的理由同链路标识提取器：xkafka 要给每条消息的 ctx 带上 topic、partition 这些字段，
// 又不该为此 import xlog——只用 xkafka 生产消息的程序不该被换掉 slog.Default。
// 字段由 xlog 的 handler 读出来写进日志；没装 xlog 时它们留在 ctx 里，没人读。

// ctxScopeKey KV 作用域在 context 中的 key。
// 用私有类型而不是字符串，避免与其它包的 context key 撞上。
type ctxScopeKey struct{}

// Scope 一次请求（或一段调用链）共享的字段集合
//
// 存进 context 的是**指针**，所以 Add 的写入对所有持有该 ctx 的地方立即可见，
// 不需要把新 context 回传给调用方——业务函数在调用栈深处拿不到 *gin.Context，
// 本来也没机会回传。这正是「返回新 context」那种写法解决不了的场景。
//
// 日志可能在任意 goroutine 写出，读写都要加锁。
type Scope struct {
	mu sync.RWMutex
	kv map[string]any
}

// scopeInitCap 首次写入时 map 的初始容量
const scopeInitCap = 8

// Add 写一个字段
func (s *Scope) Add(k string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		s.kv = make(map[string]any, scopeInitCap)
	}
	s.kv[k] = v
}

// AddAll 批量写字段
func (s *Scope) AddAll(kvs map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		s.kv = make(map[string]any, max(len(kvs), scopeInitCap))
	}
	maps.Copy(s.kv, kvs)
}

// Each 在读锁内遍历，避免为每一行日志复制一次 map
func (s *Scope) Each(f func(k string, v any)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.kv {
		f(k, v)
	}
}

// Len 字段数
func (s *Scope) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.kv)
}

// copyWith 复制一份字段，再叠上 kvs（同名以 kvs 为准）
func (s *Scope) copyWith(kvs map[string]any) *Scope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := &Scope{kv: make(map[string]any, max(len(s.kv)+len(kvs), scopeInitCap))}
	maps.Copy(c.kv, s.kv)
	maps.Copy(c.kv, kvs)
	return c
}

// ScopeFrom ctx 里的作用域，没有时返回 nil
func ScopeFrom(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(ctxScopeKey{}).(*Scope)
	return s
}

// WithScope 开启一个空的作用域，契约见 xlog.CtxWithScope：已经有作用域时原样返回
func WithScope(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ScopeFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, ctxScopeKey{}, &Scope{})
}

// WithKV 派生一个带着 kvs 的新 context，契约见 xlog.CtxWithKV：
// 父 context 已有的字段照样带着，再叠上 kvs；写进派生出来的作用域的，不回流到父 context
func WithKV(ctx context.Context, kvs map[string]any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	parent := ScopeFrom(ctx)
	if parent == nil {
		parent = &Scope{}
	}
	return context.WithValue(ctx, ctxScopeKey{}, parent.copyWith(kvs))
}
