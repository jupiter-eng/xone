package xflow

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// StepEvent is the event of one finished step, shared by Process and Rollback.
type StepEvent struct {
	Flow       string
	Processor  string
	Dependency Dependency
	Rollback   bool // true means this was a rollback
	Err        error
	Duration   time.Duration
}

// FlowEvent is the event of a finished flow; Duration includes the rollback time.
type FlowEvent struct {
	Flow     string
	Result   *Result
	Duration time.Duration
}

// Monitor observes flow execution.
//
// Both callbacks are isolated against panics: a failing monitoring implementation loses one observation and never interrupts the business flow.
type Monitor interface {
	OnStep(ctx context.Context, e *StepEvent)
	OnFlow(ctx context.Context, e *FlowEvent)
}

// monitor is the implementation currently in effect, read and written atomically: Execute is a hot path and should not take a lock for it.
var monitor atomic.Pointer[Monitor]

// SetMonitor replaces the default monitoring implementation. Passing nil turns monitoring off.
func SetMonitor(m Monitor) {
	if m == nil {
		monitor.Store(nil)
		return
	}
	monitor.Store(&m)
}

func init() { SetMonitor(slogMonitor{}) }

// activeMonitor is the Monitor this execution uses, returning nil when monitoring is off (zero overhead).
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

// now reads the clock only while monitoring is on: the elapsed time is only for monitoring, and reading it per step
// anyway would mean six wasted clock reads per execution of a five-step flow, which would make "zero overhead" untrue.
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

// recoverNotify isolates panics in a monitoring implementation.
//
// It is deferred directly rather than deferring a closure that calls m.OnX: the closure would capture m, the ctx and
// the event, costing one more allocation per step on a path every step takes.
func recoverNotify() {
	if r := recover(); r != nil {
		slog.Error("xflow monitor panicked, isolated", "error", r)
	}
}

// slogMonitor is the default implementation, writing to the standard library's slog.
//
// A successful step is recorded at info: turning XFlow.Monitor on means wanting to see every step, and that should be visible at the default level.
// A five-step flow writes six lines per execution; anyone not wanting per-step logs turns Monitor off or replaces it with SetMonitor.
type slogMonitor struct{}

func (slogMonitor) OnStep(ctx context.Context, e *StepEvent) {
	// A successful step is recorded at info. When the level is not enabled (the log level is set to warn, say) it returns
	// here rather than assembling the line first and handing it to slog to discard -- a five-step flow would assemble five lines per execution, all for nothing.
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
		// A panic's call stack is not in the error message (see PanicError), so it is recorded as a field of its own here.
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

	// A failed rollback means some resource was not compensated and needs a human -- this one has to stand out.
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

// millis converts an elapsed time to milliseconds, keeping microseconds. The field name carries the unit: slog's JSON writes a Duration as an integer number of nanoseconds.
func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
