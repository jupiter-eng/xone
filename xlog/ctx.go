package xlog

import (
	"context"
	"sync/atomic"

	"github.com/jupiter-eng/xone/internal/logext"
)

// CtxWithScope opens a field scope.
//
// Every field AddKV writes anywhere on that context afterwards appears in every log line recorded on it --
// including the access log the request entry point writes after the handler returns.
//
// It is idempotent: an existing scope is returned as it is, so calling it again (a middleware registered twice, say)
// does not clear the fields already written.
func CtxWithScope(ctx context.Context) context.Context { return logext.WithScope(ctx) }

// CtxWithKV derives a new context carrying kvs: the fields the parent context already has are kept and kvs is
// layered on top (kvs wins on a name clash).
//
// The division of labor with AddKV is the reach: AddKV writes in place and every later line in the same request
// carries it; CtxWithKV affects only the context it returns, which suits labelling one stretch of calls -- each item
// in a batch carrying its own order_id, a goroutine carrying its worker name -- without siblings bleeding into each other.
//
//	for _, o := range orders {
//		ctx := xlog.CtxWithKV(ctx, map[string]any{"order_id": o.ID})
//		process(ctx, o) // every log in here carries order_id, and the next round does not carry the previous one's
//	}
//
// The derived context brings a scope of its own: an AddKV on it writes only into itself and does not flow back to
// the parent context -- so the access log at the entry point does not see these fields.
// It also does not see fields the parent adds afterwards: it took a copy at the moment it was derived.
func CtxWithKV(ctx context.Context, kvs map[string]any) context.Context {
	return logext.WithKV(ctx, kvs)
}

// AddKV writes one field into the current scope.
//
// Without a scope it is discarded and counted -- otherwise "the field simply did not appear in the log" leaves no trace.
func AddKV(ctx context.Context, k string, v any) {
	s := scopeFrom(ctx)
	if s == nil {
		droppedKV.Add(1)
		return
	}
	s.Add(k, v)
}

// AddKVs writes several fields into the current scope.
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

// droppedKV is the number of fields discarded on a context with no scope.
// It is there for investigating "I called AddKV and the log has nothing" -- most likely a missing CtxWithScope.
var droppedKV atomic.Int64

// DroppedKVCount returns the number of discarded fields; a non-zero value means some AddKV call had no matching scope.
func DroppedKVCount() int64 { return droppedKV.Load() }

// scopeFrom: the scope itself lives in internal/logext, for the reason given there.
func scopeFrom(ctx context.Context) *logext.Scope { return logext.ScopeFrom(ctx) }
