// Package xclient provides "a set of instances organized by name" as such.
//
// xgorm, xredis and xcache all have the same shape: the configuration may hold one instance or
// several by name; at run time C() takes one by name; at startup they are built one by one, and
// one that fails to build closes everything already built.
// These semantics should be the same everywhere -- "what to say when a name is not found" and
// "what to do when the two spellings are mixed" are decided once, and spread across three modules
// they would become three implementations drifting apart.
//
// An integration uses it like this:
//
//	var reg = xclient.NewRegistry[*redis.Client]("xredis", ConfigKey)
//
//	func C(name ...string) *redis.Client { return reg.Get(name...) }
//	func Has(name ...string) bool        { return reg.Has(name...) }
//	func Names() []string                { return reg.Names() }
//
//	func initXRedis(ctx context.Context) error {
//		if !xconfig.Has(ConfigKey) {
//			return xclient.Build(ctx, reg, nil, build) // run it even when unconfigured, so that a later lookup says "not configured" rather than "called too early"
//		}
//		c := DefaultConfig()
//		if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
//			return err
//		}
//		return xclient.Build(ctx, reg, c.Clients, build)
//	}
//
//	// build builds one instance; name is what it is called in the configuration, written into the
//	// logs to tell them apart
//	func build(ctx context.Context, name string, c ClientConfig) (*redis.Client, io.Closer, error) {
//		return New(ctx, c)
//	}
//
//	func closeXRedis(context.Context) error { return reg.Close() }
//
// The connection probe at startup is one thing as well: how many attempts, how long each one gets,
// and no more attempts once authentication is refused. Probe gathers those three in one place (see
// probe.go), and each module only provides "how to probe once" and "how to recognize an
// authentication failure".
//
// It is internal: this is the shared implementation of this repository's three multi-instance
// modules, not something a caller has to learn. An integration of your own that wants several
// instances is welcome to copy it -- it is a map of instances by name, and not worth making
// everybody learn another package for.
//
// This package has no third-party dependencies.
package xclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/jupiter-eng/xone/xerror"
)

// DefaultName is the name used when an instance is taken without an argument, and the name the
// single-instance spelling is normalized to.
// It is written here alone: the constants of the same name in internal/config, xconfig and each
// integration all refer to it
const DefaultName = "default"

// Registry is a set of instances organized by name
type Registry[T any] struct {
	module string // appears in the panic text, "xgorm" for one
	key    string // the configuration block's top-level key, "XGorm" for one

	// Replaced whole rather than field by field: a reader always gets a complete one and needs no
	// lock. C() is on the path of every data access, and a read lock's counter is fought over across
	// cores: a concurrent Get on 4 cores was measured at about 100ns, slower than the 25ns of a
	// single goroutine; with an atomic pointer it is about 30ns
	state atomic.Pointer[snapshot[T]]
}

// snapshot is the registry's whole content at one moment, never modified once published
type snapshot[T any] struct {
	items   map[string]T
	closers []io.Closer
	phase   phase
}

// phase is where in the lifecycle the registry is. It serves one purpose: saying exactly why
// something was not found -- called too early, called too late, never configured and a misspelled
// name are four different problems, each sending somebody to a different place
type phase int

const (
	notStarted phase = iota // the start hooks have not run yet
	running                 // built; with the block unconfigured, that means none was built
	closed                  // the stop hooks have run
)

// NewRegistry creates a registry. module and key serve only to assemble the error messages.
func NewRegistry[T any](module, key string) *Registry[T] {
	r := &Registry[T]{module: module, key: key}
	r.state.Store(&snapshot[T]{items: map[string]T{}})
	return r
}

// Get takes one instance, the one called default without an argument. One that is not found panics
// outright.
//
// It does not return the zero value: returning nil would not get the program any further -- any
// method on the client is a nil pointer dereference, which only defers the same panic to the
// caller's first use of it, where the stack holds nothing but "invalid memory address" and says
// nothing about the configuration being the root cause.
// This is a configuration problem at startup, not an error to be handled at run time.
//
// An optional dependency (used when configured, skipped when not) checks with Has first.
func (r *Registry[T]) Get(name ...string) T {
	v, ok := r.Lookup(name...)
	if !ok {
		panic(r.missing(nameOf(name)))
	}
	return v
}

// Lookup takes one instance, returning the zero value and false when it is not found
func (r *Registry[T]) Lookup(name ...string) (T, bool) {
	v, ok := r.state.Load().items[nameOf(name)]
	return v, ok
}

// Has reports whether the named instance is configured, for an optional dependency to check
func (r *Registry[T]) Has(name ...string) bool {
	_, ok := r.Lookup(name...)
	return ok
}

// Names returns the configured instance names, sorted by name
func (r *Registry[T]) Names() []string {
	return slices.Sorted(maps.Keys(r.state.Load().items))
}

// All returns every instance there is now, name to instance.
//
// What it returns is a copy: a snapshot is immutable once published, and a caller changing it
// cannot reach it. It is also one moment's worth -- Names followed by Lookup one by one would
// return half of them when a Close falls in between.
func (r *Registry[T]) All() map[string]T {
	return maps.Clone(r.state.Load().items)
}

// missing: reporting "not found" alone helps little -- called too early, called too late, the
// whole block unconfigured and a misspelled name each send somebody to a different place, and the
// wording has to tell them apart at a glance.
//
// Called too early is the one most easily told wrong: it used to read the same as "the whole block
// is unconfigured", sending whoever took an instance in main, before Run, to a configuration file
// that was perfectly correct.
func (r *Registry[T]) missing(want string) string {
	s := r.state.Load()
	p, got := s.phase, slices.Sorted(maps.Keys(s.items))

	switch {
	case p == notStarted:
		return fmt.Sprintf("%s: instance %q was requested before xone.Run started %s — "+
			"use it from your Runnable's Start, a request handler, or a hook at the default stage (StageBusiness) or later", r.module, want, r.module)
	case p == closed:
		return fmt.Sprintf("%s: instance %q was requested after %s was closed — "+
			"nothing should use it once xone.Run has stopped the server", r.module, want, r.module)
	case len(got) == 0:
		return fmt.Sprintf("%s: no instance named %q, and none is configured at all — check the %s block in the config",
			r.module, want, r.key)
	}
	return fmt.Sprintf("%s: no instance named %q, configured ones are [%s]",
		r.module, want, strings.Join(got, " "))
}

func nameOf(name []string) string {
	if len(name) > 0 {
		return name[0]
	}
	return DefaultName
}

// Build builds every instance in name order and publishes them to the registry.
//
// It is called even with an empty cfgs (the block is unconfigured): nothing is built, but the
// registry learns from it that the start hooks have run, so a later lookup says "not configured"
// rather than "called too early".
//
// One that fails to build closes everything already built before the error is returned: when a
// start hook returns an error, this package's stop hook does not run, and without cleaning up here
// those connection pools would leak.
// The errors of the ones that could not be closed are joined into the error returned, so a leaked
// connection pool is visible.
//
// new receives the instance name: the log written once it is built has to say which one it is --
// with several configured, an address alone tells them apart for nobody.
//
// The ctx is passed all the way to new and checked once before each instance: with five databases
// configured and the first one retrying until it times out, a shutdown signal should stop it right
// there rather than having the remaining four tried one by one as well.
func Build[C, T any](ctx context.Context, r *Registry[T], cfgs map[string]C,
	new func(ctx context.Context, name string, cfg C) (T, io.Closer, error)) error {
	built := make(map[string]T, len(cfgs))
	var closers []io.Closer
	// abort closes what was already built and joins whatever could not be closed after err; with
	// everything closed it returns err unchanged, so the outermost layer is still this module's
	// *xerror.Error
	abort := func(err error) error {
		if cerr := closeAll(r.module, closers); cerr != nil {
			return errors.Join(err, cerr)
		}
		return err
	}

	// Built in sorted name order, so that the order of a failure can be reproduced and the order of
	// the logs is stable
	for _, name := range slices.Sorted(maps.Keys(cfgs)) {
		if err := ctx.Err(); err != nil {
			return abort(xerror.Newf(r.module, "new", "shutdown signal received before building instance %q: %w", name, err))
		}
		v, closer, err := safeNew(ctx, r.module, name, cfgs[name], new)
		if err != nil {
			return abort(err)
		}
		built[name] = v
		closers = append(closers, closer)
	}

	r.state.Store(&snapshot[T]{items: built, closers: closers, phase: running})
	return nil
}

// Close takes every instance out and closes them in reverse order, for a stop hook to use
// directly.
//
// Taken out before closing: the other way round, C() would still find an instance that is being
// closed halfway through.
func (r *Registry[T]) Close() error {
	old := r.state.Swap(&snapshot[T]{items: map[string]T{}, phase: closed})
	return closeAll(r.module, old.closers)
}

// safeNew builds one instance and contains a panic.
//
// Uncontained, the panic is thrown up through Build while the Closers of the instances already
// built exist only in Build's own frame as local variables -- once the stack unwinds they cannot be
// found again, and those are connection pools that can never be closed.
func safeNew[C, T any](ctx context.Context, module, name string, cfg C,
	new func(context.Context, string, C) (T, io.Closer, error)) (v T, closer io.Closer, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			v, closer, err = zero, nil, xerror.Newf(module, "new", "instance %q %w", name, panicked(r))
		}
	}()

	v, closer, err = new(ctx, name, cfg)
	if err != nil {
		err = named(module, name, err)
	}
	return v, closer, err
}

// named names the instance an error belongs to, and keeps one module boundary to one layer of
// xerror.
//
// What new returns is usually this module's xerror already, carrying an op such as config or
// connect. Another layer with new would turn the text into
// xone xgorm new failed, err=[instance "a": xone xgorm connect failed, err=[...]]: the module name
// repeated, the op that actually helps pushed inside, and errors.As always taking out new.
// So an error of this module keeps its own op with the instance name added in front of the
// message, and only the rest is wrapped with new.
//
// Only the outermost layer is looked at rather than errors.As: the latter walks past a layer
// somebody else wrapped, and rewriting it would lose that layer's text.
func named(module, name string, err error) error {
	if xe, ok := err.(*xerror.Error); ok && xe.Module == module {
		return xerror.Newf(module, xe.Op, "instance %q: %w", name, xe.Err)
	}
	return xerror.Newf(module, "new", "instance %q: %w", name, err)
}

// safeClose closes one instance and contains a panic.
//
// The reasoning mirrors safeNew's: one instance's Close blowing up should not leave the rest of the
// set unclosed.
func safeClose(module string, c io.Closer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = xerror.Newf(module, "close", "close %w", panicked(r))
		}
	}()
	return c.Close()
}

// panicked turns a recovered value into an error, as the function of the same name in xone.go
// does.
//
// When what was panicked with is an error itself, %w catches it: with %v it would be left as a
// piece of text and errors.Is / errors.As would stop here.
func panicked(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", r)
}

// closeAll closes in reverse order, where one failure does not affect the rest
func closeAll(module string, closers []io.Closer) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if closers[i] == nil {
			continue
		}
		if err := safeClose(module, closers[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
