package trans

import (
	"errors"
	"strings"
	"testing"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"

	"github.com/jupiter-eng/xone/xerror"
)

type form struct {
	Name  string `binding:"required" json:"name"`
	Email string `binding:"required,email" json:"email"`
	Age   int    `binding:"gte=18" json:"age"`
}

// validateForm 走 gin 的校验器，拿到真实的 ValidationErrors
func validateForm(t *testing.T, f form) error {
	t.Helper()
	return binding.Validator.ValidateStruct(&f)
}

func TestToZH_TranslatesValidationErrors(t *testing.T) {
	if err := RegisterZH(); err != nil {
		t.Fatalf("注册失败：%v", err)
	}

	err := validateForm(t, form{Age: 10})
	if err == nil {
		t.Fatal("这份数据应当校验失败")
	}
	msg := Msg(err)
	if msg == "" {
		t.Fatal("应当有错误文案")
	}
	// 翻译成功的标志是文案里有中文，而不是 validator 的英文原文
	if !strings.ContainsAny(msg, "必填必须填写为") {
		t.Errorf("应当翻成中文，got=%q", msg)
	}
}

func TestToZH_StableFieldOrder(t *testing.T) {
	// 同一组校验错误每次都要得到同样的消息，否则接口的错误文案会随
	// map 遍历顺序变化，测试和告警都对不上
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	err := validateForm(t, form{Age: 10})

	first := Msg(err)
	for i := 0; i < 20; i++ {
		if got := Msg(err); got != first {
			t.Fatalf("同一个错误两次得到不同文案：\n%q\n%q", first, got)
		}
	}
}

func TestToZH_NonValidationErrorReturnedAsIs(t *testing.T) {
	// 调用方不必先判断这是不是校验错误
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	orig := errors.New("数据库连不上")
	if got := ToZH(orig); got != orig {
		t.Errorf("非校验错误应原样返回，got=%v", got)
	}
	if ToZH(nil) != nil {
		t.Error("nil 应返回 nil")
	}
	if Msg(nil) != "" {
		t.Error("nil 的文案应是空串")
	}
}

func TestToZH_ReturnedAsIsWhenNotRegistered(t *testing.T) {
	// 没启用翻译的服务照样能调用，不会拿到空文案
	mu.Lock()
	old := translator
	translator = nil
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		translator = old
		mu.Unlock()
	})

	err := validateForm(t, form{Age: 10})
	// 不能用 != 比：ValidationErrors 是切片类型，接口比较会 panic。
	// 这里真正要断言的是「没有被包装成翻译结果」
	var wrapped *Error
	if errors.As(ToZH(err), &wrapped) {
		t.Error("没注册翻译器时不该包装成翻译结果")
	}
}

// otherValidator 一个不是 go-playground/validator 的校验器
type otherValidator struct{}

func (otherValidator) ValidateStruct(any) error { return nil }
func (otherValidator) Engine() any              { return "not a validator" }

func TestRegisterZH_FailureReportsXginRegister(t *testing.T) {
	// 调用方靠 xerror.Is / Module 判断「这是谁报的」，op 是固定词表里的 register：
	// 注册翻译器跟注册指标、注册方言是同一类动作，原先写的 new 对不上
	mu.Lock()
	old := translator
	translator = nil
	mu.Unlock()
	oldV := binding.Validator
	binding.Validator = otherValidator{}
	t.Cleanup(func() {
		binding.Validator = oldV
		mu.Lock()
		translator = old
		mu.Unlock()
	})

	err := RegisterZH()
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xgin" || xe.Op != "register" {
		t.Fatalf("该是 xgin register 的 xerror，got=%#v", err)
	}
}

func TestRegisterZH_RepeatedRegisterIsNoOp(t *testing.T) {
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := RegisterZH(); err != nil {
			t.Errorf("重复注册不该报错：%v", err)
		}
	}
}

func TestError_KeepsOriginalError(t *testing.T) {
	// 翻译之后仍然要能 errors.As 出原始的校验错误，
	// 否则调用方没法按字段做更细的处理
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	translated := ToZH(validateForm(t, form{Age: 10}))

	var ves validator.ValidationErrors
	if !errors.As(translated, &ves) {
		t.Fatal("翻译后应当还能取出原始的 ValidationErrors")
	}
	if len(ves) == 0 {
		t.Error("原始错误里应当有字段信息")
	}
}

func TestToZH_ConcurrencySafe(t *testing.T) {
	// ToZH 在请求期读 translator，RegisterZH 是公开 API、调用时机由使用者决定
	err := validateForm(t, form{Age: 10})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(reg bool) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if reg {
					RegisterZH()
				} else {
					Msg(err)
				}
			}
		}(i%2 == 0)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
