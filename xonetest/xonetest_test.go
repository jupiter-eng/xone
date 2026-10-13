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

// fakeT 替测试记下「失败了没有」和 Cleanup，用来测本包自己让测试失败、收尾的行为
type fakeT struct {
	*testing.T
	failed   bool
	cleanups []func()
}

func (f *fakeT) Errorf(string, ...any) { f.failed = true }
func (f *fakeT) Fatalf(string, ...any) { f.failed = true; runtime.Goexit() }
func (f *fakeT) Cleanup(fn func())     { f.cleanups = append(f.cleanups, fn) }

// run 在自己的协程里跑 fn（Fatalf 的 Goexit 只结束这个协程），再逆序跑 Cleanup，
// 和一个子测试结束时一样
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
			t.Errorf("该读到测试给的配置，got=%+v err=%v", d, err)
		}
	})
	// 结束时清掉：下一个测试的配置不该还是上一个的
	if config.Has("Demo") {
		t.Error("测试结束之后配置该被清掉")
	}
}

func TestUseConfig_LoadFailureFailsTest(t *testing.T) {
	if f := run(t, func(tb testing.TB) { UseConfig(tb, "/definitely/not/here.yml") }); !f.failed {
		t.Error("配置文件不存在时该让测试失败")
	}
}

// board 换一块空的登记板，测试结束时清空
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
			t.Errorf("启动钩子该按档位升序跑，got=%s", got)
		}
	})

	if f.failed {
		t.Error("钩子都成功了，测试不该失败")
	}
	if got := strings.Join(*seq, " "); got != "start:client start:biz stop:biz stop:client" {
		t.Errorf("测试结束时该逆序跑停止钩子，got=%s", got)
	}
}

func TestStartHooks_OnStartFailureClosesOnlyStarted(t *testing.T) {
	seq := board(t)
	xhook.BeforeStart(rec(seq, "start:a", nil), xhook.At(xhook.StageClient))
	xhook.BeforeStop(rec(seq, "stop:a", nil))
	xhook.BeforeStart(rec(seq, "start:b", errors.New("boom")))
	xhook.BeforeStop(rec(seq, "stop:b", nil))

	if f := run(t, StartHooks); !f.failed {
		t.Error("启动钩子失败时该让测试失败")
	}
	if got := strings.Join(*seq, " "); got != "start:a start:b stop:a" {
		t.Errorf("失败的那一对不该跑停止钩子，已经起来的要关掉，got=%s", got)
	}
}

func TestStartHooks_StopHookErrorFailsTest(t *testing.T) {
	seq := board(t)
	xhook.BeforeStart(rec(seq, "start:a", nil))
	xhook.BeforeStop(rec(seq, "stop:a", errors.New("flush failed")))

	if f := run(t, StartHooks); !f.failed {
		t.Error("停止钩子出错不该被吞掉")
	}
}
