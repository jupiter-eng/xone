package xhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/internal/hook"
)

const self = "github.com/jupiter-eng/xone/xhook"

func initFake(context.Context) error { return nil }

type fakeComp struct{}

func (c *fakeComp) start(context.Context) error { return nil }
func (c fakeComp) stop(context.Context) error   { return nil }

func initFor[T any](context.Context) error { return nil }

// only takes the single entry registered in this round.
func only(t *testing.T, es []hook.Entry) hook.Entry {
	t.Helper()
	if len(es) != 1 {
		t.Fatalf("exactly one entry must be registered, got %d", len(es))
	}
	return es[0]
}

func clean(t *testing.T) {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
}

func TestBeforeStart_DefaultsToBusinessStage(t *testing.T) {
	// The default stage is the caller's business code. Landing on StageClient would put business hooks on the
	// same stage as xgorm, and which runs first would depend on package initialization order -- with the business package path sorting first, an xgorm.C() in the hook panics on the spot.
	clean(t)
	BeforeStart(initFake)
	BeforeStop(initFake)
	if got := only(t, hook.Start()).Stage; got != StageBusiness {
		t.Errorf("a start hook must default to StageBusiness, got=%v", got)
	}
	if got := only(t, hook.Stop()).Stage; got != StageBusiness {
		t.Errorf("a stop hook must default to StageBusiness, got=%v", got)
	}
}

func TestAt_OverridesDefaultStage(t *testing.T) {
	clean(t)
	BeforeStart(initFake, At(StageLog))
	BeforeStop(initFake, At(StageServer))
	if got := only(t, hook.Start()).Stage; got != StageLog {
		t.Errorf("the start stage had no effect, got=%v", got)
	}
	if got := only(t, hook.Stop()).Stage; got != StageServer {
		t.Errorf("the stop stage had no effect, got=%v", got)
	}
}

func TestBeforeStop_WithoutStageFollowsPairedStartHook(t *testing.T) {
	// At is written once, on the start hook: closing lands on the same stage and does not fall back to
	// StageBusiness -- otherwise the client would be closed before the business hooks, whose cleanup would then find a closed connection.
	clean(t)
	BeforeStart(initFake, At(StageClient))
	BeforeStop(initFake)
	if got := only(t, hook.Stop()).Stage; got != StageClient {
		t.Errorf("a stop hook must inherit the start hook's stage StageClient, got=%v", got)
	}
}

func TestBeforeStop_UnpairedDefaultsToBusinessStage(t *testing.T) {
	clean(t)
	BeforeStop(initFake)
	if got := only(t, hook.Stop()).Stage; got != StageBusiness {
		t.Errorf("without a pair a stop hook must be on StageBusiness, got=%v", got)
	}
}

func TestAt_LastOneWins(t *testing.T) {
	clean(t)
	BeforeStart(initFake, At(StageLog), At(StageServer))
	if got := only(t, hook.Start()).Stage; got != StageServer {
		t.Errorf("the options must stack in order, with the last one winning, got=%v", got)
	}
}

func TestBeforeStart_NameTakenFromPassedFunc(t *testing.T) {
	// The name is the only bit of location the caller sees in logs and in a startup failure message.
	// Getting it wrong leaves "which hook failed" to guesswork.
	clean(t)
	BeforeStart(initFake)
	if got, want := only(t, hook.Start()).Name, "xhook.initFake"; got != want {
		t.Errorf("want %s, got %s", want, got)
	}
}

func TestBeforeStart_MethodValueHasName(t *testing.T) {
	clean(t)
	c := &fakeComp{}
	BeforeStart(c.start)
	BeforeStop(fakeComp{}.stop)

	if got, want := only(t, hook.Start()).Name, "xhook.(*fakeComp).start-fm"; got != want {
		t.Errorf("pointer receiver want %s, got %s", want, got)
	}
	if got, want := only(t, hook.Stop()).Name, "xhook.fakeComp.stop-fm"; got != want {
		t.Errorf("value receiver want %s, got %s", want, got)
	}
}

func TestBeforeStart_GenericInstanceNameHasNoPath(t *testing.T) {
	// After instantiation the runtime gives a generic a name of the form pkg.initFor[...], with the type
	// arguments folded into "...". If the runtime wrote the full type arguments instead, a "/" inside them would
	// make a split on the last slash land inside the brackets -- this case watches that premise.
	clean(t)
	BeforeStart(initFor[int])

	e := only(t, hook.Start())
	if strings.Contains(e.Name, "/") {
		t.Errorf("no path may be left in the name, got %s", e.Name)
	}
	if !strings.HasPrefix(e.Name, "xhook.initFor[") {
		t.Errorf("want the prefix xhook.initFor[, got %s", e.Name)
	}
	if e.Pkg != self {
		t.Errorf("package name of a generic instantiation want %s, got %s", self, e.Pkg)
	}
}

func TestBeforeStart_AnonymousFuncHasName(t *testing.T) {
	clean(t)
	BeforeStart(func(context.Context) error { return nil })

	e := only(t, hook.Start())
	if !strings.HasPrefix(e.Name, "xhook.TestBeforeStart_AnonymousFuncHasName.func") {
		t.Errorf("an anonymous function's name must carry the function it sits in, got %s", e.Name)
	}
	if e.Pkg != self {
		t.Errorf("want %s, got %s", self, e.Pkg)
	}
}

func TestBeforeStart_NilFallsBackToPlaceholderName(t *testing.T) {
	// Registering nil is the caller's mistake, but it must not blow up at the naming step: the error message
	// would then not even say which one it was. It is left to run time, where the framework's panic isolation
	// turns it into an ordinary startup error.
	clean(t)
	BeforeStart(nil)

	e := only(t, hook.Start())
	if e.Name == "" || e.Pkg == "" {
		t.Errorf("neither the name nor the package may be empty, got name=%q pkg=%q", e.Name, e.Pkg)
	}
}

func TestBeforeStart_RegistersThePassedFunc(t *testing.T) {
	clean(t)
	want := errors.New("boom")
	BeforeStart(func(context.Context) error { return want })

	if got := only(t, hook.Start()).Run(context.Background()); !errors.Is(got, want) {
		t.Errorf("Run did not run the registered function, got %v", got)
	}
}

func TestBeforeStart_StartAndStopGoToSeparateBoards(t *testing.T) {
	clean(t)
	BeforeStart(initFake)
	if len(hook.Stop()) != 0 {
		t.Error("only a start hook was registered, so the stop board must be empty")
	}
	BeforeStop(initFake)
	if len(hook.Start()) != 1 || len(hook.Stop()) != 1 {
		t.Errorf("one entry on each board, got start=%d stop=%d", len(hook.Start()), len(hook.Stop()))
	}
}

func TestPkgOf_ReturnsFullImportPath(t *testing.T) {
	// With the last path element as the pairing key, two packages with the same last element are taken for the
	// same one: wrapping a package of one's own called xlog is a common thing to do, and on a collision its start
	// hook failing skips the framework xlog's stop hook with it, so the log writer never flushes again.
	cases := []struct{ full, want string }{
		{"github.com/jupiter-eng/xone/xredis.initXRedis", "github.com/jupiter-eng/xone/xredis"},
		{"github.com/you/app/xlog.initMyLog", "github.com/you/app/xlog"},
		{"github.com/jupiter-eng/xone/xgorm.(*pool).close-fm", "github.com/jupiter-eng/xone/xgorm"},
		{"github.com/jupiter-eng/xone/xgorm/clickhouse.register", "github.com/jupiter-eng/xone/xgorm/clickhouse"},
		{"github.com/jupiter-eng/xone/xcache.initXCache.func1", "github.com/jupiter-eng/xone/xcache"},
		{"github.com/jupiter-eng/xone/xclient.Build[...]", "github.com/jupiter-eng/xone/xclient"},
		{"main.initSomething", "main"},
		{"hook", "hook"}, // the fallback when no name can be taken
	}
	for _, c := range cases {
		if got := pkgOf(c.full); got != c.want {
			t.Errorf("pkgOf(%q)\nwant %s\ngot  %s", c.full, c.want, got)
		}
	}
}

func TestPkgOf_PackagesWithSameLastSegmentNotPaired(t *testing.T) {
	mine := pkgOf("github.com/you/app/xlog.initMyLog")
	theirs := pkgOf("github.com/jupiter-eng/xone/xlog.initXLog")
	if mine == theirs {
		t.Errorf("two different packages got the same pairing key %q", mine)
	}
}

func TestInitPkg_OnlyRecognizesPackageInitFuncs(t *testing.T) {
	// Package-level variable initialization lives in pkg.init, and each func init() is pkg.init.N.
	// A closure inside an init, a method that happens to be called init, and an initXRedis are none of those.
	cases := []struct {
		fn, want string
		ok       bool
	}{
		{"github.com/you/app/xredis.init", "github.com/you/app/xredis", true},
		{"github.com/you/app/xredis.init.0", "github.com/you/app/xredis", true},
		{"github.com/you/app/xredis.init.12", "github.com/you/app/xredis", true},
		{"main.init.1", "main", true},
		{"github.com/you/app/xredis.init.0.func1", "", false},
		{"github.com/you/app/xredis.initXRedis", "", false},
		{"github.com/you/app/xredis.(*T).init", "", false},
		{"github.com/you/app/xredis.init.", "", false},
		{"runtime.doInit1", "", false},
	}
	for _, c := range cases {
		got, ok := initPkg(c.fn)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("initPkg(%q) = %q, %v; want %q, %v", c.fn, got, ok, c.want, c.ok)
		}
	}
}

func TestBeforeStart_OutsideInitUsesHookFuncPackage(t *testing.T) {
	// A direct call in a test, or registration in main: no package initialization function is on the stack, so it falls back to the hook function's package.
	clean(t)
	BeforeStart(initFake)
	if got := only(t, hook.Start()).Pkg; got != self {
		t.Errorf("want %s, got %s", self, got)
	}
}

func TestShortName_StripsOnlyPathPrefix(t *testing.T) {
	cases := []struct{ full, want string }{
		{"github.com/jupiter-eng/xone/xredis.initXRedis", "xredis.initXRedis"},
		{"github.com/jupiter-eng/xone/xgorm.(*pool).close-fm", "xgorm.(*pool).close-fm"},
		{"main.initSomething", "main.initSomething"},
		{"hook", "hook"},
	}
	for _, c := range cases {
		if got := shortName(c.full); got != c.want {
			t.Errorf("shortName(%q)\nwant %s\ngot  %s", c.full, c.want, got)
		}
	}
}
