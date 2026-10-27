// Package trans translates validator's validation errors into Chinese.
//
// It is off by default; turn it on in the config:
//
//	XGin:
//	  ZHTranslations: true
//
// After that, inside a handler:
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

// mu guards reads and writes of translator.
//
// Locking the write side alone is not enough: ToZH reads it during a request while RegisterZH is
// an exported API the caller may call whenever it likes, and -race reports the data race between
// the two directly.
var (
	mu         sync.RWMutex
	translator ut.Translator
)

// RegisterZH registers the Chinese translator.
//
// Calling it again after it succeeded does nothing, and a failure can be retried.
// Errors are xerror values recorded under the module xgin (this package is part of it) with the
// operation register.
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

	// binding.Validator is an interface, and gin.DisableBindValidation sets it to nil: asking a nil
	// interface for its Engine panics, and this runs during assembly, where a panic takes the
	// server down over a translation nobody could have used anyway.
	if binding.Validator == nil {
		return xerror.Newf("xgin", "register", "gin validator is disabled (gin.DisableBindValidation), there is nothing to translate")
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

// ToZH translates a validation error into Chinese.
//
// Anything that is not a validation error, and anything at all when no translator was registered,
// comes back unchanged, so the caller need not check first whether this is enabled.
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

	// Sorted by field name: the same set of validation errors has to produce the same message every
	// time, or an endpoint's error text follows the map iteration order and neither tests nor alerts
	// line up.
	msgs := make([]string, 0, len(byField))
	for _, field := range slices.Sorted(maps.Keys(byField)) {
		msgs = append(msgs, strings.Join(byField[field], ", "))
	}
	return &Error{Msg: strings.Join(msgs, ", "), Cause: err}
}

// Msg is the translated error text, and an empty string when err is nil.
func Msg(err error) string {
	if err = ToZH(err); err != nil {
		return err.Error()
	}
	return ""
}

// Error is a translated validation error that keeps the original for errors.Is / errors.As.
type Error struct {
	Msg   string
	Cause error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Cause }
