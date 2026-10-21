package xerror

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errBase = errors.New("the underlying error")

func TestErrorMessage(t *testing.T) {
	e := New("xgorm", "init", errBase)
	got := e.Error()
	// The prefix is the framework name xone; it was once mistyped as xtwo -- it appears in every error this package produces.
	for _, want := range []string{"xone ", "xgorm", "init", "the underlying error"} {
		if !strings.Contains(got, want) {
			t.Errorf("the error message must contain %q, got=%q", want, got)
		}
	}
}

func TestUnwrapSupportsErrorsIs(t *testing.T) {
	e := New("xgorm", "init", errBase)
	if !errors.Is(e, errBase) {
		t.Fatal("errors.Is must find the wrapped original error")
	}
}

func TestIsWalksWholeChain(t *testing.T) {
	// Modules wrap each other: an xgorm error holds a config error inside.
	inner := New("config", "load", errBase)
	outer := New("xgorm", "init", inner)

	if !Is(outer, "xgorm") {
		t.Error("the outermost module must be recognized")
	}
	if !Is(outer, "config") {
		t.Error("a module further inside the chain must be recognized -- looking only at the outermost layer would miss it")
	}
	if Is(outer, "xredis") {
		t.Error("a module that is not in the chain must not be recognized")
	}
}

func TestModuleTakesOutermost(t *testing.T) {
	outer := New("xgorm", "init", New("config", "load", errBase))
	if got := Module(outer); got != "xgorm" {
		t.Errorf("Module must take the outermost layer (who finally reported this), got=%q", got)
	}
	if got := Module(errBase); got != "" {
		t.Errorf("an error from another package must give an empty string, got=%q", got)
	}
}

func TestNewf(t *testing.T) {
	e := Newf("xhttp", "request", "timed out timeout=[%v]", "3s")
	if !strings.Contains(e.Error(), "timeout=[3s]") {
		t.Errorf("Newf must format the message, got=%q", e.Error())
	}
}

func TestErrorMessage_FrameworkErrorsDoNotRepeatPrefix(t *testing.T) {
	// The prefix exists so an error landing in somebody else's log says who reported it.
	// For one whose module name is already xone, adding it again would read "xone xone init failed".
	if got := New("xone", "init", errBase).Error(); strings.HasPrefix(got, "xone xone") {
		t.Errorf("the prefix is duplicated: %s", got)
	}
	if got := New("xgorm", "init", errBase).Error(); !strings.HasPrefix(got, "xone xgorm") {
		t.Errorf("another module must carry the prefix: %s", got)
	}
}

func TestIs_ReturnsFalseForNilAndForeignErrors(t *testing.T) {
	if Is(nil, "xgorm") {
		t.Error("nil belongs to no module")
	}
	if Is(errBase, "xgorm") {
		t.Error("an error from another package must not be taken for one of a module")
	}
}

func TestIs_StopsAtForeignErrorMidChain(t *testing.T) {
	// With a standard-library error wrapped in between, the outer layer is still an error from this package,
	// and the walk must stop at that layer rather than spinning or getting it wrong.
	outer := New("xgorm", "query", fmt.Errorf("driver: %w", errBase))
	if !Is(outer, "xgorm") {
		t.Error("the outermost layer must be recognized")
	}
	if Is(outer, "xconfig") {
		t.Error("there is no xconfig in the chain, so it must not be recognized")
	}
}

func TestIs_errors_JoinFindsEveryBranch(t *testing.T) {
	// Run returns the start error and the stop error joined together. errors.As only reaches the first *Error
	// and then follows its .Err inward -- the siblings later in the Join were never looked at.
	joined := errors.Join(New("xgin", "start", errBase), New("xgorm", "close", errBase))
	for _, m := range []string{"xgin", "xgorm"} {
		if !Is(joined, m) {
			t.Errorf("%s inside the Join must be recognized, err=%v", m, joined)
		}
	}
	if Is(joined, "xredis") {
		t.Error("a module that is not in the Join must not be recognized")
	}
}

func TestIs_JoinNestedInsideErrorIsFound(t *testing.T) {
	// An error from this package wrapping a Join whose second branch wraps another module:
	// the whole tree must still be walked when the two kinds of Unwrap alternate.
	inner := fmt.Errorf("partial: %w", errors.Join(errBase, New("xredis", "close", errBase)))
	outer := New("xone", "stop", inner)
	if !Is(outer, "xredis") {
		t.Errorf("it must be recognized deep in the tree too, err=%v", outer)
	}
}

func TestModule_JoinTakesFirstXerror(t *testing.T) {
	// Run always puts the main cause first in the Join, so "who reported it" takes the first one found depth-first.
	// An error from another package in front of it is skipped rather than producing an empty string.
	joined := errors.Join(errBase, New("xgin", "start", errBase), New("xgorm", "close", errBase))
	if got := Module(joined); got != "xgin" {
		t.Errorf("across a Join it must take the module of the first error from this package, got=%q", got)
	}
}

// ---- one frame per error ----

func TestNewf_SameModuleInnerKeepsOnlyOpAndCause(t *testing.T) {
	// It used to read xone xgorm new failed, err=[instance "a": xone xgorm connect failed, err=[...]]:
	// the module name said twice, and the cause the caller is looking for buried in the second pair of brackets.
	inner := Newf("xgorm", "connect", "cannot reach %s", "db:3306")
	outer := Newf("xgorm", "new", "instance %q: %w", "a", inner)

	want := `xone xgorm new failed, err=[instance "a": connect: cannot reach db:3306]`
	if got := outer.Error(); got != want {
		t.Errorf("\n got=%s\nwant=%s", got, want)
	}
}

func TestNewf_OtherModuleInnerDropsXonePrefix(t *testing.T) {
	// "this came from xone" only needs saying once, at the outermost layer.
	inner := Newf("xconfig", "config", "invalid config %s", "App")
	outer := Newf("xone", "start", "%s: %w", "xapp.loadConfig", inner)

	want := "xone start failed, err=[xapp.loadConfig: xconfig config failed, err=[invalid config App]]"
	if got := outer.Error(); got != want {
		t.Errorf("\n got=%s\nwant=%s", got, want)
	}
}

func TestNew_ReturnsSameModuleErrorAsIs(t *testing.T) {
	// One module boundary, one frame, and what is kept is the more specific op.
	inner := Newf("xgorm", "connect", "cannot reach db")
	if got := New("xgorm", "init", inner); got != inner {
		t.Errorf("wrapping the same module again must return the inner one as it is, got=%v", got)
	}
	if got := New("xgin", "init", inner).Error(); strings.Count(got, "xone") != 1 {
		t.Errorf("across modules xone must appear only once, got=%s", got)
	}
}

func TestNewf_ErrorChainSurvivesFolding(t *testing.T) {
	inner := Newf("xgorm", "connect", "dial: %w", errBase)
	outer := Newf("xone", "start", "%s: %w", "xgorm.init", Newf("xgorm", "new", "instance %q: %w", "a", inner))

	if !errors.Is(outer, errBase) {
		t.Error("errors.Is must see through every folded layer")
	}
	var xe *Error
	if !errors.As(outer, &xe) || xe.Module != "xone" {
		t.Errorf("errors.As must reach the outermost layer, got=%+v", xe)
	}
	if !Is(outer, "xgorm") {
		t.Error("Is must recognize a module folded into an inner layer")
	}
}

func TestNewf_DoesNotMutateCallerArgs(t *testing.T) {
	// Passing args... hands over the caller's own slice, and modifying it would show them an unfamiliar type.
	inner := Newf("xgorm", "connect", "x")
	args := []any{inner}
	_ = Newf("xgorm", "new", "%w", args...)
	if args[0] != any(inner) {
		t.Errorf("the caller's slice was modified, got=%T", args[0])
	}
}

func TestNew_TypedNilTreatedAsNoCauseNotPanic(t *testing.T) {
	// A var e *Error returned from some function and wrapped without a nil check -- a common slip, and a panic
	// on the error path would bury the real failure.
	var nilErr *Error
	e := New("xgorm", "init", nilErr)
	if got, want := e.Error(), "xone xgorm init failed"; got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
	if e.Err != nil {
		t.Errorf("a typed nil must not be stored in Err, got=%#v", e.Err)
	}
	if Is(e, "xredis") || !Is(e, "xgorm") || Module(e) != "xgorm" {
		t.Errorf("Is / Module must keep working, got Is(xredis)=%v Is(xgorm)=%v Module=%q",
			Is(e, "xredis"), Is(e, "xgorm"), Module(e))
	}
}

func TestNewf_TypedNilArgRendersAsNilNotPanic(t *testing.T) {
	var nilErr *Error
	for _, verb := range []string{"%v", "%w"} {
		for _, module := range []string{"xgorm", "xredis"} { // both paths: the same module and another one
			e := Newf(module, "init", "cause="+verb, nilErr)
			if got, want := e.Error(), "xone "+module+" init failed, err=[cause=<nil>]"; got != want {
				t.Errorf("%s/%s got=%q want=%q", verb, module, got, want)
			}
			if Is(e, "xhttp") || !Is(e, module) || Module(e) != module {
				t.Errorf("%s/%s Is / Module must keep working", verb, module)
			}
		}
	}
	if Is(nilErr, "xgorm") || Module(nilErr) != "" {
		t.Error("a typed nil itself belongs to no module")
	}
}
