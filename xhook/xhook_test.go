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

// only 取出本轮登记的唯一一项
func only(t *testing.T, es []hook.Entry) hook.Entry {
	t.Helper()
	if len(es) != 1 {
		t.Fatalf("应当只登记了一项，got %d", len(es))
	}
	return es[0]
}

func clean(t *testing.T) {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
}

func TestBeforeStart_DefaultsToBusinessStage(t *testing.T) {
	// 默认档是使用者的业务代码。落到 StageClient 的话，业务钩子和 xgorm 同档，
	// 谁先跑看包的初始化顺序——业务包路径排在前面时，钩子里的 xgorm.C() 当场 panic
	clean(t)
	BeforeStart(initFake)
	BeforeStop(initFake)
	if got := only(t, hook.Start()).Stage; got != StageBusiness {
		t.Errorf("启动钩子的默认档应为 StageBusiness，got=%v", got)
	}
	if got := only(t, hook.Stop()).Stage; got != StageBusiness {
		t.Errorf("停止钩子的默认档应为 StageBusiness，got=%v", got)
	}
}

func TestAt_OverridesDefaultStage(t *testing.T) {
	clean(t)
	BeforeStart(initFake, At(StageLog))
	BeforeStop(initFake, At(StageServer))
	if got := only(t, hook.Start()).Stage; got != StageLog {
		t.Errorf("启动档位没生效，got=%v", got)
	}
	if got := only(t, hook.Stop()).Stage; got != StageServer {
		t.Errorf("停止档位没生效，got=%v", got)
	}
}

func TestBeforeStop_WithoutStageFollowsPairedStartHook(t *testing.T) {
	// 只在启动钩子上写一次 At：关闭也落在同一档，不会掉回 StageBusiness——
	// 否则客户端会在业务钩子之前被关掉，业务的收尾动作摸到的是关掉的连接
	clean(t)
	BeforeStart(initFake, At(StageClient))
	BeforeStop(initFake)
	if got := only(t, hook.Stop()).Stage; got != StageClient {
		t.Errorf("停止钩子该继承启动钩子的档位 StageClient，got=%v", got)
	}
}

func TestBeforeStop_UnpairedDefaultsToBusinessStage(t *testing.T) {
	clean(t)
	BeforeStop(initFake)
	if got := only(t, hook.Stop()).Stage; got != StageBusiness {
		t.Errorf("没有配对时停止钩子该在 StageBusiness，got=%v", got)
	}
}

func TestAt_LastOneWins(t *testing.T) {
	clean(t)
	BeforeStart(initFake, At(StageLog), At(StageServer))
	if got := only(t, hook.Start()).Stage; got != StageServer {
		t.Errorf("选项应按顺序叠加，最后一个生效，got=%v", got)
	}
}

func TestBeforeStart_NameTakenFromPassedFunc(t *testing.T) {
	// 名字是使用者在日志和启动失败信息里唯一能看到的定位信息。
	// 取不准的话，「哪个钩子失败了」就得靠猜
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
		t.Errorf("指针接收者 want %s, got %s", want, got)
	}
	if got, want := only(t, hook.Stop()).Name, "xhook.fakeComp.stop-fm"; got != want {
		t.Errorf("值接收者 want %s, got %s", want, got)
	}
}

func TestBeforeStart_GenericInstanceNameHasNoPath(t *testing.T) {
	// 泛型实例化后 runtime 给的名字形如 pkg.initFor[...]，类型实参被折叠成
	// "..."。要是 runtime 把完整类型实参写进去，里面的 "/" 会让按最后一个
	// 斜杠切分的做法切在括号内部——这条用例盯着这个前提
	clean(t)
	BeforeStart(initFor[int])

	e := only(t, hook.Start())
	if strings.Contains(e.Name, "/") {
		t.Errorf("名字里不该还留着路径，got %s", e.Name)
	}
	if !strings.HasPrefix(e.Name, "xhook.initFor[") {
		t.Errorf("want 前缀 xhook.initFor[，got %s", e.Name)
	}
	if e.Pkg != self {
		t.Errorf("泛型实例的包名 want %s, got %s", self, e.Pkg)
	}
}

func TestBeforeStart_AnonymousFuncHasName(t *testing.T) {
	clean(t)
	BeforeStart(func(context.Context) error { return nil })

	e := only(t, hook.Start())
	if !strings.HasPrefix(e.Name, "xhook.TestBeforeStart_AnonymousFuncHasName.func") {
		t.Errorf("匿名函数的名字应带上它所在的那个函数，got %s", e.Name)
	}
	if e.Pkg != self {
		t.Errorf("want %s, got %s", self, e.Pkg)
	}
}

func TestBeforeStart_NilFallsBackToPlaceholderName(t *testing.T) {
	// 登记 nil 是使用者的错，但不该在取名字这一步先炸掉：
	// 那样错误信息里连「是哪一条」都没有。留到执行时由框架的 panic
	// 隔离变成一条普通的启动错误
	clean(t)
	BeforeStart(nil)

	e := only(t, hook.Start())
	if e.Name == "" || e.Pkg == "" {
		t.Errorf("名字和包名都不该为空，got name=%q pkg=%q", e.Name, e.Pkg)
	}
}

func TestBeforeStart_RegistersThePassedFunc(t *testing.T) {
	clean(t)
	want := errors.New("boom")
	BeforeStart(func(context.Context) error { return want })

	if got := only(t, hook.Start()).Run(context.Background()); !errors.Is(got, want) {
		t.Errorf("Run 跑的不是登记的那个函数，got %v", got)
	}
}

func TestBeforeStart_StartAndStopGoToSeparateBoards(t *testing.T) {
	clean(t)
	BeforeStart(initFake)
	if len(hook.Stop()) != 0 {
		t.Error("只登记了启动钩子，停止板不该有东西")
	}
	BeforeStop(initFake)
	if len(hook.Start()) != 1 || len(hook.Stop()) != 1 {
		t.Errorf("两块板各一项，got start=%d stop=%d", len(hook.Start()), len(hook.Stop()))
	}
}

func TestPkgOf_ReturnsFullImportPath(t *testing.T) {
	// 配对键取末段包名的话，两个末段同名的包会被当成同一个：
	// 使用者自己包一层叫 xlog 的包很常见，撞上之后它的启动钩子一失败，
	// 框架 xlog 的停止钩子就跟着被跳过，日志写入器再也不 flush
	cases := []struct{ full, want string }{
		{"github.com/jupiter-eng/xone/xredis.initXRedis", "github.com/jupiter-eng/xone/xredis"},
		{"github.com/you/app/xlog.initMyLog", "github.com/you/app/xlog"},
		{"github.com/jupiter-eng/xone/xgorm.(*pool).close-fm", "github.com/jupiter-eng/xone/xgorm"},
		{"github.com/jupiter-eng/xone/xgorm/clickhouse.register", "github.com/jupiter-eng/xone/xgorm/clickhouse"},
		{"github.com/jupiter-eng/xone/xcache.initXCache.func1", "github.com/jupiter-eng/xone/xcache"},
		{"github.com/jupiter-eng/xone/xclient.Build[...]", "github.com/jupiter-eng/xone/xclient"},
		{"main.initSomething", "main"},
		{"hook", "hook"}, // 取不到名字时的退路
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
		t.Errorf("两个不同的包拿到了同一个配对键 %q", mine)
	}
}

func TestInitPkg_OnlyRecognizesPackageInitFuncs(t *testing.T) {
	// 包级变量的初始化在 pkg.init 里，每个 func init() 是 pkg.init.N。
	// init 里的闭包、名字恰好叫 init 的方法、initXRedis 这种都不是
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
	// 测试里直接调、或者在 main 里登记：栈上没有包初始化函数，退回钩子函数的包
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
