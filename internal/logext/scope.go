package logext

import (
	"context"
	"maps"
	"sync"
)

// The scope of log fields used to live in xlog as well (xlog.CtxWithScope / CtxWithKV / AddKV
// forward to here).
// It moved here for the same reason as the trace identifier extractor: xkafka wants to put fields
// such as topic and partition into each message's ctx without importing xlog for it -- a program
// using xkafka only to produce messages should not have slog.Default replaced.
// The fields are read out by xlog's handler and written into the logs; with xlog not installed they
// stay in the ctx and nobody reads them.

// ctxScopeKey is the KV scope's key in a context.
// A private type rather than a string, so that it cannot collide with another package's context
// key.
type ctxScopeKey struct{}

// Scope is the set of fields shared by one request (or one stretch of a call chain).
//
// What goes into the context is a **pointer**, so a write through Add is visible at once everywhere
// that ctx is held and no new context has to be handed back to the caller -- a business function
// deep in the call stack has no *gin.Context and no way to hand one back anyway. This is exactly
// what the "return a new context" spelling cannot solve.
//
// A log may be written from any goroutine, so reads and writes both take the lock.
type Scope struct {
	mu sync.RWMutex
	kv map[string]any
}

// scopeInitCap is the map's initial capacity on the first write
const scopeInitCap = 8

// Add writes one field
func (s *Scope) Add(k string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		s.kv = make(map[string]any, scopeInitCap)
	}
	s.kv[k] = v
}

// AddAll writes several fields at once
func (s *Scope) AddAll(kvs map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		s.kv = make(map[string]any, max(len(kvs), scopeInitCap))
	}
	maps.Copy(s.kv, kvs)
}

// Each ranges under the read lock, so that no map is copied for every line of log
func (s *Scope) Each(f func(k string, v any)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.kv {
		f(k, v)
	}
}

// Len is the number of fields
func (s *Scope) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.kv)
}

// copyWith copies the fields and lays kvs on top (kvs decides for a name in both)
func (s *Scope) copyWith(kvs map[string]any) *Scope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := &Scope{kv: make(map[string]any, max(len(s.kv)+len(kvs), scopeInitCap))}
	maps.Copy(c.kv, s.kv)
	maps.Copy(c.kv, kvs)
	return c
}

// ScopeFrom is the scope inside a ctx, or nil when there is none
func ScopeFrom(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(ctxScopeKey{}).(*Scope)
	return s
}

// WithScope opens an empty scope; the contract is in xlog.CtxWithScope: a ctx that already has one
// is returned unchanged
func WithScope(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ScopeFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, ctxScopeKey{}, &Scope{})
}

// WithKV derives a new context carrying kvs; the contract is in xlog.CtxWithKV: the fields the
// parent context already has are carried along with kvs laid on top, and what is written into the
// derived scope does not flow back into the parent context
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
