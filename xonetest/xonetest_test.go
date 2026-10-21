package xonetest

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

type demo struct {
	Name string `yaml:"Name"`
}

// fakeT records "did it fail" and the Cleanups on behalf of a test, to test this package's own failing and cleanup behavior.
type fakeT struct {
	*testing.T
	failed   bool
	cleanups []func()
}

func (f *fakeT) Errorf(string, ...any) { f.failed = true }
func (f *fakeT) Fatalf(string, ...any) { f.failed = true; runtime.Goexit() }
func (f *fakeT) Cleanup(fn func())     { f.cleanups = append(f.cleanups, fn) }

// run executes fn in a goroutine of its own (the Goexit from Fatalf only ends that goroutine) and then runs
// the Cleanups in reverse, the same way a subtest ends.
func run(t *testing.T, fn func(testing.TB)) *fakeT {
	f := &fakeT{T: t}
	done := make(chan struct{})
	go func() { defer close(done); fn(f) }()
	<-done
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
	return f
}

func TestUseConfigYAML_ReadsGivenYAMLAndCleansUpAtEnd(t *testing.T) {
	run(t, func(tb testing.TB) {
		UseConfigYAML(tb, "Demo:\n  Name: inner\n")
		var d demo
		if err := xconfig.Unmarshal("Demo", &d); err != nil || d.Name != "inner" {
			t.Errorf("the configuration given by the test must be read, got=%+v err=%v", d, err)
		}
	})
	// Cleared at the end: the next test's configuration must not still be the previous one's.
	if config.Has("Demo") {
		t.Error("the configuration must be cleared when the test ends")
	}
}

func TestUseConfig_LoadFailureFailsTest(t *testing.T) {
	if f := run(t, func(tb testing.TB) { UseConfig(tb, "/definitely/not/here.yml") }); !f.failed {
		t.Error("a missing configuration file must fail the test")
	}
}

// board swaps in an empty registration board and clears it when the test ends.
func board(t *testing.T) *[]string {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
	return new([]string)
}

func rec(seq *[]string, s string, err error) xhook.HookFunc {
	return func(context.Context) error { *seq = append(*seq, s); return err }
}

func TestStartHooks_StartsByStageAndClosesInReverseAtEnd(t *testing.T) {
	seq := board(t)
	xhook.BeforeStart(rec(seq, "start:biz", nil))
	xhook.BeforeStop(rec(seq, "stop:biz", nil))
	xhook.BeforeStart(rec(seq, "start:client", nil), xhook.At(xhook.StageClient))
	xhook.BeforeStop(rec(seq, "stop:client", nil))

	f := run(t, func(tb testing.TB) {
		StartHooks(tb)
		if got := strings.Join(*seq, " "); got != "start:client start:biz" {
			t.Errorf("the start hooks must run in ascending stage order, got=%s", got)
		}
	})

	if f.failed {
		t.Error("the hooks all succeeded, so the test must not fail")
	}
	if got := strings.Join(*seq, " "); got != "start:client start:biz stop:biz stop:client" {
		t.Errorf("the stop hooks must run in reverse when the test ends, got=%s", got)
	}
}

func TestStartHooks_OnStartFailureClosesOnlyStarted(t *testing.T) {
	seq := board(t)
	xhook.BeforeStart(rec(seq, "start:a", nil), xhook.At(xhook.StageClient))
	xhook.BeforeStop(rec(seq, "stop:a", nil))
	xhook.BeforeStart(rec(seq, "start:b", errors.New("boom")))
	xhook.BeforeStop(rec(seq, "stop:b", nil))

	if f := run(t, StartHooks); !f.failed {
		t.Error("a failing start hook must fail the test")
	}
	if got := strings.Join(*seq, " "); got != "start:a start:b stop:a" {
		t.Errorf("the failing pair must not run its stop hook, and what is already up must be closed, got=%s", got)
	}
}

func TestStartHooks_StopHookErrorFailsTest(t *testing.T) {
	seq := board(t)
	xhook.BeforeStart(rec(seq, "start:a", nil))
	xhook.BeforeStop(rec(seq, "stop:a", errors.New("flush failed")))

	if f := run(t, StartHooks); !f.failed {
		t.Error("an error from a stop hook must not be swallowed")
	}
}
