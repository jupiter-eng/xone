package xhook_test

import (
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xhook/internal/viahelper"
)

// viaHelper registers once through the helper package while this test package initializes, and snapshots the registration board on the spot.
//
// Package-level variable initialization also runs inside init, which is exactly when an integration package registers its hooks.
// It has to be taken on the spot: by the time the test functions start, other cases have long since cleared the board.
var viaHelper = func() []hook.Entry {
	viahelper.Register()
	return hook.Start()
}()

func TestBeforeStart_HookViaHelperPkgAttributedToRegisteringPkg(t *testing.T) {
	// The hook function is a closure inside the helper package, so the name belongs to the helper. Recognizing
	// the package by name would make every package registering through it the same one: one package's start hook fails and another package's stop hook is skipped with it.
	const want = "github.com/jupiter-eng/xone/xhook_test"
	var found bool
	for _, e := range viaHelper {
		if !strings.Contains(e.Name, "viahelper") {
			continue
		}
		found = true
		if e.Pkg != want {
			t.Errorf("it must be credited to the registering package %s, got=%s", want, e.Pkg)
		}
	}
	if !found {
		t.Fatalf("the hook registered through the helper package was not found, board=%v", viaHelper)
	}
}
