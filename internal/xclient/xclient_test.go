package xclient

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jupiter-eng/xone/xerror"
)

// conn is a fake connection pool that remembers whether it was closed
type conn struct {
	name string
	log  *log
	err  error
	boom bool
}

func (c *conn) Close() error {
	if c.boom {
		panic("close exploded")
	}
	c.log.add("close:" + c.name)
	return c.err
}

type log struct {
	mu  sync.Mutex
	seq []string
}

func (l *log) add(s string) { l.mu.Lock(); l.seq = append(l.seq, s); l.mu.Unlock() }
func (l *log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.seq, " ")
}

type cfg struct {
	err  error
	boom bool
}

// builder makes a new function that, as its configuration says, builds successfully, returns an
// error or panics.
// What it records is the instance name Build handed in, which verifies the name along the way
func builder(l *log) func(context.Context, string, cfg) (*conn, io.Closer, error) {
	return func(_ context.Context, name string, c cfg) (*conn, io.Closer, error) {
		if c.boom {
			l.add("panic:" + name)
			panic("new exploded")
		}
		if c.err != nil {
			l.add("fail:" + name)
			return nil, nil, c.err
		}
		l.add("new:" + name)
		v := &conn{name: name, log: l}
		return v, v, nil
	}
}

func newReg() *Registry[*conn] { return NewRegistry[*conn]("xfake", "XFake") }

func mustPanic(t *testing.T, f func()) any {
	t.Helper()
	var got any
	func() {
		defer func() { got = recover() }()
		f()
	}()
	if got == nil {
		t.Fatal("a panic was expected and did not come")
	}
	return got
}

// publish publishes a set of ready-made instances through Build: these tests only want "the
// registry holds these"
func publish(r *Registry[*conn], items map[string]*conn) {
	keep := func(_ context.Context, _ string, c *conn) (*conn, io.Closer, error) { return c, nil, nil }
	if err := Build(context.Background(), r, items, keep); err != nil {
		panic(err)
	}
}

// ---- Taking an instance ----

func TestGet_NoArgReturnsDefault(t *testing.T) {
	r := newReg()
	want := &conn{name: "d"}
	publish(r, map[string]*conn{DefaultName: want})
	if got := r.Get(); got != want {
		t.Errorf("want %v, got %v", want, got)
	}
	if got := r.Get(DefaultName); got != want {
		t.Errorf("writing default explicitly must give the same one, got %v", got)
	}
}

func TestGet_PanicsInsteadOfReturningZeroWhenMissing(t *testing.T) {
	// Returning nil only defers the same panic to the caller's first use of it, where the stack
	// holds nothing but "invalid memory address" and says nothing about the configuration being the
	// root cause
	r := newReg()
	publish(r, map[string]*conn{"read": {}, "write": {}})

	msg := mustPanic(t, func() { r.Get("relay") })
	s, _ := msg.(string)
	for _, want := range []string{"xfake", `"relay"`, "read", "write"} {
		if !strings.Contains(s, want) {
			t.Errorf("the panic text must contain %q, got: %s", want, s)
		}
	}
}

func TestGet_NamesConfigKeyWhenNoneConfigured(t *testing.T) {
	// A misspelled name and an unconfigured block are two different problems and the wording has to
	// tell them apart at a glance: the first sends somebody to the name, the second to whether the
	// block was written at all and whether the matching package was imported
	r := newReg()
	publish(r, nil) // the block is unconfigured: the start hook still goes through Build and builds none
	s, _ := mustPanic(t, func() { r.Get() }).(string)
	if !strings.Contains(s, "XFake") {
		t.Errorf("with none configured it has to name the configuration block to look at, got: %s", s)
	}
	if !strings.Contains(s, "none is configured") {
		t.Errorf("it has to say the whole block is unconfigured rather than the name being misspelled, got: %s", s)
	}
}

func TestGet_SaysTooEarlyBeforeStartHook(t *testing.T) {
	// This used to report "the whole block is unconfigured", sending whoever took an instance in
	// main, before Run, to a configuration file that was perfectly correct
	r := newReg()
	s, _ := mustPanic(t, func() { r.Get() }).(string)
	if !strings.Contains(s, "before xone.Run started") {
		t.Errorf("it has to say it was called too early, got: %s", s)
	}
	if strings.Contains(s, "none is configured") {
		t.Errorf("called too early must not be reported as unconfigured, got: %s", s)
	}
}

func TestGet_SaysTooLateAfterClose(t *testing.T) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := mustPanic(t, func() { r.Get() }).(string)
	if !strings.Contains(s, "after xfake was closed") {
		t.Errorf("it has to say it was called too late, got: %s", s)
	}
}

func TestLookup_ReturnsZeroAndFalseWhenMissing(t *testing.T) {
	r := newReg()
	if v, ok := r.Lookup("nope"); ok || v != nil {
		t.Errorf("want nil,false, got %v,%v", v, ok)
	}
}

func TestHas_ForOptionalDependencies(t *testing.T) {
	r := newReg()
	if r.Has() {
		t.Error("an empty registry must not report a default")
	}
	publish(r, map[string]*conn{DefaultName: {}})
	if !r.Has() || r.Has("other") {
		t.Errorf("got Has()=%v Has(other)=%v", r.Has(), r.Has("other"))
	}
}

func TestNames_SortedByName(t *testing.T) {
	// A stable order is what makes the logs and the error texts reproducible
	r := newReg()
	publish(r, map[string]*conn{"write": {}, "read": {}, "archive": {}})
	if got, want := strings.Join(r.Names(), " "), "archive read write"; got != want {
		t.Errorf("want %s, got %s", want, got)
	}
}

func TestNames_EmptyRegistryReturnsEmptySlice(t *testing.T) {
	if got := newReg().Names(); len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}

func TestAll_ReturnsEveryInstanceAsACopy(t *testing.T) {
	r := newReg()
	a, b := &conn{name: "a"}, &conn{name: "b"}
	publish(r, map[string]*conn{"a": a, "b": b})

	got := r.All()
	if len(got) != 2 || got["a"] != a || got["b"] != b {
		t.Fatalf("want the two instances a and b, got %v", got)
	}
	// A snapshot is immutable once published: what the caller changes is a copy of their own
	delete(got, "a")
	got["c"] = &conn{}
	if !r.Has("a") || r.Has("c") {
		t.Errorf("changing what All returned must not change the registry, got %v", r.Names())
	}
}

func TestAll_EmptyBeforeStartAndAfterClose(t *testing.T) {
	r := newReg()
	if got := r.All(); got == nil || len(got) != 0 {
		t.Errorf("before startup, want a non-nil empty map, got %v", got)
	}
	publish(r, map[string]*conn{"a": {}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := r.All(); got == nil || len(got) != 0 {
		t.Errorf("after closing, want a non-nil empty map, got %v", got)
	}
}

// ---- Building the instances ----

func TestBuild_BuildsInNameOrder(t *testing.T) {
	// The order they are built in has to be reproducible, or "five databases configured, which one
	// broke" is different every time
	l := &log{}
	r := newReg()
	err := Build(context.Background(), r, map[string]cfg{
		"write": {}, "archive": {}, "read": {},
	}, builder(l))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.String(), "new:archive new:read new:write"; got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
	if got, want := strings.Join(r.Names(), " "), "archive read write"; got != want {
		t.Errorf("everything built must be published, want %s, got %s", want, got)
	}
}

func TestBuild_PublishesEmptyRegistryWithoutConfig(t *testing.T) {
	r := newReg()
	if err := Build(context.Background(), r, map[string]cfg{}, builder(&log{})); err != nil {
		t.Fatal(err)
	}
	if len(r.Names()) != 0 {
		t.Errorf("want empty, got %v", r.Names())
	}
}

func TestBuild_ClosesBuiltOnesOnMidwayFailure(t *testing.T) {
	// When a start hook returns an error, this package's stop hook does not run, and without
	// cleaning up here those earlier connection pools would leak
	l := &log{}
	boom := errors.New("dial refused")
	err := Build(context.Background(), newReg(), map[string]cfg{
		"a": {}, "b": {}, "c": {err: boom},
	}, builder(l))

	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying error must be reachable through errors.Is, got %v", err)
	}
	if got, want := l.String(), "new:a new:b fail:c close:b close:a"; got != want {
		t.Errorf("what was built must be closed in reverse order\nwant %s\ngot  %s", want, got)
	}
}

func TestBuild_RegistryUnchangedOnFailure(t *testing.T) {
	// Publishing half a set is worse than publishing none: C() finds a and not b, while startup has
	// in fact failed
	l := &log{}
	r := newReg()
	old := &conn{name: "old"}
	publish(r, map[string]*conn{DefaultName: old})

	err := Build(context.Background(), r, map[string]cfg{
		"a": {}, "b": {err: errors.New("nope")},
	}, builder(l))
	if err == nil {
		t.Fatal("want an error")
	}
	if got, ok := r.Lookup(DefaultName); !ok || got != old {
		t.Errorf("the registry was polluted with half a set, got %v", r.Names())
	}
}

func TestBuild_ErrorNamesInstance(t *testing.T) {
	err := Build(context.Background(), newReg(), map[string]cfg{
		"replica": {err: errors.New("dial refused")},
	}, builder(&log{}))
	if err == nil || !strings.Contains(err.Error(), `"replica"`) {
		t.Errorf("the error has to say which instance it is, got %v", err)
	}
	if !xerror.Is(err, "xfake") {
		t.Errorf("the error must be credited to this module, got %v", err)
	}
}

func TestBuild_OwnModuleErrorNotRewrappedAndKeepsOp(t *testing.T) {
	// One xerror per module boundary. New reported connect already, and another layer with new
	// would put the module name in the text twice and have errors.As always take out new, leaving
	// the caller unable to tell a misconfiguration from an unreachable server
	root := errors.New("dial refused")
	err := Build(context.Background(), newReg(), map[string]cfg{
		"replica": {err: xerror.Newf("xfake", "connect", "cannot reach h:1: %w", root)},
	}, builder(&log{}))

	var xe *xerror.Error
	if !errors.As(err, &xe) {
		t.Fatalf("this must be an xerror, got %v", err)
	}
	if xe.Module != "xfake" || xe.Op != "connect" {
		t.Errorf("the op New reported must be kept, got module=%s op=%s", xe.Module, xe.Op)
	}
	var inner *xerror.Error
	if errors.As(xe.Err, &inner) {
		t.Errorf("one module may have one layer of xerror only, got %v", err)
	}
	if strings.Count(err.Error(), "xfake") != 1 {
		t.Errorf("the module name may appear once only, got %v", err)
	}
	if !strings.Contains(err.Error(), `"replica"`) || !errors.Is(err, root) {
		t.Errorf("both the instance name and the root cause must be kept, got %v", err)
	}
}

func TestBuild_OtherErrorsWrappedOnceAsNew(t *testing.T) {
	err := Build(context.Background(), newReg(), map[string]cfg{
		"a": {err: xerror.New("xother", "connect", errors.New("x"))},
	}, builder(&log{}))
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xfake" || xe.Op != "new" {
		t.Errorf("an error from elsewhere must be credited to this module's new, got %v", err)
	}
	if !xerror.Is(err, "xother") {
		t.Errorf("the inner layer's module must be kept, got %v", err)
	}
}

func TestBuild_StopsBuildingOnShutdownSignal(t *testing.T) {
	// With five databases configured and the first one retrying until it times out, a shutdown
	// signal should stop it right there rather than having the remaining four tried one by one
	l := &log{}
	ctx, cancel := context.WithCancel(context.Background())
	var n int
	err := Build(ctx, newReg(), map[string]cfg{
		"a": {}, "b": {}, "c": {},
	}, func(_ context.Context, name string, _ cfg) (*conn, io.Closer, error) {
		n++
		if n == 2 {
			cancel()
		}
		l.add("new:" + name)
		v := &conn{name: name, log: l}
		return v, v, nil
	})

	if err == nil {
		t.Fatal("want an error: an interrupted startup cannot be reported as a success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("it must be visible that this was cancelled, got %v", err)
	}
	if got, want := l.String(), "new:a new:b close:b close:a"; got != want {
		t.Errorf("what was built must be closed and c must not be built\nwant %s\ngot  %s", want, got)
	}
}

func TestBuild_BuildsNothingWhenAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := &log{}
	err := Build(ctx, newReg(), map[string]cfg{"a": {}}, builder(l))
	if err == nil {
		t.Fatal("want an error")
	}
	if l.String() != "" {
		t.Errorf("not one may be built, got %s", l.String())
	}
}

func TestBuild_new_PanicDoesNotLeakBuiltOnes(t *testing.T) {
	// Uncontained, the panic is thrown up through Build while the Closers of what was already built
	// exist only in Build's own frame as local variables -- once the stack unwinds they cannot be
	// found again
	l := &log{}
	err := Build(context.Background(), newReg(), map[string]cfg{
		"a": {}, "b": {boom: true},
	}, builder(l))

	if err == nil {
		t.Fatal("a panic must become an ordinary error rather than taking the whole process down")
	}
	if !strings.Contains(err.Error(), "panicked") || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("the error has to say which instance blew up, got %v", err)
	}
	if got, want := l.String(), "new:a panic:b close:a"; got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestBuild_new_NilCloserIsHarmless(t *testing.T) {
	// Some instances have nothing to close, and returning a nil Closer is valid
	r := newReg()
	err := Build(context.Background(), r, map[string]cfg{"a": {}},
		func(context.Context, string, cfg) (*conn, io.Closer, error) {
			return &conn{name: "a"}, nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("closing an instance with no Closer must not be an error, got %v", err)
	}
}

// ---- Closing the instances ----

func TestClose_ClosesInReverseOrder(t *testing.T) {
	l := &log{}
	r := newReg()
	if err := Build(context.Background(), r, map[string]cfg{
		"a": {}, "b": {}, "c": {},
	}, builder(l)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := l.String(), "new:a new:b new:c close:c close:b close:a"; got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestClose_UnpublishesBeforeClosing(t *testing.T) {
	// The other way round, C() would still find an instance that is being closed halfway through
	r := newReg()
	var namesDuringClose []string
	err := Build(context.Background(), r, map[string]cfg{"a": {}},
		func(context.Context, string, cfg) (*conn, io.Closer, error) {
			return &conn{name: "a"}, closerFunc(func() error {
				namesDuringClose = r.Names()
				return nil
			}), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if len(namesDuringClose) != 0 {
		t.Errorf("an instance must no longer be findable while closing is under way, got %v", namesDuringClose)
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestClose_OneFailureDoesNotAffectOthers(t *testing.T) {
	// Shutdown closes as much as it can
	l := &log{}
	boom := errors.New("close failed")
	r := newReg()
	r.state.Store(&snapshot[*conn]{
		items: map[string]*conn{"a": {}, "b": {}, "c": {}},
		closers: []io.Closer{
			&conn{name: "a", log: l},
			&conn{name: "b", log: l, err: boom},
			&conn{name: "c", log: l},
		},
	})

	err := r.Close()
	if !errors.Is(err, boom) {
		t.Errorf("the failures have to be gathered up, got %v", err)
	}
	if got, want := l.String(), "close:c close:b close:a"; got != want {
		t.Errorf("the rest are closed as usual\nwant %s\ngot  %s", want, got)
	}
}

func TestClose_OnePanicDoesNotAffectOthers(t *testing.T) {
	l := &log{}
	r := newReg()
	r.state.Store(&snapshot[*conn]{closers: []io.Closer{
		&conn{name: "a", log: l},
		&conn{name: "b", boom: true},
		&conn{name: "c", log: l},
	}})

	err := r.Close()
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("a panic must become an ordinary error, got %v", err)
	}
	if got, want := l.String(), "close:c close:a"; got != want {
		t.Errorf("the rest are closed as usual\nwant %s\ngot  %s", want, got)
	}
}

func TestClose_RepeatedCallIsNoop(t *testing.T) {
	// A stop hook may be retried, and a second close must not close the same connection pool again
	l := &log{}
	r := newReg()
	if err := Build(context.Background(), r, map[string]cfg{"a": {}}, builder(l)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("a second close must not be an error, got %v", err)
	}
	if got, want := l.String(), "new:a close:a"; got != want {
		t.Errorf("only closed once expected\nwant %s\ngot  %s", want, got)
	}
}

func TestBuild_new_panic_ErrorStaysInChain(t *testing.T) {
	// Caught with %v it would be left as a piece of text and the caller could never ask what the
	// root cause was
	cause := errors.New("driver exploded")
	explode := func(context.Context, string, cfg) (*conn, io.Closer, error) { panic(cause) }
	err := Build(context.Background(), newReg(), map[string]cfg{"a": {}}, explode)
	if !errors.Is(err, cause) {
		t.Fatalf("the error panicked with must still be in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), `instance "a" panicked: driver exploded`) {
		t.Errorf("the wording has to say which instance panicked, got %v", err)
	}
}

func TestClose_panic_ErrorStaysInChain(t *testing.T) {
	cause := errors.New("close exploded")
	r := newReg()
	r.state.Store(&snapshot[*conn]{closers: []io.Closer{closerFunc(func() error { panic(cause) })}})

	err := r.Close()
	if !errors.Is(err, cause) {
		t.Fatalf("the error panicked with must still be in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "close panicked: close exploded") {
		t.Errorf("got %v", err)
	}
}

func TestClose_InstancesGoneAfterClose(t *testing.T) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if r.Has() {
		t.Error("an instance is still findable after closing, so a caller would reach a connection pool that is already closed")
	}
}

func TestRegistry_NoDataRaceUnderConcurrentAccess(t *testing.T) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				r.Has()
			case 1:
				r.Names()
			case 2:
				r.Lookup(DefaultName)
			case 3:
				publish(r, map[string]*conn{DefaultName: {}})
			}
		}(i)
	}
	wg.Wait()
}

// C() goes through Get on the path of every data access
func BenchmarkGet_Default(b *testing.B) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}, "b": {}})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Get()
	}
}

func BenchmarkGet_Default_Parallel(b *testing.B) {
	r := newReg()
	publish(r, map[string]*conn{DefaultName: {}, "b": {}})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Get()
		}
	})
}

func TestBuild_CloseErrorsOfBuiltOnesJoinedIntoBuildError(t *testing.T) {
	// When building failed halfway and what was built was closed again, an error from a failed close
	// used to be dropped: the leaked connection pool left not one word in the logs
	closeErr := errors.New("close a failed")
	boom := errors.New("dial refused")
	for name, fail := range map[string]func(context.CancelFunc, string) error{
		"new failed": func(_ context.CancelFunc, n string) error {
			if n == "b" {
				return boom
			}
			return nil
		},
		"a shutdown signal arrived": func(cancel context.CancelFunc, n string) error {
			if n == "a" {
				cancel()
			}
			return nil
		},
	} {
		l := &log{}
		ctx, cancel := context.WithCancel(context.Background())
		err := Build(ctx, newReg(), map[string]cfg{"a": {}, "b": {}}, func(_ context.Context, n string, _ cfg) (*conn, io.Closer, error) {
			if err := fail(cancel, n); err != nil {
				return nil, nil, err
			}
			v := &conn{name: n, log: l, err: closeErr}
			return v, v, nil
		})
		cancel()
		if !errors.Is(err, closeErr) {
			t.Errorf("%s: a failed close must be joined into Build's error, got %v", name, err)
		}
		if !errors.Is(err, boom) && !errors.Is(err, context.Canceled) {
			t.Errorf("%s: the original error must be kept, got %v", name, err)
		}
	}
}
