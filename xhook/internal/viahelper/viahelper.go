// Package viahelper is only for xhook's tests: a helper package that registers hooks on behalf of others.
//
// What it registers is a closure built here, so the function name belongs to this package while the package doing
// the registering is the one calling Register. The framework must credit the hook to the latter, otherwise every package registering through a helper is taken for the same one.
package viahelper

import (
	"context"

	"github.com/jupiter-eng/xone/xhook"
)

// Register registers a start hook that does nothing, on behalf of the caller.
//
// Inlining is forbidden: once inlined, this closure is instantiated at the caller and its name becomes the
// caller's, so the test could no longer tell "recognize the package by name" from "recognize it by call stack".
// A real helper package has no such guarantee -- a slightly larger function is not inlined, and a method value never is.
//
//go:noinline
func Register() {
	xhook.BeforeStart(func(context.Context) error { return nil })
}
