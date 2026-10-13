// Package trans 把 validator 的校验报错翻成中文。
//
// 默认不启用，要用就在配置里打开：
//
//	XGin:
//	  ZHTranslations: true
//
// 之后在 handler 里：
//
//	if err := c.ShouldBindJSON(&req); err != nil {
//		c.JSON(400, gin.H{"error": trans.ToZH(err).Error()})
//	}
package trans

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/locales/zh"
	ut "github.com/go-playground/universal-translator"
	"github.com/go-playground/validator/v10"
	zhtrans "github.com/go-playground/validator/v10/translations/zh"

	"github.com/jupiter-eng/xone/xerror"
)

// mu 保护 translator 的读写
//
// 只锁写侧不够：ToZH 在请求期读它，RegisterZH 是公开 API、调用时机由使用者决定，
// 两者并发时 -race 能直接测出数据竞争。
var (
	mu         sync.RWMutex
	translator ut.Translator
)

// RegisterZH 注册中文翻译器。
//
// 成功之后重复调用是空操作；失败了可以重试。
// 错误都是 xerror，模块记作 xgin（本包是它的一部分），操作是 register。
func RegisterZH() error {
	mu.Lock()
	defer mu.Unlock()

	if translator != nil {
		return nil
	}

	locale := zh.New()
	t, ok := ut.New(locale, locale).GetTranslator("zh")
	if !ok {
		return xerror.Newf("xgin", "register", "zh translator not found")
	}

	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		return xerror.Newf("xgin", "register", "gin validator is %T, not *validator.Validate", binding.Validator.Engine())
	}
	if err := zhtrans.RegisterDefaultTranslations(v, t); err != nil {
		return xerror.Newf("xgin", "register", "zh translations: %w", err)
	}

	translator = t
	return nil
}

// ToZH 把校验错误翻成中文。
//
// 不是校验错误、或者没注册过翻译器时，原样返回——
// 调用方不必先判断有没有启用。
func ToZH(err error) error {
	if err == nil {
		return nil
	}

	mu.RLock()
	t := translator
	mu.RUnlock()
	if t == nil {
		return err
	}

	var ves validator.ValidationErrors
	if !errors.As(err, &ves) {
		return err
	}

	byField := make(map[string][]string, len(ves))
	for _, e := range ves {
		byField[e.Field()] = append(byField[e.Field()], e.Translate(t))
	}

	// 按字段名排序：同一组校验错误每次都要得到同样的消息，
	// 否则接口的错误文案会随 map 遍历顺序变化，测试和告警都对不上
	msgs := make([]string, 0, len(byField))
	for _, field := range slices.Sorted(maps.Keys(byField)) {
		msgs = append(msgs, strings.Join(byField[field], ", "))
	}
	return &Error{Msg: strings.Join(msgs, ", "), Cause: err}
}

// Msg 翻译后的错误文案，err 为 nil 时返回空串
func Msg(err error) string {
	if err = ToZH(err); err != nil {
		return err.Error()
	}
	return ""
}

// Error 翻译后的校验错误，保留原始错误供 errors.Is / errors.As 使用
type Error struct {
	Msg   string
	Cause error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Cause }
