package xone

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
)

// ---- Test doubles: no mocks, just ordinary types ----

type recorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.seq = append(r.seq, s); r.mu.Unlock() }
func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.seq, " → ")
}

type closer struct {
	name string
	r    *recorder
	err  error
}

func (c *closer) Close() error { c.r.add("close:" + c.name); return c.err }

// pair is a pair of hooks, standing in for the two an integration registers
type pair struct{ start, stop hook.Entry }

// comp builds a pair of hooks: one note on start, one note on stop
func comp(name string, stage hook.Stage, r *recorder, startErr error) pair {
	return pair{
		start: hook.Entry{Name: name + ".init", Pkg: name, Stage: stage, Run: func(context.Context) error {
			r.add("init:" + name)
			return startErr
		}},
		stop: hook.Entry{Name: name + ".close", Pkg: name, Stage: stage, Run: func(context.Context) error {
			r.add("close:" + name)
			return nil
		}},
	}
}

// comps registers a number of hook pairs on the global board and clears it when the test ends.
//
// They are registered in the order given, each pair start first and stop second -- the same order
// an integration registers them in its init, which is why a stop hook is paired with exactly the
// start hook from its own pair
func comps(t *testing.T, ps ...pair) {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
	for _, p := range ps {
		if p.start.Run != nil {
			hook.AddStart(p.start)
		}
		if p.stop.Run != nil {
			hook.AddStop(p.stop)
		}
	}
}

type server struct {
	r       *recorder
	stopped chan struct{}
	startEr error
}

func newServer(r *recorder) *server { return &server{r: r, stopped: make(chan struct{})} }

func (s *server) Start(context.Context) error {
	s.r.add("start:server")
	if s.startEr != nil {
		return s.startEr
	}
	<-s.stopped
	return nil
}

func (s *server) Stop(context.Context) error {
	s.r.add("stop:server")
	close(s.stopped)
	return nil
}

// signalOnStart wraps r: after Start has been called, it waits for after and then sends itself sig.
//
// A goroutine started before Run that sends at a fixed time will not do: under load (test.sh runs a
// dozen modules in parallel, with -race) Run may not have taken the signals over yet at that moment,
// or may already have returned with an error and handed them back to their default disposition --
// and the default disposition of SIGTERM is killing the whole test process, which is reported as a
// signal: terminated in some entirely unrelated case. Sent from inside Start, the signal is certain
// to land in the window Run has taken over; and if Run fails early it is never sent at all
func signalOnStart(r Runnable, sig syscall.Signal, after time.Duration) Runnable {
	k := signaler{sig: sig, after: after}
	if s, ok := r.(stopper); ok {
		return &signalingServer{signalingRunnable{r, k}, s}
	}
	return &signalingRunnable{r, k} // no Stop out of thin air: a Runnable with only Start keeps only Start
}

type signaler struct {
	sig   syscall.Signal
	after time.Duration
}

type signalingRunnable struct {
	r Runnable
	k signaler
}

func (s *signalingRunnable) Start(ctx context.Context) error {
	go func() { time.Sleep(s.k.after); syscall.Kill(syscall.Getpid(), s.k.sig) }()
	return s.r.Start(ctx)
}

type signalingServer struct {
	signalingRunnable
	s stopper
}

func (s *signalingServer) Stop(ctx context.Context) error { return s.s.Stop(ctx) }

func emptyConf(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(p, []byte(""), 0o600)
	return p
}

// ---- Cases ----

func TestRun_ZeroStopBudgetFailsFast(t *testing.T) {
	// Here 0 does not mean "unlimited" but "no wait at all": Stop receives a context that has already
	// expired, the server is cut off on the spot, and every component behind it is closed in a
	// timed-out state. A misconfiguration like this has to be caught before anything is done
	started := false
	r := &lateRunnable{
		start: func(context.Context) error { started = true; return nil },
		stop:  func(context.Context) error { return nil },
	}
	err := Run(r, WithLogger(quietLogger()), WithStopTimeout(0))
	if err == nil {
		t.Fatal("a stop budget of 0 must fail outright")
	}
	if started {
		t.Error("the server must not have been started before the error")
	}
}

func TestRun_ClosesInReverseOrder(t *testing.T) {
	r := &recorder{}

	comps(t, comp("a", hook.StageClient, r, nil), comp("b", hook.StageClient, r, nil))
	err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))
	if err != nil {
		t.Fatalf("an ordinary shutdown must produce no error: %v", err)
	}

	want := "init:a → init:b → start:server → stop:server → close:b → close:a"
	if got := r.String(); got != want {
		t.Errorf("wrong shutdown order\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_StageDecidesOrderNotRegistration(t *testing.T) {
	// This is the central claim of the whole design: the order init() runs in (that is, the order of
	// registration) does not affect the result
	r := &recorder{}

	// Deliberately registered in the "wrong" order
	comps(t,
		comp("client", hook.StageClient, r, nil),
		comp("trace", hook.StageTelemetry, r, nil),
		comp("log", hook.StageLog, r, nil),
	)
	err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(r.String(), "init:log → init:trace → init:client") {
		t.Errorf("initialization must follow ascending Stage, regardless of registration order, got=%s", r.String())
	}
	if !strings.HasSuffix(r.String(), "close:client → close:trace → close:log") {
		t.Errorf("shutdown must be the strict reverse of initialization, got=%s", r.String())
	}
}

func TestRun_KeepsRegistrationOrderWithinStage(t *testing.T) {
	r := &recorder{}

	comps(t,
		comp("first", hook.StageClient, r, nil),
		comp("second", hook.StageClient, r, nil),
	)
	_ = Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))
	if !strings.HasPrefix(r.String(), "init:first → init:second") {
		t.Errorf("within one stage the registration order must be kept (a stable sort), got=%s", r.String())
	}
}

func TestRun_InitFailureRollsBackInitialized(t *testing.T) {
	r := &recorder{}
	boom := errors.New("cannot connect")

	comps(t,
		comp("ok", hook.StageClient, r, nil),
		comp("bad", hook.StageClient, r, boom),
	)
	err := Run(newServer(r), WithConfigPath(emptyConf(t)))

	if !errors.Is(err, boom) {
		t.Fatalf("the original error of the failed initialization must be returned, got=%v", err)
	}
	if got := r.String(); got != "init:ok → init:bad → close:ok" {
		t.Errorf("on a failure what was initialized must be closed in reverse and no server started, got=%s", got)
	}
}

func TestRun_ServiceStartFails(t *testing.T) {
	r := &recorder{}
	boom := errors.New("port already in use")
	s := newServer(r)
	s.startEr = boom

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(s, WithConfigPath(emptyConf(t)))
	if !errors.Is(err, boom) {
		t.Fatalf("the server start error must be returned, got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("the components must be closed when the server fails to come up, got=%s", r.String())
	}
}

func TestRun_CloseErrorsAggregatedNotSwallowed(t *testing.T) {
	r := &recorder{}
	closeErr := errors.New("cannot close")

	comps(t, pair{
		start: hook.Entry{Name: "stuck.init", Pkg: "stuck", Run: func(context.Context) error { return nil }},
		stop:  hook.Entry{Name: "stuck.close", Pkg: "stuck", Run: func(context.Context) error { return closeErr }},
	})
	err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)))

	if !errors.Is(err, closeErr) {
		t.Fatalf("a close error must be returned, got=%v", err)
	}
}

// demoConf reproduces a caller's configuration block: one struct, with defaults
type demoConf struct {
	Name string `yaml:"Name"`
}

func TestRun_InitSeesFileValuesNotDefaults(t *testing.T) {
	// An integration reads its configuration in a BeforeStart hook. When exactly the framework loads
	// the file is something the caller never has to know -- what is guarded here is that a hook
	// really does read the values from the file
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("Demo:\n  Name: from-yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := demoConf{Name: "default"}
	var seen string
	r := &recorder{}
	s := newServer(r)
	s.startEr = errors.New("stop right away") // this case is only about the startup phase

	comps(t, pair{start: hook.Entry{Name: "demo.init", Pkg: "demo", Run: func(context.Context) error {
		if err := xconfig.Unmarshal("Demo", &cfg); err != nil {
			return err
		}
		seen = cfg.Name
		return nil
	}}})
	_ = Run(s, WithConfigPath(p), WithLogger(quietLogger()))

	if seen != "from-yaml" {
		t.Fatalf("Init must read the values from the configuration file, got=%q", seen)
	}
}

// earlyConf writes a configuration and has it found through XONE_CONFIG, standing in for "reading
// the configuration in main, before Run"
func earlyConf(t *testing.T, yml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	t.Setenv(config.EnvKey, p)
	return p
}

func TestRun_ReadingConfigBeforeRunGetsFileValues(t *testing.T) {
	// What this guards against: reading too early quietly yields zero values, the server comes up
	// with a set of defaults, and nobody notices until somebody finds it connected to the wrong
	// database. Now the first read loads first: what main reads and what takes effect inside Run are
	// one and the same
	earlyConf(t, "Demo:\n  Name: from-yaml\n")

	var early demoConf
	if err := xconfig.Unmarshal("Demo", &early); err != nil {
		t.Fatal(err)
	}
	if early.Name != "from-yaml" {
		t.Fatalf("what is read before Run must be the values from the file, got=%q", early.Name)
	}

	// Run reuses this one: Demo has been claimed and is not reported as a key nobody read
	r := &recorder{}
	s := newServer(r)
	s.startEr = errors.New("stop right away")
	err := Run(s, WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "stop right away") {
		t.Fatalf("a block read early must count as claimed and the server must start as usual, got=%v", err)
	}
}

func TestRun_FailsWhenOtherFileNamedAfterEarlyRead(t *testing.T) {
	// Swapping in another one quietly would leave what main read and what the components read out of
	// step
	earlyConf(t, "Demo:\n  Name: a\n")
	var c demoConf
	if err := xconfig.Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("this must fail and tell the caller to use --config / XONE_CONFIG instead, got=%v", err)
	}
	if strings.Contains(r.String(), "init:") {
		t.Errorf("with the configuration out of step not one hook may run, got=%s", r.String())
	}
}

func TestRun_UnreadConfigBlockIsError(t *testing.T) {
	// Most likely it is misspelled, or the matching package was never imported. Ignored quietly, the
	// caller spends a long time staring at a file that "clearly has it configured"
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("XGrom:\n  DSN: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	s := newServer(r)
	s.startEr = errors.New("server started") // with the check in effect, its turn never comes
	err := Run(s, WithConfigPath(p), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("a top-level key nobody read must fail startup")
	}
	if !strings.Contains(err.Error(), "XGrom") {
		t.Errorf("the error must name the key, got=%v", err)
	}
	// Another common cause is reading too late: a key first read inside Start has not been read by
	// anyone when the startup check runs
	if !strings.Contains(err.Error(), "read it in main or a BeforeStart hook") {
		t.Errorf("the error must say where to read it, got=%v", err)
	}
	if strings.Contains(r.String(), "start:server") {
		t.Errorf("since startup failed, the server must not have been started, got=%s", r.String())
	}
}

func TestRun_ComponentPanicIsIsolated(t *testing.T) {
	r := &recorder{}
	comps(t, pair{start: hook.Entry{Name: "panicky.init", Pkg: "panicky",
		Run: func(context.Context) error { panic("initialization blew up") }}})
	err := Run(newServer(r), WithConfigPath(emptyConf(t)))

	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("a component panic must become an error instead of taking the process down, got=%v", err)
	}
}

func TestRun_MissingExplicitConfigFileIsError(t *testing.T) {
	r := &recorder{}
	// The assertion goes on the error text itself: the temporary directory's path carries the test
	// name, which used to match the wording being asserted on, so the assertion always held
	err := Run(newServer(r), WithConfigPath(filepath.Join(t.TempDir(), "nope.yml")))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a configuration file named explicitly and not found must fail on the spot, got=%v", err)
	}
}

func TestRun_StartsWithDefaultsWhenNoConfigFile(t *testing.T) {
	// The component has to really read its configuration: the fake component here used to read not
	// one line of it, which is why the bug where "with no configuration file every Unmarshal reports
	// being read too early and the server never comes up" went unnoticed for so long
	r := &recorder{}
	chdir(t, t.TempDir()) // nothing at the conventional paths
	os.Args = []string{"svc"}
	config.Reset()
	t.Setenv(config.EnvKey, "")

	cfg := demoConf{Name: "default"}
	reads := pair{start: hook.Entry{Name: "demo.init", Pkg: "demo", Run: func(context.Context) error {
		return xconfig.Unmarshal("Demo", &cfg)
	}}}
	comps(t, comp("a", hook.StageClient, r, nil), reads)
	if err := Run(signalOnStart(newServer(r), syscall.SIGTERM, 120*time.Millisecond), WithLogger(quietLogger())); err != nil {
		t.Fatalf("with no configuration file this must only warn and start with the defaults, got=%v", err)
	}
	if !strings.Contains(r.String(), "init:a") {
		t.Errorf("the components must still be initialized, got=%s", r.String())
	}
	if cfg.Name != "default" {
		t.Errorf("with no configuration file the defaults must be read, got=%q", cfg.Name)
	}
}

// chdir switches the working directory and switches back when the test ends. See the note on the
// helper of the same name in internal/config: testing.T.Chdir needs Go 1.24, and the core module's
// floor is not raised for a test helper.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestRun_WaitsForServiceExitBeforeClosing(t *testing.T) {
	// A regression case. Stop returning does not mean the server has stopped cleanly -- Stop only has
	// to "make it stop", and whether it waits for the requests being handled is up to each
	// implementation. Closing on without waiting, a request still running would reach a database and
	// a cache that are already closed.
	var closedAt, startReturnedAt time.Time
	var mu sync.Mutex

	probe := pair{
		start: hook.Entry{Name: "probe.init", Pkg: "probe", Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "probe.close", Pkg: "probe", Stage: hook.StageClient,
			Run: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				closedAt = time.Now()
				return nil
			}},
	}

	// Stop only sends a signal and returns, while Start keeps running for a while before it exits
	stop := make(chan struct{})
	r := &lateRunnable{
		start: func(context.Context) error {
			<-stop
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			startReturnedAt = time.Now()
			return nil
		},
		stop: func(context.Context) error { close(stop); return nil },
	}

	comps(t, probe)
	if err := Run(signalOnStart(r, syscall.SIGINT, 50*time.Millisecond), WithLogger(quietLogger())); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if startReturnedAt.IsZero() || closedAt.IsZero() {
		t.Fatal("both sides must have been reached")
	}
	if closedAt.Before(startReturnedAt) {
		t.Errorf("the component was closed before the server exited: closed=%v server exited=%v", closedAt, startReturnedAt)
	}
}

func TestRun_ServiceExitErrorIsKept(t *testing.T) {
	// On the signal path Start's return value has never been read before
	wantErr := errors.New("the listener died")
	stop := make(chan struct{})
	r := &lateRunnable{
		start: func(context.Context) error { <-stop; return wantErr },
		stop:  func(context.Context) error { close(stop); return nil },
	}

	err := Run(signalOnStart(r, syscall.SIGINT, 50*time.Millisecond), WithLogger(quietLogger()))
	if !errors.Is(err, wantErr) {
		t.Errorf("the error from the server exiting must be carried out, got=%v", err)
	}
}

type lateRunnable struct {
	start func(context.Context) error
	stop  func(context.Context) error
}

func (l *lateRunnable) Start(ctx context.Context) error { return l.start(ctx) }
func (l *lateRunnable) Stop(ctx context.Context) error  { return l.stop(ctx) }

// startOnly is a Runnable with only Start: the usual shape of a consumer or a scheduled job
type startOnly struct{ r *recorder }

func (s *startOnly) Start(ctx context.Context) error {
	s.r.add("start:job")
	<-ctx.Done()
	s.r.add("done:job")
	return nil
}

func TestRun_StartOnlyRunnableWorks(t *testing.T) {
	// A Runnable that can be stopped through the ctx alone must not be forced to write an empty Stop
	r := &recorder{}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(signalOnStart(&startOnly{r: r}, syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.String(), "init:a → start:job → done:job → close:a"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}
}

func TestFunc_RunEndsWhenFuncReturns_ErrorPassedThrough(t *testing.T) {
	// A one-off task: the hooks build the components, the function returns when its work is done, and
	// Run closes the components in reverse and hands the function's error back
	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	boom := errors.New("migrate failed")
	err := Run(Func(func(context.Context) error { r.add("job"); return boom }),
		WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if !errors.Is(err, boom) {
		t.Fatalf("Run must hand the function's error back, got=%v", err)
	}
	if got, want := r.String(), "init:a → job → close:a"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}
}

func TestFunc_NilPanics(t *testing.T) {
	// Uncaught, a nil would only blow up once every start hook has run, at the moment Start is called
	defer func() {
		if recover() == nil {
			t.Error("Func(nil) must panic")
		}
	}()
	Func(nil)
}

func TestUntilSignal_BlocksUntilSignalThenClosesInReverse(t *testing.T) {
	// A process whose work all happens in hooks: Run stops here once the start hooks are done, and
	// runs the stop hooks only once a signal arrives
	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	start := time.Now()
	if err := Run(signalOnStart(UntilSignal(), syscall.SIGTERM, 120*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("this must block until the signal (120ms), yet it returned after %v", took)
	}
	if got, want := r.String(), "init:a → close:a"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}
}

// wrongStop's Stop is missing its ctx: it compiles, but it is not the signature the framework goes
// by
type wrongStop struct{ started bool }

func (w *wrongStop) Start(context.Context) error { w.started = true; return nil }
func (w *wrongStop) Stop() error                 { return nil }

func TestRun_StopWithWrongSignatureFails(t *testing.T) {
	// Stop is optional, so the compiler does not catch a wrong signature and it simply never runs --
	// the server cannot be stopped when the signal arrives and has to wait out the stop budget. This
	// has to be said plainly before anything is done
	w := &wrongStop{}
	err := Run(w, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "Stop(context.Context) error") {
		t.Fatalf("this must fail and state the signature the framework wants, got=%v", err)
	}
	if w.started {
		t.Error("the server must not have been started before the error")
	}
}

// valueStart has Start on the value and Stop on the pointer: passing a value compiles, but the
// value's method set holds no Stop
type valueStart struct{ started *bool }

func (v valueStart) Start(context.Context) error { *v.started = true; return nil }
func (v *valueStart) Stop(context.Context) error { return nil }

func TestRun_StopOnPointerButValuePassedFails(t *testing.T) {
	// With a value passed in, Stop is not in the method set, the framework cannot call it, and the
	// server cannot be stopped when the signal arrives
	started := false
	err := Run(valueStart{started: &started}, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "pointer receiver") {
		t.Fatalf("this must fail and suggest passing a pointer, got=%v", err)
	}
	if started {
		t.Error("the server must not have been started before the error")
	}
}

func TestRun_NilFails(t *testing.T) {
	// Uncaught, a nil would only blow up once every start hook has run, at the moment Start is called
	if err := Run(nil, WithLogger(quietLogger())); err == nil {
		t.Fatal("no Runnable must be an error")
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// syscallSelfInterrupt sends itself a SIGINT, standing in for a shutdown signal arriving
func syscallSelfInterrupt(t *testing.T) {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Error(err)
		return
	}
	if err := p.Signal(syscall.SIGINT); err != nil {
		t.Error(err)
	}
}

func TestRun_ReturnsWhenServiceExitsOnItsOwn(t *testing.T) {
	// A regression case. When the server exits by itself the select above has already taken runErr,
	// and taking it again would simply burn the whole stop budget -- a bug whose only symptom is
	// "slow".
	r := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	if err := Run(r, WithLogger(quietLogger()), WithStopTimeout(5*time.Second)); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a server exiting by itself must return at once, it took %v", elapsed)
	}
}

// ---- A shutdown signal during startup ----

func TestRun_SignalDuringInitSkipsServiceStart(t *testing.T) {
	// The signals have to be taken over before initialization. Installed after it instead, a SIGTERM
	// during the seconds spent connecting to the database, connecting to Redis and retrying a Ping
	// takes the system's default disposition -- the process dies on the spot and not one of the
	// resources already built gets to deregister (the record in the registry and the distributed lock
	// are left for the other side to expire)
	r := &recorder{}
	started := false

	slow := pair{
		start: hook.Entry{Name: "slow.init", Pkg: "slow", Stage: hook.StageClient,
			Run: func(ctx context.Context) error {
				r.add("init:slow")
				syscallSelfInterrupt(t) // a shutdown signal arrives mid-startup
				select {
				case <-ctx.Done(): // the hook passed the ctx down itself, so it can give up on the spot
					r.add("slow interrupted")
				case <-time.After(3 * time.Second):
					t.Error("the hook's ctx was not cancelled")
				}
				return nil
			}},
		stop: hook.Entry{Name: "slow.close", Pkg: "slow", Stage: hook.StageClient,
			Run: func(context.Context) error { r.add("close:slow"); return nil }},
	}
	after := comp("later", hook.StageServer, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { started = true; return nil },
		stop:  func(context.Context) error { return nil },
	}

	comps(t, slow, after)
	if err := Run(srv, WithLogger(quietLogger())); err != nil {
		t.Fatalf("shutting down on a signal is not a failure and must not be reported as one: %v", err)
	}
	if started {
		t.Error("the shutdown signal arrived during initialization, so the server must not be started")
	}
	got := r.String()
	if !strings.Contains(got, "slow interrupted") {
		t.Errorf("a component must see the shutdown signal through its ctx, got=%s", got)
	}
	if strings.Contains(got, "init:later") {
		t.Errorf("no remaining component may be initialized after the shutdown signal, got=%s", got)
	}
	if !strings.Contains(got, "close:slow") {
		t.Errorf("the components already built must still be closed cleanly in reverse, got=%s", got)
	}
}

func TestRun_InitInterruptedBySignalIsNotFailure(t *testing.T) {
	// A cancelled connect is bound to fail. Reported as a failure, it would leave a "startup failed"
	// on the dashboard every time a rolling update hit this window, while it is in fact shutting down
	// as asked
	r := &recorder{}
	broken := pair{start: hook.Entry{Name: "unreachable-db.init", Pkg: "unreachable-db", Stage: hook.StageClient,
		Run: func(ctx context.Context) error {
			syscallSelfInterrupt(t)
			<-ctx.Done()
			return ctx.Err() // the connect was cancelled, and the error is reported faithfully
		}}}
	before := comp("earlier", hook.StageLog, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { t.Error("the server must not be started"); return nil },
		stop:  func(context.Context) error { return nil },
	}

	comps(t, before, broken)
	if err := Run(srv, WithLogger(quietLogger())); err != nil {
		t.Fatalf("shutting down on a signal must not end in an error: %v", err)
	}
	if got := r.String(); !strings.Contains(got, "close:earlier") {
		t.Errorf("the component that came up earlier must still be closed, got=%s", got)
	}
}

// stuckChildEnv, when set, has the test process play the "child process stuck in initialization"
const stuckChildEnv = "XONE_TEST_STUCK_CHILD"

func TestRun_SecondSignalKillsWhenStuck(t *testing.T) {
	if os.Getenv(stuckChildEnv) == "1" {
		runStuckChild()
		return
	}

	// Registering a signal handler is itself what cancels the system's original "die on arrival".
	// So once the framework is stuck inside some third-party call that takes no ctx (stood in for
	// here by time.Sleep), a signal only goes into a ctx nobody is watching -- the process becomes
	// collectable with kill -9 alone and K8s waits out the whole termination grace period. The
	// default disposition is handed back after the first signal, which is what gives the second one
	// somewhere to go.
	// os.Executable rather than os.Args[0]: the tests about locating the configuration file rewrite
	// os.Args, and running after them os.Args[0] is already the fake program name one of them made
	// up
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestRun_SecondSignalKillsWhenStuck$")
	cmd.Env = append(os.Environ(), stuckChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitFor(t, stdout, "STUCK")

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // give the goroutine that took over time to hand the default disposition back
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("the child process must be terminated by the signal, got=%v", err)
		}
		st, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !st.Signaled() || st.Signal() != syscall.SIGTERM {
			t.Errorf("this must end as \"terminated by SIGTERM\" (exit code 143), got=%v", ee)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal failed to terminate the stuck child process -- only kill -9 is left")
	}
}

// runStuckChild starts a process that is stuck in initialization and takes no ctx
func runStuckChild() {
	stuck := pair{start: hook.Entry{Name: "stuck.init", Pkg: "stuck", Stage: hook.StageClient,
		Run: func(context.Context) error {
			fmt.Println("STUCK")
			os.Stdout.Sync()
			time.Sleep(60 * time.Second) // stands in for a third-party connect with no timeout
			return nil
		}}}
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { return nil },
	}
	hook.AddStart(stuck.start) // the board in the child process is empty to begin with, nothing to clear
	_ = Run(srv, WithLogger(quietLogger()))
}

// waitFor reads the child process's output until marker appears
func waitFor(t *testing.T, r io.Reader, marker string) {
	t.Helper()
	found := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if strings.Contains(sc.Text(), marker) {
				close(found)
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(10 * time.Second):
		t.Fatalf("the child process never printed %q", marker)
	}
}

// ---- The stop budget ----

func TestShutdown_StuckComponentDoesNotBlockOthers(t *testing.T) {
	// WithStopTimeout is "the budget for the whole shutdown", while io.Closer.Close() takes no ctx.
	// Not watching it means there is no upper bound at all -- one connection pool that cannot be
	// closed hangs the whole process alongside it until the deployment environment sends SIGKILL
	r := &recorder{}
	stuck := pair{
		start: hook.Entry{Name: "unclosable.init", Pkg: "unclosable", Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "unclosable.close", Pkg: "unclosable", Stage: hook.StageClient,
			Run: func(context.Context) error { select {} }},
	}
	after := comp("closed-first", hook.StageServer, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	comps(t, stuck, after)
	err := Run(srv, WithLogger(quietLogger()), WithStopTimeout(300*time.Millisecond))
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("a stuck Close held up the whole shutdown, elapsed=%v", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "unclosable") {
		t.Errorf("which component failed to close must be reported faithfully, got=%v", err)
	}
	// The stuck one is closed later, and what comes before it must still be closed
	if got := r.String(); !strings.Contains(got, "close:closed-first") {
		t.Errorf("a stuck component must not hold up the others, got=%s", got)
	}
}

func TestShutdown_ShutdownAfterInitFailureIsBudgeted(t *testing.T) {
	// This path has no stopCtx yet, while the few components already built may still refuse to close
	stuck := pair{
		start: hook.Entry{Name: "unclosable.init", Pkg: "unclosable", Stage: hook.StageLog,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "unclosable.close", Pkg: "unclosable", Stage: hook.StageLog,
			Run: func(context.Context) error { select {} }},
	}
	boom := pair{start: hook.Entry{Name: "fails-to-start.init", Pkg: "fails-to-start", Stage: hook.StageClient,
		Run: func(context.Context) error { return errors.New("cannot start") }}}

	srv := &lateRunnable{
		start: func(context.Context) error { t.Error("the server must not be started"); return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	comps(t, stuck, boom)
	err := Run(srv, WithLogger(quietLogger()), WithStopTimeout(300*time.Millisecond))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the shutdown after a failed startup had no budget, elapsed=%v", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("the reason initialization failed must be carried out, got=%v", err)
	}
}

// stopSlack is the slack left for scheduling on top of the stop budget. It has to be smaller than
// the budget itself: an "over budget" bug usually means a whole extra budget was taken, and only a
// slack smaller than the budget tells the two apart
const stopSlack = 400 * time.Millisecond

// stuckComp is a component that cannot be closed: its stop hook takes no ctx and never returns
func stuckComp(name string) pair {
	return pair{
		start: hook.Entry{Name: name + ".init", Pkg: name, Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: name + ".close", Pkg: name, Stage: hook.StageClient,
			Run: func(context.Context) error { select {} }},
	}
}

// flushComp is the logging closed last: its stop hook takes 1ms and leaves a note when done
func flushComp(r *recorder) pair {
	return pair{
		start: hook.Entry{Name: "logging.init", Pkg: "logging", Stage: hook.StageLog,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "logging.close", Pkg: "logging", Stage: hook.StageLog,
			Run: func(context.Context) error { time.Sleep(time.Millisecond); r.add("flush:logging"); return nil }},
	}
}

func TestShutdown_StuckHookCannotEatLaterHooksShare(t *testing.T) {
	// If one stuck hook could eat the whole budget, every hook behind it would be out of time the
	// moment it began, without even 1ms to finish its work. Logging suffers most: it is closed last
	// and has to write out every other component's shutdown log, yet it would be reported as "did
	// not finish" every time.
	// Two stuck ones back to back: every hook whose turn has not come needs a share of its own,
	// rather than all of them fighting over what is left
	r := &recorder{}
	srv := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	budget := 800 * time.Millisecond
	start := time.Now()
	comps(t, stuckComp("stuck-one"), stuckComp("stuck-two"), flushComp(r))
	err := Run(srv, WithLogger(quietLogger()), WithStopTimeout(budget))
	elapsed := time.Since(start)

	if got := r.String(); got != "flush:logging" {
		t.Errorf("the logging hook behind the stuck ones must run to completion, got=%q", got)
	}
	if err == nil || !strings.Contains(err.Error(), "stuck-one") || !strings.Contains(err.Error(), "stuck-two") {
		t.Errorf("both stuck hooks must be reported faithfully, got=%v", err)
	}
	if err != nil && strings.Contains(err.Error(), "logging.close") {
		t.Errorf("a hook that finished in time must not be reported as a timeout, got=%v", err)
	}
	if limit := budget + stopSlack; elapsed > limit {
		t.Errorf("shutdown took longer than the stop budget, elapsed=%v limit=%v", elapsed, limit)
	}
}

func TestShutdown_TotalTimeWithinStopBudget(t *testing.T) {
	// WithStopTimeout is the one number the caller has to go by: this long at worst, from the start
	// of shutdown until Run returns. Even with the worst case assembled -- a server that refuses to
	// exit and a connection pool that cannot be closed -- it must not be exceeded, and the logging
	// closed last must still run to completion: the server may use only the first share and cannot
	// eat into the hooks'
	r := &recorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); <-release; return nil },
		stop:  func(context.Context) error { return nil },
	}

	const signalAt = 50 * time.Millisecond
	budget := 800 * time.Millisecond
	start := time.Now()
	comps(t, stuckComp("unclosable"), flushComp(r))
	err := Run(signalOnStart(srv, syscall.SIGINT, signalAt), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()), WithStopTimeout(budget))
	elapsed := time.Since(start)

	if got := r.String(); got != "flush:logging" {
		t.Errorf("a server refusing to exit must not eat the hooks' share, and the logging hook must run to completion, got=%q", got)
	}
	if err == nil || !strings.Contains(err.Error(), "unclosable") {
		t.Errorf("a component that cannot be closed must be reported faithfully, got=%v", err)
	}
	// At most one budget from the signal onwards (a server refusing to exit used to let the
	// components take another one, making two)
	if limit := signalAt + budget + stopSlack; elapsed > limit {
		t.Errorf("shutdown took longer than the stop budget, elapsed=%v limit=%v", elapsed, limit)
	}
}

// entry builds a hook: it leaves a note when it runs and returns err. The part of the name before
// the dot is its package
func entry(r *recorder, name string, err error) hook.Entry {
	pkg, _, _ := strings.Cut(name, ".")
	return hook.Entry{Name: name, Pkg: pkg, Stage: hook.StageClient,
		Run: func(context.Context) error { r.add(name); return err }}
}

func TestRun_StopHookPairsOnlyWithItsStartHook(t *testing.T) {
	// Registered together makes a pair. One package registers two pairs in turn, and when the second
	// pair's start fails: the first is closed as usual -- its resource was built, and not closing it
	// leaks; the second is not closed -- it was never built at all.
	// Pairing by package would force a choice between the two: either both are closed (and the second
	// has to handle "not built yet"), or neither is (and the first leaks)
	r := &recorder{}
	boom := errors.New("the second pair cannot be built")
	t.Cleanup(hook.Reset)
	hook.Reset()
	hook.AddStart(entry(r, "twopairs.openA", nil))
	hook.AddStop(entry(r, "twopairs.closeA", nil))
	hook.AddStart(entry(r, "twopairs.openB", boom))
	hook.AddStop(entry(r, "twopairs.closeB", nil))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if !errors.Is(err, boom) {
		t.Fatalf("a failed start hook must return its error, got=%v", err)
	}
	if got, want := r.String(), "twopairs.openA → twopairs.openB → twopairs.closeA"; got != want {
		t.Errorf("only the pair that was built may be closed\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_StopHookPairsWithLaterOfTwoStartHooks(t *testing.T) {
	// initA, initB and close are registered in turn: close is paired with initB, the nearest one
	// before it. initB fails, the resource is only half built, and close must not be called -- it
	// never has to handle "half built".
	// What initA built is left unclosed, which is the price of splitting two steps into two start
	// hooks: either merge them into one, or give initA a stop hook of its own
	r := &recorder{}
	t.Cleanup(hook.Reset)
	hook.Reset()
	hook.AddStart(entry(r, "halfbuilt.initA", nil))
	hook.AddStart(entry(r, "halfbuilt.initB", errors.New("the second step failed")))
	hook.AddStop(entry(r, "halfbuilt.close", nil))
	hook.AddStop(entry(r, "logging.close", nil))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("a failed start hook must return an error")
	}
	// logging.close has no start hook of its own package before it, does not depend on startup, and
	// runs as usual
	if got, want := r.String(), "halfbuilt.initA → halfbuilt.initB → logging.close"; got != want {
		t.Errorf("a stop hook must not run once its paired start hook has failed\n got=%s\nwant=%s", got, want)
	}
}

// earlyFailure registers one pair of hooks and one stop hook that does not depend on startup, for
// the cases that fail before startup
func earlyFailure(t *testing.T, r *recorder) {
	t.Helper()
	t.Cleanup(hook.Reset)
	hook.Reset()
	hook.AddStart(entry(r, "db.open", nil))
	hook.AddStop(entry(r, "db.close", nil))
	hook.AddStop(entry(r, "logging.flush", nil))
}

func TestRun_RunnableInvalid_UnpairedStopHooksStillRun(t *testing.T) {
	// The documentation says a stop hook with no start hook before it always runs. A rejected
	// Runnable used to return outright, so a hook like the logging flush never ran once and the few
	// lines in the buffer were lost
	r := &recorder{}
	earlyFailure(t, r)
	err := Run(&wrongStop{}, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "Stop(context.Context) error") {
		t.Fatalf("the problem with the Runnable must still be reported, got=%v", err)
	}
	if got, want := r.String(), "logging.flush"; got != want {
		t.Errorf("only the stop hook that does not depend on startup may run\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_ConfigLoadFails_UnpairedStopHooksStillRun(t *testing.T) {
	r := &recorder{}
	earlyFailure(t, r)
	err := Run(newServer(r), WithConfigPath(filepath.Join(t.TempDir(), "nope.yml")), WithLogger(quietLogger()))

	if !xerror.Is(err, "xconfig") {
		t.Fatalf("the problem with the configuration must still be reported, got=%v", err)
	}
	if got, want := r.String(), "logging.flush"; got != want {
		t.Errorf("only the stop hook that does not depend on startup may run\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_HookPanicWrappedOnceWithStandardOp(t *testing.T) {
	// One xerror per module boundary: wrapping a panic once at the hook layer and again at the start
	// layer would nest the text into xone start failed, err=[... xone hook failed, err=[...]].
	// When what was panicked with is an error, %w has to catch it, or the caller cannot tell what the
	// root cause was
	cause := errors.New("the connection pool blew up")
	comps(t, pair{start: hook.Entry{Name: "panicky.init", Pkg: "panicky",
		Run: func(context.Context) error { panic(cause) }}})
	err := Run(newServer(&recorder{}), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("a panic must become an error")
	}
	if n := strings.Count(err.Error(), "xone "); n != 1 {
		t.Errorf("there must be exactly one layer of xone error, got %d: %v", n, err)
	}
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Op != "start" {
		t.Errorf("a panic in a start hook must be reported on start, got=%v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("the error panicked with must still be in the chain, got=%v", err)
	}
}

func TestRun_ServicePanicReportedOnMatchingOp(t *testing.T) {
	// op comes from the table in CLAUDE.md alone: a server blowing up on start is start, on stop it
	// is stop
	for _, tc := range []struct {
		name string
		srv  *lateRunnable
		op   string
	}{
		{"Start", &lateRunnable{
			start: func(context.Context) error { panic("Start blew up") },
			stop:  func(context.Context) error { return nil },
		}, "start"},
		{"Stop", &lateRunnable{
			start: func(context.Context) error { return nil },
			stop:  func(context.Context) error { panic("Stop blew up") },
		}, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(tc.srv, WithLogger(quietLogger()))
			var xe *xerror.Error
			if !errors.As(err, &xe) || xe.Op != tc.op {
				t.Errorf("this must be reported on %s, got=%v", tc.op, err)
			}
		})
	}
}

func TestRun_ErrorTellsWhoReportedIt(t *testing.T) {
	// This is the whole point of settling on xerror: the caller holding one error can ask both "who
	// reported it in the end" and "who is involved in the chain", without having to match a string
	// prefix in the error message
	boom := xerror.Newf("xgorm", "connect", "cannot reach %s: %w", "127.0.0.1:5432", errors.New("connection refused"))
	r := &recorder{}

	comps(t, comp("XGorm", hook.StageClient, r, boom))
	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("a failed initialization must return an error")
	}
	// The outermost layer is the framework: it is what handed this error over in the end
	if got := xerror.Module(err); got != "xone" {
		t.Errorf("the outermost layer must be xone, got=%q", got)
	}
	// Yet the root cause can still be asked for
	if !xerror.Is(err, "xgorm") {
		t.Errorf("it must be possible to tell the root cause is in xgorm, err=%v", err)
	}
	if xerror.Is(err, "xredis") {
		t.Errorf("a module that was not involved must not be recognized, err=%v", err)
	}
	// The original error chain is unbroken too
	if !errors.Is(err, boom) {
		t.Errorf("the original error must still be in the chain, err=%v", err)
	}
}

// ---- Signals ----

func TestRun_ListensForBothExitSignals(t *testing.T) {
	// K8s sends SIGTERM and Ctrl-C sends SIGINT. Miss either one and a process on that path is simply
	// killed by the system, with no graceful shutdown at all
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			r := &recorder{}

			comps(t, comp("a", hook.StageClient, r, nil))
			if err := Run(signalOnStart(newServer(r), sig, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
				t.Fatalf("shutting down on a signal is not a failure and must not be reported as one: %v", err)
			}
			if got := r.String(); !strings.Contains(got, "stop:server") || !strings.Contains(got, "close:a") {
				t.Errorf("receiving %v must go through the whole graceful shutdown, got=%s", sig, got)
			}
		})
	}
}

// blockingHandler is a slog.Handler that stops on seeing the msg it was given, notifies the outside
// and waits to be released. It turns "the framework is right now at this step" into a definite
// moment
type blockingHandler struct {
	slog.Handler
	on      string
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu   sync.Mutex
	msgs []string
}

func (h *blockingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()

	if r.Message == h.on {
		h.once.Do(func() {
			close(h.entered)
			<-h.release
		})
	}
	return h.Handler.Handle(ctx, r)
}

func (h *blockingHandler) saw(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, msg) {
			return true
		}
	}
	return false
}

func TestRun_SignalDuringConfigLoadCounts(t *testing.T) {
	// The signals have to be taken over before the configuration is read. Installed after it instead,
	// a SIGTERM in that window takes the system's default disposition -- the process dies on the
	// spot. With a large configuration file, or one importing several others, that window is not
	// short.
	//
	// Blocking on the "loading config" log line and then sending the signal turns that window into a
	// definite moment
	h := &blockingHandler{
		Handler: slog.NewTextHandler(io.Discard, nil),
		on:      "loading config",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	r := &recorder{}
	done := make(chan error, 1)
	go func() {
		comps(t, comp("a", hook.StageClient, r, nil))
		done <- Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(slog.New(h)))
	}()

	<-h.entered             // Run is right now stopped at the step that reads the configuration
	syscallSelfInterrupt(t) // the signal lands inside that window
	close(h.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutting down on a signal is not a failure and must not be reported as one: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a signal while the configuration was being read was not caught, and Run did not exit")
	}

	// The assertion goes on "the framework caught the signal", not on "did the server come up" or
	// "were the components closed": whether the cancellation beats runStart's check is a pointless
	// race. The promise here is that the process does not take the system's default disposition in
	// that window -- had it really not been caught, the test process would have died right here and
	// never reached the line below
	if !h.saw("shutdown signal received") {
		t.Errorf("a signal while the configuration is read must be caught by the framework, logs=%v", h.msgs)
	}
}

func TestRun_CanRunAgainAfterReturn(t *testing.T) {
	// Run has to deregister the signals cleanly when it returns. Without that, the second Run's
	// handler hangs off a channel nobody reads and the signal is eaten by what the first one left
	// behind -- the symptom being "the second one will not stop no matter what". Every test that Runs
	// repeatedly in one process depends on this
	for i := 1; i <= 2; i++ {
		r := &recorder{}

		comps(t, comp("a", hook.StageClient, r, nil))
		if err := Run(signalOnStart(newServer(r), syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
			t.Fatalf("Run %d failed: %v", i, err)
		}
		if !strings.Contains(r.String(), "stop:server") {
			t.Fatalf("run %d failed to stop on the signal, got=%s", i, r.String())
		}
	}
}

func TestRun_NextRunReadsOwnConfigAfterLoadFailure(t *testing.T) {
	// The configuration belongs to one Run, a failed one included: a failed load left behind in the
	// package would block the next Run in the same process from reading one of its own
	bad := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(bad, []byte("Demo: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(newServer(&recorder{}), WithConfigPath(bad), WithLogger(quietLogger())); err == nil {
		t.Fatal("a malformed configuration must make Run fail")
	}

	s := newServer(&recorder{})
	s.startEr = errors.New("stop right away")
	err := Run(s, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if err == nil || !strings.Contains(err.Error(), "stop right away") {
		t.Fatalf("the next Run must read a configuration of its own and start as usual, got=%v", err)
	}
}

// ---- The ctx during shutdown ----

func TestRun_StopCtxDoesNotInheritCancellation(t *testing.T) {
	// Once the signal has arrived the only thing left to do is shut down gracefully, and that rests
	// entirely on Stop still being able to work. Reusing the cancelled ctx would have every shutdown
	// step refused the moment it begins -- the requests in flight unfinished and the record in the
	// registry not deregistered, which is no graceful shutdown at all
	var serverStopErr, hookStopErr error
	var serverHasDeadline, hookHasDeadline bool

	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop: func(ctx context.Context) error {
			serverStopErr = ctx.Err()
			_, serverHasDeadline = ctx.Deadline()
			return nil
		},
	}
	probe := pair{
		start: hook.Entry{Name: "probe.init", Pkg: "probe", Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "probe.close", Pkg: "probe", Run: func(ctx context.Context) error {
			hookStopErr = ctx.Err()
			_, hookHasDeadline = ctx.Deadline()
			return nil
		}},
	}

	comps(t, probe)
	if err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatalf("shutting down on a signal must not be reported as an error: %v", err)
	}

	if serverStopErr != nil {
		t.Errorf("the ctx Stop receives must not be a cancelled one, got=%v", serverStopErr)
	}
	if hookStopErr != nil {
		t.Errorf("the ctx a stop hook receives must not be a cancelled one, got=%v", hookStopErr)
	}
	// The cancellation is not inherited, but the budget has to be -- otherwise one resource that
	// cannot be closed hangs the process until SIGKILL
	if !serverHasDeadline || !hookHasDeadline {
		t.Errorf("the ctx during shutdown must carry the stop budget, server=%v hook=%v", serverHasDeadline, hookHasDeadline)
	}
}

// ---- Containing panics ----

func TestRun_ServiceStartPanicIsIsolated(t *testing.T) {
	// One panic taking the process down would leave not one of the resources already built closed
	r := &recorder{}
	srv := &lateRunnable{
		start: func(context.Context) error { panic("Start blew up") },
		stop:  func(context.Context) error { return nil },
	}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(srv, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("a panic in Start must become an error, got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("the server blew up and the components must still be closed, got=%s", r.String())
	}
}

func TestRun_ServiceStopPanicIsIsolated(t *testing.T) {
	r := &recorder{}
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { panic("Stop blew up") },
	}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("a panic in Stop must become an error, got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("Stop blew up and the other components must still be closed, got=%s", r.String())
	}
}

func TestRun_StopHookPanicDoesNotAbortOthers(t *testing.T) {
	// Shutdown closes as much as it can. Stopping at the first hook that blows up would leak every
	// connection pool behind it
	r := &recorder{}
	boom := pair{
		start: hook.Entry{Name: "boom.init", Pkg: "boom", Stage: hook.StageClient,
			Run: func(context.Context) error { return nil }},
		stop: hook.Entry{Name: "boom.close", Pkg: "boom", Stage: hook.StageClient,
			Run: func(context.Context) error { panic("blew up while closing") }},
	}
	// A lower stage = closed later, so it comes after boom
	last := comp("closed-last", hook.StageLog, r, nil)

	comps(t, boom, last)
	err := Run(signalOnStart(newServer(r), syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("a panic in a stop hook must become an error, got=%v", err)
	}
	if !strings.Contains(r.String(), "close:closed-last") {
		t.Errorf("one hook blowing up must not hold up the ones behind it, got=%s", r.String())
	}
}

// ---- The server exiting ----

func TestRun_ServiceStopErrorIsAggregated(t *testing.T) {
	r := &recorder{}
	stopErr := errors.New("the graceful shutdown did not finish")
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { return stopErr },
	}

	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))

	if !errors.Is(err, stopErr) {
		t.Fatalf("the error from Stop must be carried out, got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("an error from Stop must not hold up closing the components, got=%s", r.String())
	}
}

func TestRun_ClosesOthersWhenServiceWontExit(t *testing.T) {
	// Stop returning is not Start returning. Waiting for Start is right (a request still being
	// handled would reach a connection pool that is already closed), but it cannot be waited for
	// forever -- once the budget runs out things have to move on, or one server that refuses to exit
	// hangs the whole process until SIGKILL
	r := &recorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); <-release; return nil },
		stop:  func(context.Context) error { return nil },
	}

	var logbuf bytes.Buffer
	start := time.Now()
	comps(t, comp("a", hook.StageClient, r, nil))
	_ = Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)),
		WithLogger(slog.New(slog.NewTextHandler(&logbuf, nil))),
		WithStopTimeout(300*time.Millisecond))
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("a server refusing to exit held up the whole shutdown, elapsed=%v", elapsed)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("the components must be closed even when the server never exits, got=%s", r.String())
	}
	if !strings.Contains(logbuf.String(), "did not exit within its share of the stop budget") {
		t.Errorf("a warning saying so plainly must be left behind, logs=\n%s", logbuf.String())
	}
}

func TestRun_StopIgnoringCtxDoesNotHangExit(t *testing.T) {
	// Stop receives a ctx, but inside may be a third-party call that takes none. Called
	// synchronously, Run would never return and not one stop hook would get its turn
	r := &recorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { <-release; return nil },
	}

	var logbuf bytes.Buffer
	comps(t, comp("a", hook.StageClient, r, nil))
	done := make(chan error, 1)
	go func() {
		done <- Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)),
			WithLogger(slog.New(slog.NewTextHandler(&logbuf, nil))),
			WithStopTimeout(300*time.Millisecond))
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a stuck Stop hung the whole shutdown")
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("the components must be closed even with Stop stuck, got=%s", r.String())
	}
	if !strings.Contains(logbuf.String(), "server Stop did not return within its share of the stop budget") {
		t.Errorf("a warning saying so plainly must be left behind, logs=\n%s", logbuf.String())
	}
}

func TestRun_DeadlineAwareStopErrorIsKept(t *testing.T) {
	// A well-behaved Stop returns at exactly the deadline: xgin waits out the handlers in flight
	// until that moment and comes back with "N handler(s) still running". It happens almost at the
	// same moment as the framework's own timeout, and were the framework to see the timeout first,
	// this error would be lost and the process would exit with 0 (e2e hit this).
	// Here it returns 5ms past the deadline, landing reliably inside that grace period
	boom := errors.New("2 handler(s) still running")
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop: func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(5 * time.Millisecond)
			return boom
		},
	}
	comps(t, comp("a", hook.StageClient, &recorder{}, nil))
	err := Run(signalOnStart(srv, syscall.SIGINT, 50*time.Millisecond), WithConfigPath(emptyConf(t)), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithStopTimeout(300*time.Millisecond))
	if !errors.Is(err, boom) {
		t.Errorf("the error Stop brings back must be reported faithfully, got=%v", err)
	}
}

// ---- Configuration ----

func TestRun_InvalidConfigRunsNoHooks(t *testing.T) {
	// The configuration is the only interface the caller has. Going on to open connections with one
	// that cannot be read would report a pile of connection errors that say nothing about the root
	// cause
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("Demo:\n  Addr: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	comps(t, comp("a", hook.StageClient, r, nil))
	err := Run(newServer(r), WithConfigPath(p), WithLogger(quietLogger()))

	if err == nil {
		t.Fatal("a configuration that cannot be parsed must fail startup")
	}
	if got := r.String(); got != "" {
		t.Errorf("with the configuration unreadable not one hook may run, got=%s", got)
	}
}

// ---- Options and entry points ----

func TestRun_StopBudgetMustBePositive(t *testing.T) {
	// 0 does not mean "unlimited" but "no wait at all": Stop receives a context that has already
	// expired and the server is cut off on the spot. The same goes for a negative value, which looks
	// more like something computed than something written down
	for _, d := range []time.Duration{0, -time.Second} {
		r := &recorder{}
		started := false
		lr := &lateRunnable{
			start: func(context.Context) error { started = true; return nil },
			stop:  func(context.Context) error { return nil },
		}
		err := Run(lr, WithLogger(quietLogger()), WithStopTimeout(d))
		if err == nil {
			t.Errorf("a stop budget of %v must fail outright", d)
		}
		if started {
			t.Errorf("no server may be started with an invalid budget, budget=%v", d)
		}
		_ = r
	}
}

func TestMustRun_ReturnsOnSuccess(t *testing.T) {
	r := &recorder{}
	srv := &lateRunnable{
		start: func(context.Context) error { r.add("start"); return nil },
		stop:  func(context.Context) error { return nil },
	}
	MustRun(srv, WithConfigPath(emptyConf(t)), WithLogger(quietLogger()))
	if !strings.Contains(r.String(), "start") {
		t.Errorf("on success this must run through as usual, got=%s", r.String())
	}
}

const mustRunChildEnv = "XONE_TEST_MUSTRUN_CHILD"

func TestMustRun_ExitsWithCode1OnError(t *testing.T) {
	if os.Getenv(mustRunChildEnv) == "1" {
		// A stop budget of 0 is the easiest startup failure to produce
		MustRun(newServer(&recorder{}), WithStopTimeout(0))
		return
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestMustRun_ExitsWithCode1OnError$")
	cmd.Env = append(os.Environ(), mustRunChildEnv+"=1")
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("MustRun must end with a non-zero exit code on an error, got=%v", err)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("the exit code must be 1, got=%d", ee.ExitCode())
	}
	if !strings.Contains(string(out), "stop budget") {
		t.Errorf("the reason for the failure must be printed to stderr, got=%s", out)
	}
}

// ---- The framework's own logs ----

func TestRun_FrameworkLogsFollowGlobalLoggerSwappedByHook(t *testing.T) {
	// xlog calls slog.SetDefault inside a start hook. Capturing the logger once at the top of Run
	// would leave every framework log afterwards writing to the old one -- the server comes up, and
	// not one line about how far initialization got is visible
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	var buf bytes.Buffer
	swap := pair{start: hook.Entry{Name: "swap.init", Pkg: "swap", Stage: hook.StageLog,
		Run: func(context.Context) error {
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			return nil
		}}}
	after := comp("later", hook.StageClient, &recorder{}, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}
	// No WithLogger: this takes the "slog.Default() afresh every time" path
	comps(t, swap, after)
	if err := Run(srv, WithConfigPath(emptyConf(t))); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "later.init") {
		t.Errorf("framework logs after the global logger was swapped must go to the new one, got=\n%s", buf.String())
	}
}

func TestRun_StartErrorKeptWhenStopReturnsAtDeadline(t *testing.T) {
	// A regression case. A well-behaved Stop returns at the deadline, and Start returns with an error
	// right behind it: at the "wait for Start to return" step, runErr and serverCtx.Done() are ready
	// at the same time. select picks one at random, and picking Done loses Start's error and prints a
	// pointless "the server did not exit in time" warning on top.
	// Which of the two is ready first is up to the scheduler, so this runs many times: the chance of
	// picking wrong is one in two, and dodging it twenty times in a row is impossible
	for i := range 20 {
		wantErr := fmt.Errorf("serve failed #%d", i)
		stop := make(chan struct{})
		startReturned := make(chan struct{})
		r := &lateRunnable{
			start: func(context.Context) error {
				syscallSelfInterrupt(t)
				<-stop
				defer close(startReturned)
				return wantErr
			},
			stop: func(ctx context.Context) error {
				<-ctx.Done() // use up the server's whole share
				close(stop)
				<-startReturned
				time.Sleep(5 * time.Millisecond) // let Start's return value land in the channel first
				return nil
			},
		}
		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		err := Run(r, WithConfigPath(emptyConf(t)), WithLogger(log), WithStopTimeout(90*time.Millisecond))
		if !errors.Is(err, wantErr) {
			t.Fatalf("run %d: Start's error was lost, got=%v", i, err)
		}
		if strings.Contains(logs.String(), "server did not exit") {
			t.Fatalf("run %d: the server has exited and must not be warned about as not having exited:\n%s", i, logs.String())
		}
	}
}
