// Package hook stores and runs the lifecycle hooks.
//
// It is split out for one reason only: to leave xhook's godoc holding nothing but what the caller
// really has to know. The types here are what the framework wires itself up with; a third-party
// integration cannot import them (the internal rule goes by path, and only packages under
// github.com/jupiter-eng/xone/... get in) and does not need to.
//
// This package has no third-party dependencies.
package hook

import (
	"cmp"
	"context"
	"slices"
	"sync"
)

// Stage is a resource's layer: the lower the value the deeper it sits -- started earlier and
// closed later.
//
// A handful of fixed stages rather than arbitrary numbers: numbers need coordinating globally (who
// gets 30 and who gets 50), stages do not -- whatever shares one stage does not depend on the rest
// of it to begin with, so the order within it does not matter.
type Stage int

const (
	// StageLog is logging: up first and closed last, so that every other component's start and stop
	// logs still get written
	StageLog Stage = iota

	// StageTelemetry is tracing and metrics: they have to be ready before any client, or the Spans a
	// client emits attach to nothing and the metrics it records are never collected
	StageTelemetry

	// StageClient is the infrastructure -- databases, caches, HTTP clients -- that the business uses
	StageClient

	// StageBusiness is the caller's own business resources: warming a local cache, scheduled jobs,
	// message consumers and so on. They use the clients and the servers depend on them. The default
	// stage
	StageBusiness

	// StageServer is what serves the outside: up last and closed first
	StageServer
)

// Func is one hook. The ctx is the process's start and stop lifetime.
type Func func(ctx context.Context) error

// Entry is one registration
type Entry struct {
	Name  string // appears in the logs and the errors; it is the hook function's own name
	Pkg   string // the package registering it (the one whose init is on the call stack), used to pair start with stop
	Stage Stage
	Run   Func
	Seq   int // a start hook's registration number, counted from 1
	Pair  int // the Seq of the start hook a stop hook is paired with, 0 for none, see AddStop

	// Inherit means the stop hook was given no stage explicitly: once AddStop has found its pair, it
	// takes that start hook's stage instead
	Inherit bool
}

var (
	mu    sync.Mutex
	start []Entry
	stop  []Entry
)

// AddStart registers a start hook
func AddStart(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	e.Seq = len(start) + 1
	start = append(start, e)
}

// AddStop registers a stop hook and pairs it right away: with the start hook of the same package
// registered most recently before it.
//
// Registered together makes a pair:
//
//	BeforeStart(openA); BeforeStop(closeA)
//	BeforeStart(openB); BeforeStop(closeB)
//
// closeA pairs with openA and closeB with openB. When openB fails, closeA runs as usual and closeB
// does not -- each closes its own and none has to handle "not built yet". With no start hook of the
// same package before it, Pair stays 0: such a stop hook depends on no startup and always runs.
//
// A stop hook with Inherit also takes the stage of the start hook it is paired with: one pair of
// hooks minds one resource, and their stage should be the same to begin with. One that found no
// pair keeps the stage it had.
func AddStop(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	for i := len(start) - 1; i >= 0; i-- {
		if start[i].Pkg == e.Pkg {
			e.Pair = start[i].Seq
			if e.Inherit {
				e.Stage = start[i].Stage
			}
			break
		}
	}
	stop = append(stop, e)
}

// Start takes the start hooks in ascending stage order, keeping the registration order within one
// stage.
func Start() []Entry {
	mu.Lock()
	defer mu.Unlock()
	return startOrder(start)
}

// startOrder sorts into start order: ascending stages, keeping the registration order within one
// stage.
func startOrder(in []Entry) []Entry {
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b Entry) int { return cmp.Compare(a.Stage, b.Stage) })
	return out
}

// Stop takes the stop hooks as the mirror image of Start's order: descending stages, and the
// registration order reversed within one stage.
//
// This way one resource declaring one stage gets both "started earlier" and "closed later".
func Stop() []Entry {
	mu.Lock()
	defer mu.Unlock()
	return stopOrder(stop)
}

// stopOrder sorts into stop order: the mirror image of Start's order.
func stopOrder(in []Entry) []Entry {
	out := startOrder(in)
	slices.Reverse(out)
	return out
}

// StopAfter takes the stop hooks that are to run, in Stop's order: the ones whose paired start
// hook did not succeed are skipped -- the resource they mind was never built. started holds the
// Seqs of the start hooks that succeeded; the unpaired ones (Pair 0) are always kept.
func StopAfter(started map[int]bool) []Entry {
	mu.Lock()
	defer mu.Unlock()
	var out []Entry
	for _, e := range stopOrder(stop) {
		if e.Pair != 0 && !started[e.Pair] {
			continue // the start hook it is paired with did not succeed, so the resource does not exist
		}
		out = append(out, e)
	}
	return out
}

// Reset clears the registration board. For the tests alone.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	start, stop = nil, nil
}
