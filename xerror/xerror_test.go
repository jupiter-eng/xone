package xerror

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errBase = errors.New("底层错误")

func TestErrorMessage(t *testing.T) {
	e := New("xgorm", "init", errBase)
	got := e.Error()
	// 前缀是框架名 xone，曾经手抖写成了 xtwo —— 它出现在本包产出的每一条错误里
	for _, want := range []string{"xone ", "xgorm", "init", "底层错误"} {
		if !strings.Contains(got, want) {
			t.Errorf("错误消息应包含 %q，got=%q", want, got)
		}
	}
}

func TestUnwrapSupportsErrorsIs(t *testing.T) {
	e := New("xgorm", "init", errBase)
	if !errors.Is(e, errBase) {
		t.Fatal("应能通过 errors.Is 找到被包装的原始错误")
	}
}

func TestIsWalksWholeChain(t *testing.T) {
	// 模块之间会互相包装：xgorm 的错误里裹着 config 的错误
	inner := New("config", "load", errBase)
	outer := New("xgorm", "init", inner)

	if !Is(outer, "xgorm") {
		t.Error("应认出最外层模块")
	}
	if !Is(outer, "config") {
		t.Error("应认出链条内层的模块 —— 只看最外层就会漏")
	}
	if Is(outer, "xredis") {
		t.Error("不该认出链条里没有的模块")
	}
}

func TestModuleTakesOutermost(t *testing.T) {
	outer := New("xgorm", "init", New("config", "load", errBase))
	if got := Module(outer); got != "xgorm" {
		t.Errorf("Module 应取最外层（谁最终报出这个错），got=%q", got)
	}
	if got := Module(errBase); got != "" {
		t.Errorf("非本包错误应返回空串，got=%q", got)
	}
}

func TestNewf(t *testing.T) {
	e := Newf("xhttp", "request", "超时 timeout=[%v]", "3s")
	if !strings.Contains(e.Error(), "timeout=[3s]") {
		t.Errorf("Newf 应格式化消息，got=%q", e.Error())
	}
}

func TestErrorMessage_FrameworkErrorsDoNotRepeatPrefix(t *testing.T) {
	// 前缀的作用是让错误落进别人的日志时看得出是谁报的。
	// 模块名本来就是 xone 的那种，再加一次就成了「xone xone init failed」
	if got := New("xone", "init", errBase).Error(); strings.HasPrefix(got, "xone xone") {
		t.Errorf("前缀重复了：%s", got)
	}
	if got := New("xgorm", "init", errBase).Error(); !strings.HasPrefix(got, "xone xgorm") {
		t.Errorf("其它模块该带前缀：%s", got)
	}
}

func TestIs_ReturnsFalseForNilAndForeignErrors(t *testing.T) {
	if Is(nil, "xgorm") {
		t.Error("nil 不属于任何模块")
	}
	if Is(errBase, "xgorm") {
		t.Error("不是本包的错误不该被认成某个模块的")
	}
}

func TestIs_StopsAtForeignErrorMidChain(t *testing.T) {
	// 中间裹着一层标准库错误时，外层还是本包的错误，
	// 遍历要在那一层停下来而不是空转或误判
	outer := New("xgorm", "query", fmt.Errorf("driver: %w", errBase))
	if !Is(outer, "xgorm") {
		t.Error("最外层应当认得出")
	}
	if Is(outer, "xconfig") {
		t.Error("链条里没有 xconfig，不该认出来")
	}
}

func TestIs_errors_JoinFindsEveryBranch(t *testing.T) {
	// Run 把启动错误和关闭错误 Join 在一起返回。errors.As 只会拿到第一个 *Error，
	// 再顺着它的 .Err 往里走——Join 里排在后面的兄弟从来没被看过
	joined := errors.Join(New("xgin", "start", errBase), New("xgorm", "close", errBase))
	for _, m := range []string{"xgin", "xgorm"} {
		if !Is(joined, m) {
			t.Errorf("Join 里的 %s 该认得出，err=%v", m, joined)
		}
	}
	if Is(joined, "xredis") {
		t.Error("不在 Join 里的模块不该认出来")
	}
}

func TestIs_JoinNestedInsideErrorIsFound(t *testing.T) {
	// 本包错误裹着一个 Join、Join 的第二个分支里又裹着别的模块：
	// 两种 Unwrap 交替出现时仍要走遍整棵树
	inner := fmt.Errorf("partial: %w", errors.Join(errBase, New("xredis", "close", errBase)))
	outer := New("xone", "stop", inner)
	if !Is(outer, "xredis") {
		t.Errorf("树的深处也该认得出，err=%v", outer)
	}
}

func TestModule_JoinTakesFirstXerror(t *testing.T) {
	// Run 总是把主因放在 Join 的最前面，所以「谁报的」取深度优先遇到的第一个。
	// 排在前面的不是本包错误时跳过它，而不是返回空串
	joined := errors.Join(errBase, New("xgin", "start", errBase), New("xgorm", "close", errBase))
	if got := Module(joined); got != "xgin" {
		t.Errorf("Join 时该取第一个本包错误的模块，got=%q", got)
	}
}

// ---- 一条错误只有一个框 ----

func TestNewf_SameModuleInnerKeepsOnlyOpAndCause(t *testing.T) {
	// 从前是 xone xgorm new failed, err=[instance "a": xone xgorm connect failed, err=[...]]：
	// 模块名说了两遍，使用者要找的原因埋在第二层括号里
	inner := Newf("xgorm", "connect", "cannot reach %s", "db:3306")
	outer := Newf("xgorm", "new", "instance %q: %w", "a", inner)

	want := `xone xgorm new failed, err=[instance "a": connect: cannot reach db:3306]`
	if got := outer.Error(); got != want {
		t.Errorf("\n got=%s\nwant=%s", got, want)
	}
}

func TestNewf_OtherModuleInnerDropsXonePrefix(t *testing.T) {
	// 「这是 xone 报的」最外层说一次就够了
	inner := Newf("xconfig", "config", "invalid config %s", "App")
	outer := Newf("xone", "start", "%s: %w", "xapp.loadConfig", inner)

	want := "xone start failed, err=[xapp.loadConfig: xconfig config failed, err=[invalid config App]]"
	if got := outer.Error(); got != want {
		t.Errorf("\n got=%s\nwant=%s", got, want)
	}
}

func TestNew_ReturnsSameModuleErrorAsIs(t *testing.T) {
	// 一个模块边界只有一层框，而且留下的是更具体的那个 op
	inner := Newf("xgorm", "connect", "cannot reach db")
	if got := New("xgorm", "init", inner); got != inner {
		t.Errorf("同模块再包一层应原样返回里层，got=%v", got)
	}
	if got := New("xgin", "init", inner).Error(); strings.Count(got, "xone") != 1 {
		t.Errorf("跨模块包一层时 xone 只该出现一次，got=%s", got)
	}
}

func TestNewf_ErrorChainSurvivesFolding(t *testing.T) {
	inner := Newf("xgorm", "connect", "dial: %w", errBase)
	outer := Newf("xone", "start", "%s: %w", "xgorm.init", Newf("xgorm", "new", "instance %q: %w", "a", inner))

	if !errors.Is(outer, errBase) {
		t.Error("errors.Is 应穿透折叠后的每一层")
	}
	var xe *Error
	if !errors.As(outer, &xe) || xe.Module != "xone" {
		t.Errorf("errors.As 取到的应是最外层，got=%+v", xe)
	}
	if !Is(outer, "xgorm") {
		t.Error("Is 应认得出折叠进里层的模块")
	}
}

func TestNewf_DoesNotMutateCallerArgs(t *testing.T) {
	// args... 展开传进来时它就是调用方那一个切片，改了它调用方会看到一个陌生的类型
	inner := Newf("xgorm", "connect", "x")
	args := []any{inner}
	_ = Newf("xgorm", "new", "%w", args...)
	if args[0] != any(inner) {
		t.Errorf("调用方的切片被改了，got=%T", args[0])
	}
}

func TestNew_TypedNilTreatedAsNoCauseNotPanic(t *testing.T) {
	// var e *Error 从某个函数返回、没判空就被包了一层——这是常见的笔误，
	// 报错路径上的 panic 会把真正的故障盖掉
	var nilErr *Error
	e := New("xgorm", "init", nilErr)
	if got, want := e.Error(), "xone xgorm init failed"; got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
	if e.Err != nil {
		t.Errorf("带类型的 nil 不该被存进 Err，got=%#v", e.Err)
	}
	if Is(e, "xredis") || !Is(e, "xgorm") || Module(e) != "xgorm" {
		t.Errorf("Is / Module 应照常工作，got Is(xredis)=%v Is(xgorm)=%v Module=%q",
			Is(e, "xredis"), Is(e, "xgorm"), Module(e))
	}
}

func TestNewf_TypedNilArgRendersAsNilNotPanic(t *testing.T) {
	var nilErr *Error
	for _, verb := range []string{"%v", "%w"} {
		for _, module := range []string{"xgorm", "xredis"} { // 同模块、别的模块两条路都走一遍
			e := Newf(module, "init", "cause="+verb, nilErr)
			if got, want := e.Error(), "xone "+module+" init failed, err=[cause=<nil>]"; got != want {
				t.Errorf("%s/%s got=%q want=%q", verb, module, got, want)
			}
			if Is(e, "xhttp") || !Is(e, module) || Module(e) != module {
				t.Errorf("%s/%s Is / Module 应照常工作", verb, module)
			}
		}
	}
	if Is(nilErr, "xgorm") || Module(nilErr) != "" {
		t.Error("带类型的 nil 本身不属于任何模块")
	}
}
