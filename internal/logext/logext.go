// Package logext holds xlog's three extension points: the trace identifier extractor, the log
// observers, and the scope of log fields inside a ctx (scope.go).
//
// They used to live in xlog. Moved here, whoever installs one (xtrace, xmetric, xkafka) need not
// import xlog: a program using only integrations such as xgorm, xredis or xkafka does not get xlog
// installed along with them and have slog.Default replaced. xlog reads the values here, and the
// public xlog.SetTraceExtractor / xlog.AddObserver / xlog.TraceIDs / xlog.CtxWithKV forward to
// here.
package logext

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
)

// TraceExtractor takes the trace identifiers out of a context; the contract is in
// xlog.TraceExtractor
type TraceExtractor func(ctx context.Context) (traceID, spanID string)

var traceExtractor atomic.Pointer[TraceExtractor]

// SetTraceExtractor installs the trace identifier extractor; nil removes it
func SetTraceExtractor(f TraceExtractor) {
	if f == nil {
		traceExtractor.Store(nil)
		return
	}
	traceExtractor.Store(&f)
}

// TraceIDs takes the trace identifiers through the installed extractor; with none installed, or a
// nil ctx, it returns two empty strings
func TraceIDs(ctx context.Context) (traceID, spanID string) {
	f := traceExtractor.Load()
	if f == nil || ctx == nil {
		return "", ""
	}
	return (*f)(ctx)
}

// Observer watches every log that is really written; the contract is in xlog.Observer
type Observer func(ctx context.Context, r slog.Record)

var observers atomic.Pointer[[]Observer]

// AddObserver installs one log observer and ignores nil. They are only ever added
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
		// After Clip, append always starts a new backing array: another goroutine may be reading *old
		next := append(slices.Clip(cur), o)
		if observers.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Observers returns every observer there is now. The slice returned is never rewritten and is
// safe to range over
func Observers() []Observer {
	if p := observers.Load(); p != nil {
		return *p
	}
	return nil
}

// ResetObservers clears the observers and returns a function putting the previous ones back. For
// the tests alone
func ResetObservers() (restore func()) {
	old := observers.Swap(nil)
	return func() { observers.Store(old) }
}
