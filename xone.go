// Package xone reads the configuration in one place, initializes every registered component by
// stage, and shuts them down in reverse order.
//
// Import the integrations to be used (xgin, xgorm, xredis and so on), write one configuration file,
// then call Run once:
//
//	func main() {
//		xone.MustRun(xgin.New().WithRoutes(routes))
//	}
//
// What Run does, in order:
//
//   - takes over SIGINT / SIGTERM (before the configuration is read);
//   - loads the configuration (WithConfigPath > --config > XONE_CONFIG > the conventional paths
//     such as conf/application.yml);
//   - runs every start hook (xhook.BeforeStart) by stage: logging -> tracing/metrics -> clients ->
//     business -> servers;
//   - calls Runnable.Start and blocks until it returns or a shutdown signal arrives;
//   - on exit: cancels Start's ctx, calls the optional Stop, waits for Start to return, then runs
//     the stop hooks in reverse order.
//
// A Runnable only has to write Start; one that cannot be stopped through the ctx alone adds a
// Stop(context.Context) error. The whole shutdown has one budget, WithStopTimeout (15s by default),
// of which the server may use at most 2/3. A second signal after the first terminates the process
// immediately.
//
// The usage and the reasoning behind the design are in the repository's docs/guide.md and
// docs/architecture.md.
package xone

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"syscall"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xerror"

	// The XApp block comes along with the framework: a program importing xone alone can still write
	// an application name. It only reads configuration and changes no global state.
	// xlog is not here: it replaces slog.Default, and a program using only xhook and xgorm should not
	// have its logging taken over. Only xgin / xecho / xcron bring it along (xtrace and xmetric do
	// not import it); any other program that wants it imports it anonymously
	_ "github.com/jupiter-eng/xone/xapp"
)

// Runnable is whatever has to keep running, usually the server.
//
// The ctx Start receives is cancelled when a shutdown signal arrives, and Start is expected to
// return because of it -- for a consumer, a scheduled job or a one-off task, that alone is enough.
//
// Whatever cannot be stopped through the ctx alone (an http.Server has to be told to Shutdown, for
// one) implements Stop(context.Context) error as well: on exit the framework calls it first, then
// waits for Start to return. Stop receives a ctx of its own carrying the server's share of the stop
// budget (see WithStopTimeout) -- it does not inherit that cancellation, which would have every
// shutdown step refused the moment it begins, leaving no graceful shutdown at all.
type Runnable interface {
	Start(context.Context) error
}

// Func turns a function into a Runnable: when fn returns, Run goes through shutdown, and fn's
// error is Run's error.
//
// A one-off task (a migration, a batch job, or anything that only wants the framework to build the
// components for one piece of work) returns when it is done. A consumer with a loop of its own
// loops until the ctx is cancelled, finishes what is in flight, and then returns -- the framework
// closes the databases and caches only after fn has returned (see docs/guide.md, "A service that
// is not a web server").
//
//	xone.MustRun(xone.Func(func(ctx context.Context) error {
//		return migrate(ctx, xgorm.CWithCtx(ctx))
//	}))
func Func(fn func(ctx context.Context) error) Runnable {
	if fn == nil {
		panic("xone: Func needs a function, got nil")
	}
	return funcRunnable(fn)
}

type funcRunnable func(context.Context) error

func (f funcRunnable) Start(ctx context.Context) error { return f(ctx) }

// UntilSignal is a Runnable that does nothing and blocks until a shutdown signal.
//
// It is for a process whose work all happens in hooks: a push-style consumer with goroutines of its
// own inside the SDK, for instance, started in xhook.BeforeStart and closed in xhook.BeforeStop.
// Run stops here once the start hooks are done, and runs the stop hooks in reverse once a signal
// arrives.
//
//	xone.MustRun(xone.UntilSignal())
func UntilSignal() Runnable {
	return Func(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
}

// stopper is the Stop a Runnable may optionally have
type stopper interface {
	Stop(context.Context) error
}

// checkRunnable catches, before any hook runs, the mistakes that would otherwise only surface
// once the process is running.
//
// A Stop that was written but cannot be called: Stop is optional, so the compiler does not catch it
// and it simply never runs -- the server cannot be stopped when the signal arrives and has to wait
// out the stop budget. There are two ways to write one that cannot be called: the wrong signature
// (a missing ctx, say), or a Stop on the pointer receiver while a value was passed in
// (Run(App{})).
func checkRunnable(r Runnable) error {
	if r == nil {
		return xerror.Newf("xone", "config", "Run needs a Runnable, got nil")
	}
	if _, ok := r.(stopper); ok {
		return nil
	}
	t := reflect.TypeOf(r)
	if m, ok := t.MethodByName("Stop"); ok {
		return xerror.Newf("xone", "config",
			"%T has a Stop method of type %v, but Run only calls Stop(context.Context) error: "+
				"fix the signature, or rename the method if it is not meant for shutdown", r, m.Type)
	}
	if t.Kind() != reflect.Pointer {
		if _, ok := reflect.PointerTo(t).MethodByName("Stop"); ok {
			return xerror.Newf("xone", "config",
				"%T has Stop on its pointer receiver, so Run cannot call it on a value: pass a pointer (&%T{...})", r, r)
		}
	}
	return nil
}

// defaultStopTimeout is the default stop budget: this much in total, from the start of shutdown
// until Run returns
const defaultStopTimeout = 15 * time.Second

// hookReserve is the minimum kept back for each stop hook whose turn has not come yet (see
// runStop).
//
// Without it, one connection pool that cannot be closed eats the whole budget and every hook behind
// it is out of time the moment it begins -- logging suffers most, as it is closed last precisely so
// that every other component's shutdown log still gets written. 1s is plenty for a Close or Flush
// that is not stuck; with many hooks and little time, the share is split evenly instead.
const hookReserve = time.Second

// Run reads the configuration, initializes every component, starts r, blocks until a shutdown
// signal, and then shuts everything down in reverse order.
func Run(r Runnable, opts ...Option) error {
	o := options{stopTimeout: defaultStopTimeout}
	for _, f := range opts {
		f(&o)
	}
	// Here 0 does not mean "unlimited" but "no wait at all": Stop would receive a context that has
	// already expired, the server would be cut off on the spot, and every component behind it would
	// be closed in a timed-out state. Failing before anything is done beats discovering at shutdown
	// that there is no graceful shutdown
	if o.stopTimeout <= 0 {
		return xerror.Newf("xone", "config", "stop budget must be > 0 (0 is not unlimited, it is no wait at all), got=%v", o.stopTimeout)
	}

	// The shutdown signal is taken over before anything else is done, so loading the configuration
	// and initializing are both covered by it. Installed after initialization instead, a SIGTERM
	// arriving during startup (while connecting to the database or Redis, or retrying a Ping) would
	// take the system's default disposition: the process dies on the spot and not one of the
	// resources already built gets to deregister -- the record in the registry and the distributed
	// lock are left for the other side to expire.
	ctx, stopSignals := notifyShutdown(o)
	defer stopSignals()
	printBanner(stderr, isTerminal(stderr))

	// If somebody read the configuration earlier, that one is reused. The configuration belongs to
	// this Run and is handed back however it ends -- a failed load included: otherwise that failure
	// would stay behind, and the next Run in the same process would not get a configuration of its
	// own
	defer config.Reset()

	// A badly written Runnable, or a configuration that cannot be read: not one start hook runs, but
	// the stop phase goes through all the same. started is empty, so only the stop hooks with no
	// start hook before them run -- the documentation says those always run, and returning early
	// would make that true only once startup got far enough
	if err := checkRunnable(r); err != nil {
		return errors.Join(err, stopWithin(o, nil))
	}
	if err := config.Ensure(o.configPath, o.log()); err != nil {
		return errors.Join(err, stopWithin(o, nil))
	}

	debugHooks(hook.Start())
	started, err := runStart(ctx, o)
	switch {
	case ctx.Err() != nil:
		// A shutdown signal during startup: the server is not started, and whatever came up is closed
		// in reverse. Even an error brought back by runStart is not reported upwards -- a cancelled
		// connect is bound to fail, and that is the result of shutting down as asked, not a failure.
		// Reported, it would leave a "startup failed" on the dashboard every time a rolling update
		// hit this window.
		attrs := []any{"ready", len(started)}
		if err != nil {
			attrs = append(attrs, "interrupted_start", err)
		}
		o.log().Info("shutdown signal received during startup, not starting the server", attrs...)
		return stopWithin(o, started)
	case err != nil:
		return errors.Join(err, stopWithin(o, started))
	}

	// Every start hook has run, so a top-level key nobody has read by now is a key nobody wants.
	// Most likely it is misspelled, or the matching integration was never imported -- either way
	// somebody configures something for a while before noticing it has no effect, and the
	// configuration file is the only interface the caller has. There is a third case, reading it too
	// late: a key first read inside Start has likewise not been read by now, and the error has to say
	// where to move it
	if orphan := config.Unclaimed(); len(orphan) > 0 {
		hint := ""
		if slices.Contains(orphan, "XLog") {
			// XLog does not come along with the root package (see the imports), which is easiest to trip
			// over in a program using only the core and the data integrations
			hint = `; XLog needs import _ "github.com/jupiter-eng/xone/xlog" (xgin, xecho and xcron bring it along)`
		}
		return errors.Join(xerror.Newf("xone", "config",
			"config keys %v are not read by anyone: check the spelling, or whether the matching package is imported; "+
				"a key first read after startup (inside Start) is too late to count, read it in main or a BeforeStart hook%s", orphan, hint),
			stopWithin(o, started))
	}

	runErr := make(chan error, 1)
	go func() { runErr <- safe("start", func() error { return r.Start(ctx) }) }()

	var first error
	var serverExited bool
	select {
	case <-ctx.Done(): // the signal has already been logged by notifyShutdown
	case e := <-runErr:
		first, serverExited = e, true
	}

	// The whole shutdown shares this one budget. The server (Stop plus waiting for Start to return)
	// may use only the first 2/3, and the last 1/3 is kept for the stop hooks: a server that refuses
	// to exit cannot eat into it, and a server that exits early leaves the rest to the hooks. With
	// the default of 15s the server's share is 10s, which is the deadline xgin's Stop waits out the
	// requests in flight against
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), o.stopTimeout)
	defer stopCancel()
	serverCtx, serverCancel := context.WithTimeout(stopCtx, o.stopTimeout-o.stopTimeout/3)
	defer serverCancel()
	if s, ok := r.(stopper); ok {
		first = errors.Join(first, stopServer(serverCtx, o, s))
	}

	// The other components are closed only once Start has really returned.
	//
	// Stop returning does not mean the server has stopped cleanly: Stop only has to "make it stop",
	// and whether it waits for the requests being handled is up to each implementation. Closing on
	// without waiting, a request still running would reach a database and a cache that are already
	// closed. This is also the only place Start's error can be collected -- on the signal path it has
	// not been read yet.
	//
	// When the server exited by itself the select above already took runErr, and it must not be taken
	// again here: there is no second value in the channel, and waiting for one would simply burn the
	// server's whole share.
	if !serverExited {
		select {
		case e := <-runErr:
			first = errors.Join(first, e)
		case <-serverCtx.Done():
			// A well-behaved Stop returns at the deadline and Start returns right behind it: with both
			// ready at once select picks at random, and picking this branch would drop Start's error.
			// So take one non-blocking look first
			select {
			case e := <-runErr:
				first = errors.Join(first, e)
			default:
				// The server used up its share without exiting. It is not waited for any longer: what is
				// left belongs to the components -- waiting on would mean failing to close the record in
				// the registry and the distributed lock. A server that refuses to exit should not take
				// every resource down with it
				o.log().Warn("server did not exit within its share of the stop budget, closing the rest",
					"budget", o.stopTimeout)
			}
		}
	}

	return errors.Join(first, runStop(stopCtx, o, started))
}

// stopServer calls the server's Stop before the ctx deadline and stops waiting for it once that
// deadline passes.
//
// Same reasoning as runWithin: Stop receives a ctx but may well not look at it. Called
// synchronously, a Stop that ignores its ctx could hang Run forever and not one stop hook would get
// its turn. A timeout is a warning and not an error: like the timeout waiting for Start to return
// below, it means the server did not stop cleanly, while the remaining components still have to be
// closed.
func stopServer(ctx context.Context, o options, s stopper) error {
	done := make(chan error, 1)
	go func() { done <- safe("stop", func() error { return s.Stop(ctx) }) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	// A short grace period past the deadline: a well-behaved Stop returns at exactly that same
	// deadline (xgin waits out the handlers in flight until the deadline and then returns with
	// "N handler(s) still running"), which happens almost at the same moment as ctx.Done here.
	// Without this grace period select could see Done first, drop the error Stop faithfully reported,
	// and let the process exit with 0 (two e2e shutdown cases hit this)
	select {
	case err := <-done:
		return err
	case <-time.After(stopGrace):
		o.log().Warn("server Stop did not return within its share of the stop budget, closing the rest",
			"budget", o.stopTimeout)
		return nil
	}
}

// stopGrace is how much longer the server's Stop is waited for past its deadline, see stopServer.
// It only catches the result of a "returns at the deadline" Stop, is borrowed from the stop hooks'
// share, and has to be far smaller than hookReserve
const stopGrace = 50 * time.Millisecond

// stopWithin is the shutdown for a failure during startup, which opens a stop budget of its own.
//
// This path has no stopCtx -- the server never came up and that ctx was never built. A budget is
// needed all the same: when startup fails halfway, the few components already built may still
// refuse to close.
func stopWithin(o options, started map[int]bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.stopTimeout)
	defer cancel()
	return runStop(ctx, o, started)
}

// runStart runs every start hook by stage and returns the ones that succeeded (by registration
// number).
//
// What is recorded is each hook, not each package: one package may register several pairs, and a
// stop hook goes by the one start hook it is paired with. See runStop.
func runStart(ctx context.Context, o options) (map[int]bool, error) {
	started := map[int]bool{}
	for _, e := range hook.Start() {
		// A look before each hook: once shutdown has been decided, there is no point connecting to
		// three more databases. The one already running cannot be interrupted -- that is up to it
		// passing the ctx down, which is why the hook signature carries one
		if ctx.Err() != nil {
			o.log().Warn("shutdown signal received, skipping the remaining start hooks", "ready", len(started))
			return started, nil
		}
		// The logger is taken afresh every time: xlog replaces the global default logger inside this
		// very loop, and the hooks after it should use the new one
		o.log().Info("starting", "hook", e.Name)
		if err := safeHook(ctx, e); err != nil {
			return started, xerror.Newf("xone", "start", "%s: %w", e.Name, err)
		}
		started[e.Seq] = true
	}
	return started, nil
}

// runStop runs the stop hooks in reverse order, all of them finishing before the ctx deadline.
//
// A stop hook runs only once the start hook it is paired with has succeeded (the pairing is fixed at
// registration time, see hook.AddStop: the one registered most recently before it in the same
// package). When startup fails halfway the resources behind it were never built, and calling their
// stop hooks would only fail on a pile of zero values -- which is why a stop hook never has to
// handle "not built yet". A stop hook with no start hook before it runs as usual, as it never
// depended on startup to begin with.
//
// Each hook keeps a share back for the ones behind it: hook i may use up to "the deadline minus the
// number of hooks behind it x reserve", where reserve = min(hookReserve, the time left / the number
// of hooks). A stuck hook cannot eat into what is behind it, and whatever the ones in front finish
// early is passed on. So every hook still has time in hand when it begins, instead of carrying an
// already expired ctx straight into a timeout.
//
// One failure does not affect the rest: shutdown closes as much as it can.
func runStop(ctx context.Context, o options, started map[int]bool) error {
	todo := hook.StopAfter(started)
	if len(todo) == 0 {
		return nil
	}

	end, _ := ctx.Deadline() // the caller always gives a deadline: Run's stopCtx, or stopWithin's
	reserve := min(hookReserve, time.Until(end)/time.Duration(len(todo)))

	var errs []error
	for i, e := range todo {
		o.log().Info("stopping", "hook", e.Name)
		hookCtx, cancel := context.WithDeadline(ctx, end.Add(-time.Duration(len(todo)-1-i)*reserve))
		err := runWithin(hookCtx, e)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runWithin runs one stop hook before the ctx deadline and stops waiting for it once that deadline
// passes.
//
// The hook receives a ctx but may well not look at it -- inside may be a third-party Close() that
// takes none. So a goroutine of its own does the waiting. After a timeout that goroutine is left
// hanging where it is, deliberately: abandoning it beats putting every hook behind it, and the
// process itself, in a queue behind one resource that cannot be closed. The process is about to
// exit, and one leaked goroutine has no consequences.
func runWithin(ctx context.Context, e hook.Entry) error {
	done := make(chan error, 1)
	go func() { done <- safeHook(ctx, e) }()

	select {
	case err := <-done:
		if err != nil {
			return xerror.Newf("xone", "stop", "%s: %w", e.Name, err)
		}
		return nil
	case <-ctx.Done():
		return xerror.Newf("xone", "stop", "%s did not finish within its share of the stop budget: %w", e.Name, ctx.Err())
	}
}

// safeHook runs one hook and contains a panic.
//
// One hook blowing up should not take the whole process down -- it should become an ordinary start
// or stop error, giving whatever came up a chance to be closed in reverse.
//
// It returns a plain error rather than an xerror: the caller (runStart / runWithin) wraps it once
// with start / stop, and wrapping here as well would make two layers of xone.
func safeHook(ctx context.Context, e hook.Entry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicked(r)
		}
	}()
	return e.Run(ctx)
}

// safe runs the server's Start / Stop and contains a panic, with op being start or stop.
//
// An error the server returns itself is handed on unchanged: that is the caller's error, and not
// the framework's to wrap.
func safe(op string, f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = xerror.New("xone", op, fmt.Errorf("server %w", panicked(r)))
		}
	}()
	return f()
}

// panicked turns a recovered value into an error.
//
// When what was panicked with is an error itself, %w catches it: with %v it would be left as a
// piece of text and errors.Is / errors.As would stop here.
func panicked(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", r)
}

// shutdownSignals are the signals that trigger a graceful shutdown
var shutdownSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// notifyShutdown takes over the shutdown signals and returns a context that is cancelled when one
// arrives.
//
// What sets it apart from signal.NotifyContext is that it hands the default disposition back after
// the first signal, so that a second one has the system terminate the process directly. That is
// required: registering a signal handler is itself what cancels the system's original "die on
// arrival", while the framework may be stuck inside some third-party call that takes no ctx during
// both initialization and shutdown -- without this escape hatch the process would, in those cases,
// only be collectable with kill -9, and K8s would wait out the whole termination grace period.
//
// The stop returned deregisters on a normal exit; it waits for the goroutine that took over to
// really exit before returning, which guarantees that the signals are back to their default
// disposition once Run returns (the tests that Run repeatedly in one process depend on this).
func notifyShutdown(o options) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, shutdownSignals...)

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case s := <-ch:
			// The default disposition is restored before cancelling: should another signal arrive in
			// between, it is meant to terminate the process directly rather than be collected by a
			// handler nobody is watching any more
			signal.Stop(ch)
			o.log().Info("shutdown signal received, closing gracefully; send it again to terminate now",
				"signal", s.String())
			cancel()
		case <-ctx.Done():
			signal.Stop(ch)
		}
	}()

	return ctx, func() {
		cancel()
		<-done
	}
}

type options struct {
	configPath  string
	stopTimeout time.Duration

	// logger left empty means slog.Default() is taken every time.
	//
	// It has to be deferred until it is used: xlog calls slog.SetDefault only while StageLog is being
	// initialized, and capturing one at the top of Run would leave every framework log afterwards
	// writing to the default logger from before that.
	logger *slog.Logger
}

// log takes the logger to use right now. Without one given explicitly it takes the standard
// library's global default every time, so that once xlog has been initialized the framework's own
// logs follow the new one by themselves.
func (o options) log() *slog.Logger {
	if o.logger != nil {
		return o.logger
	}
	return slog.Default()
}

type Option func(*options)

// WithConfigPath names the configuration file, taking precedence over --config, XONE_CONFIG and the
// conventional paths.
//
// If the configuration was read before Run, it has already been loaded by one of those means, and
// naming another file here makes Run fail outright -- use --config or XONE_CONFIG instead then.
func WithConfigPath(p string) Option { return func(o *options) { o.configPath = p } }

// WithStopTimeout sets the stop budget, 15s by default: this much in total from the start of
// shutdown (a signal arriving, or the server returning by itself) until Run returns, so the
// deployment environment only needs a termination grace period longer than it.
// It must be greater than 0 -- 0 does not mean "unlimited" but "no wait at all", and Run returns an
// error outright.
//
// The framework splits it up internally: the server takes at most the first 2/3 and the rest is kept
// for the stop hooks, each of which keeps a share back for the ones behind it -- neither a server
// that refuses to exit nor a connection pool that cannot be closed can eat into somebody else's
// share. A server whose Stop does not return on the ctx it was given is no longer waited for once
// the server's share has run out.
func WithStopTimeout(d time.Duration) Option { return func(o *options) { o.stopTimeout = d } }

// WithLogger swaps the logger for the framework's own start and stop logs alone, mainly to keep
// tests quiet; business code and the integrations keep writing to slog.Default(). To replace the
// logging backend of the whole process, use xlog.UseHandler.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// MustRun is Run, exiting outright on an error.
func MustRun(r Runnable, opts ...Option) {
	if err := Run(r, opts...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
