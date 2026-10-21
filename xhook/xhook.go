// Package xhook holds the two verbs an integration joins the framework with: BeforeStart and BeforeStop.
// Writing an integration means knowing this package and xconfig (for reading the configuration), nothing else.
//
//	func init() {
//		xhook.BeforeStart(initXMine, xhook.At(xhook.StageClient))
//		xhook.BeforeStop(closeXMine) // the stage follows the start hook above
//	}
//
//	func initXMine(ctx context.Context) error {
//		c := DefaultConfig()
//		if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
//			return err
//		}
//		client, closer, err := New(ctx, c)
//		if err != nil {
//			return err
//		}
//		setDefault(client, closer) // where it is kept and how it is fetched is the integration's own business
//		return nil
//	}
//
//	func closeXMine(context.Context) error { return currentCloser().Close() }
//
// What goes inside is ordinary Go code: read the configuration, build the instance, keep it somewhere. The
// framework only calls them at the right moment, and calls the stop hooks in the opposite order.
//
// A stage (At) says "must come before or after whom". Business code does not write one -- the default
// StageBusiness comes after every client and before the servers, so a hook can call xgorm.C() directly.
// StageClient is written, as above, only for clients such as databases and caches that business code depends
// on; a stop hook writes no stage and follows the start hook it is paired with.
//
// This package has no third-party dependencies and does not import the framework itself: an integration knows
// only this package and xconfig, which is what makes releasing one as a module of its own work.
package xhook

import (
	"context"
	"reflect"
	"runtime"
	"strings"

	"github.com/jupiter-eng/xone/internal/hook"
)

// Stage is the resource layer: the smaller the value, the lower the layer -- started earlier, closed later.
type Stage = hook.Stage

const (
	// StageLog is logging: first up and last down, so every other component's start and stop logs get written.
	StageLog = hook.StageLog

	// StageTelemetry is tracing and metrics: they have to be ready before any client, so the Spans clients emit
	// have somewhere to attach and the metrics they record are collected.
	StageTelemetry = hook.StageTelemetry

	// StageClient is infrastructure such as databases, caches and HTTP clients, which business code uses.
	// The framework's own xgorm, xredis, xhttp and xcache all sit on this stage.
	StageClient = hook.StageClient

	// StageBusiness is the caller's own business resources: warming a local cache, scheduled jobs, message consumers.
	// They use the clients (a hook can call xgorm.C() directly) and the servers depend on them. It is the default.
	StageBusiness = hook.StageBusiness

	// StageServer is the outward-facing servers: last up and first down.
	StageServer = hook.StageServer
)

// HookFunc is one hook.
//
// The ctx is the process start-and-stop lifetime: the one a start hook receives is cancelled when the stop
// signal arrives, and the one a stop hook receives carries its share of the stop budget. Anything that blocks
// (connecting, retrying, probing) has to pass it down -- otherwise a SIGTERM arriving halfway through startup
// leaves the process stuck there until that work finishes on its own.
type HookFunc func(ctx context.Context) error

// Option is an optional setting for a hook.
type Option func(*options)

type options struct {
	stage Stage
	set   bool // At was written explicitly
}

// At sets the stage. A start hook without one gets StageBusiness; a stop hook without one follows the start
// hook it is paired with (see BeforeStop), and gets StageBusiness only when there is no pair.
//
//	xhook.BeforeStart(initXLog, xhook.At(xhook.StageLog))
//
// It is needed only for things that must come before or after others: logging first up and last down, tracing
// and metrics before the clients, the clients before business code. Business code writes none; the default is right.
func At(s Stage) Option { return func(o *options) { o.stage, o.set = s, true } }

// BeforeStart registers a start hook, run after the configuration is loaded and before the servers come up.
//
// They run in ascending stage order. If one returns an error (or panics), startup fails there and then: the
// remaining start hooks do not run and no server comes up, while the stop hooks paired with those that did
// succeed run in reverse to clean up (sharing the one WithStopTimeout budget), and Run returns that error.
//
// Within a stage they run in registration order, which is the order Go initializes the packages: identical on
// every run for the same code, but decided by the import relationships and the lexical order of import paths,
// not by how the import statements are written. So things in one stage should not depend on each other.
func BeforeStart(f HookFunc, opts ...Option) {
	hook.AddStart(entry(f, opts))
}

// BeforeStop registers a stop hook, run after the servers have stopped.
//
// The order mirrors BeforeStart as a whole: descending by stage, and in reverse registration order within a stage.
//
// It is paired with the nearest start hook registered before it in the same package and runs only if that start
// hook succeeded, so a stop hook never handles "it was never built". With no start hook before it, it always runs.
//
// Without an At, the stage follows that start hook: declaring the stage once, on the start hook, already gives
// both "started first" and "closed last". An explicit At wins.
//
// One returning an error does not interrupt the others -- the shutdown phase closes as much as it can.
func BeforeStop(f HookFunc, opts ...Option) {
	hook.AddStop(entry(f, opts))
}

func entry(f HookFunc, opts []Option) hook.Entry {
	o := options{stage: StageBusiness}
	for _, opt := range opts {
		opt(&o)
	}
	full := fullName(f)
	return hook.Entry{Name: shortName(full), Pkg: registrant(full), Stage: o.stage, Run: hook.Func(f), Inherit: !o.set}
}

// registrant works out which package is registering: the package owning the nearest package initialization
// function on the call stack (pkg.init, pkg.init.0, ...). The framework pairs starts with stops by it (see BeforeStop).
//
// Looking only at the hook function's own name gets it wrong: a hook registered through a helper package (a
// closure built inside the helper, or a method value of a helper type) carries a name belonging to the helper,
// so every package registering through it is taken for the same one -- one package's start hook fails and another
// package's stop hook is skipped with it. Without an init frame on the stack (a direct call in a test), it falls back to the hook function's package.
func registrant(full string) string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if pkg, ok := initPkg(f.Function); ok {
			return pkg
		}
		if !more {
			return pkgOf(full)
		}
	}
}

// initPkg recognizes a package initialization function: github.com/you/app/xredis.init, ....init.0, and returns the package path.
//
// A closure inside an init (....init.0.func1) does not count: one frame further out is the init it lives in.
func initPkg(fn string) (string, bool) {
	pkg := pkgOf(fn)
	rest, ok := strings.CutPrefix(fn, pkg+".")
	if !ok {
		return "", false
	}
	if rest == "init" {
		return pkg, true
	}
	n, ok := strings.CutPrefix(rest, "init.")
	return pkg, ok && n != "" && strings.Trim(n, "0123456789") == ""
}

// pkgOf takes "github.com/you/app/xredis" out of "github.com/you/app/xredis.initXRedis".
//
// It takes the full import path rather than the last element: last elements collide. Wrapping a package of
// one's own called xlog is a common thing to do, and on a collision the two are taken for the same package --
// that one's start hook fails and the framework xlog's stop hook is skipped with it, so the log writer never
// flushes again. This key never appears in any output, so its length costs nothing.
func pkgOf(full string) string {
	// A package path may contain dots (github.com) and so may a function name (the Comp.Init-fm of a method
	// value), so the boundary is the first "." after the last "/".
	slash := strings.LastIndex(full, "/")
	if i := strings.Index(full[slash+1:], "."); i >= 0 {
		return full[:slash+1+i]
	}
	return full
}

// fullName takes the hook function's full name, of the form github.com/jupiter-eng/xone/xredis.initXRedis.
//
// Asking for a name string on top of that would be pure duplication -- the name is written on the function
// just passed in. When it cannot be taken (nil was passed) it falls back to a placeholder, which beats an empty string.
func fullName(f HookFunc) string {
	fn := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if fn == nil {
		return "hook"
	}
	return fn.Name()
}

// shortName drops the path prefix, leaving "xredis.initXRedis", for logs and error messages.
//
// The name of a generic instantiation has the form xredis.newFor[...], where the type arguments are folded by
// the runtime into "...", so no "/" inside the brackets can disturb this split.
func shortName(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}
