package trans

import (
	"errors"
	"testing"
	"unicode"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"

	"github.com/jupiter-eng/xone/xerror"
)

type form struct {
	Name  string `binding:"required" json:"name"`
	Email string `binding:"required,email" json:"email"`
	Age   int    `binding:"gte=18" json:"age"`
}

// validateForm runs gin's validator to obtain real ValidationErrors.
func validateForm(t *testing.T, f form) error {
	t.Helper()
	return binding.Validator.ValidateStruct(&f)
}

func TestToZH_TranslatesValidationErrors(t *testing.T) {
	if err := RegisterZH(); err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	err := validateForm(t, form{Age: 10})
	if err == nil {
		t.Fatal("this input should fail validation")
	}
	msg := Msg(err)
	if msg == "" {
		t.Fatal("there should be an error message")
	}
	// A successful translation shows up as Han characters in the message rather than validator's
	// English original. The check is by Unicode range, so this file needs no Chinese of its own.
	if !containsHan(msg) {
		t.Errorf("the message should be translated into Chinese, got=%q", msg)
	}
}

func TestToZH_StableFieldOrder(t *testing.T) {
	// The same set of validation errors has to produce the same message every time, or an
	// endpoint's error text follows the map iteration order and neither tests nor alerts line up.
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	err := validateForm(t, form{Age: 10})

	first := Msg(err)
	for i := 0; i < 20; i++ {
		if got := Msg(err); got != first {
			t.Fatalf("the same error produced two different messages:\n%q\n%q", first, got)
		}
	}
}

func TestToZH_NonValidationErrorReturnedAsIs(t *testing.T) {
	// The caller need not check first whether this is a validation error.
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	orig := errors.New("database unreachable")
	if got := ToZH(orig); got != orig {
		t.Errorf("anything that is not a validation error comes back unchanged, got=%v", got)
	}
	if ToZH(nil) != nil {
		t.Error("nil should return nil")
	}
	if Msg(nil) != "" {
		t.Error("the message for nil should be an empty string")
	}
}

func TestToZH_ReturnedAsIsWhenNotRegistered(t *testing.T) {
	// A service without translation enabled can still call it and never gets an empty message.
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
	// != cannot be used: ValidationErrors is a slice type and comparing the interfaces panics.
	// What is actually asserted here is that it was not wrapped into a translation result.
	var wrapped *Error
	if errors.As(ToZH(err), &wrapped) {
		t.Error("without a registered translator it must not be wrapped into a translation result")
	}
}

// otherValidator is a validator that is not go-playground/validator.
type otherValidator struct{}

func (otherValidator) ValidateStruct(any) error { return nil }
func (otherValidator) Engine() any              { return "not a validator" }

func TestRegisterZH_FailureReportsXginRegister(t *testing.T) {
	// The caller uses xerror.Is / Module to tell who reported it, and op comes from the fixed set
	// as register: registering a translator is the same kind of action as registering a metric or a
	// dialect, and the new it used to say did not match.
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
		t.Fatalf("this should be an xgin register xerror, got=%#v", err)
	}
}

func TestRegisterZH_RepeatedRegisterIsNoOp(t *testing.T) {
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := RegisterZH(); err != nil {
			t.Errorf("registering again must not fail: %v", err)
		}
	}
}

func TestError_KeepsOriginalError(t *testing.T) {
	// After translation, errors.As still has to yield the original validation error, or the caller
	// cannot do anything finer per field.
	if err := RegisterZH(); err != nil {
		t.Fatal(err)
	}
	translated := ToZH(validateForm(t, form{Age: 10}))

	var ves validator.ValidationErrors
	if !errors.As(translated, &ves) {
		t.Fatal("the original ValidationErrors should still be reachable after translation")
	}
	if len(ves) == 0 {
		t.Error("the original error should carry the field information")
	}
}

func TestToZH_ConcurrencySafe(t *testing.T) {
	// ToZH reads translator during a request while RegisterZH is an exported API the caller may
	// call whenever it likes.
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

// containsHan reports whether s holds at least one Han character, which is how this file checks a
// message was translated without spelling any Chinese out.
func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
