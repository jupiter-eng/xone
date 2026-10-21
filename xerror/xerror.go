// Package xerror provides the one error type: it carries a module name and an operation name, and supports errors.Is / As correctly.
package xerror

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Error is that type, holding the module name, the operation name and the original error.
type Error struct {
	Module string // the module name, such as "xconfig" or "xgorm"
	Op     string // the operation name, such as "config" or "close"
	Err    error  // the original error
}

// Error renders as "xone {module} {op} failed, err=[...]".
func (e *Error) Error() string { return e.render(true) }

// render's prefix says whether to include the leading "xone ".
//
// Errors the framework reports itself already have xone as their module name, and the prefix would make that
// read "xone xone init failed". The prefix exists so an error landing in somebody else's log says who reported
// it: once, at the outermost layer, is enough, and repeating it in the nested layers is only noise. See nested.
func (e *Error) render(prefix bool) string {
	var b strings.Builder
	b.Grow(32 + len(e.Module) + len(e.Op))
	if prefix && e.Module != "xone" {
		b.WriteString("xone ")
	}
	b.WriteString(e.Module)
	b.WriteByte(' ')
	b.WriteString(e.Op)
	b.WriteString(" failed")
	if e.Err != nil {
		b.WriteString(", err=[")
		b.WriteString(e.Err.Error())
		b.WriteByte(']')
	}
	return b.String()
}

// Unwrap makes errors.Is / errors.As walk the chain.
func (e *Error) Unwrap() error {
	return e.Err
}

// New creates an Error.
//
// When err is already an Error from the same module it is returned as it is: one module boundary should hold one
// frame only. Wrapping again reads xone xgorm init failed, err=[xone xgorm connect failed, ...] -- no more
// information, twice the noise, and the more specific op buried in the inner layer.
//
// A typed nil (var e *Error; New(m, op, e)) counts as no cause: it reports nothing, and storing it in Err as it
// is would dereference it while rendering and while comparing.
func New(module, op string, err error) *Error {
	if xe, ok := err.(*Error); ok {
		if xe == nil {
			return &Error{Module: module, Op: op}
		}
		if xe.Module == module {
			return xe
		}
	}
	return &Error{Module: module, Op: op, Err: nest(module, err)}
}

// Newf creates an Error with a formatted message.
//
// An Error among the arguments has its duplicated parts removed while rendering, see nested; errors.Is / As
// still see through it. A typed nil is not folded and is left for fmt to render as <nil>.
func Newf(module, op, format string, args ...any) *Error {
	var own []any // the caller's slice is not modified: passing args... hands over the caller's own slice
	for i, a := range args {
		if xe, ok := a.(*Error); ok && xe != nil {
			if own == nil {
				own = slices.Clone(args)
			}
			own[i] = nest(module, xe)
		}
	}
	if own != nil {
		args = own
	}
	return &Error{Module: module, Op: op, Err: fmt.Errorf(format, args...)}
}

// nest replaces an Error that is about to be wrapped in a module layer with a nested; other errors are returned as they are.
func nest(module string, err error) error {
	if xe, ok := err.(*Error); ok {
		return nested{e: xe, same: xe.Module == module}
	}
	return err
}

// nested is an Error wrapped inside another Error, rendered with the duplicated parts removed:
// one from the same module keeps only "op: cause", and one from another module drops the leading "xone ".
//
// When an error lands in a caller's log, only one place should tell them "this came from xone". A full frame
// at every layer reads xone start failed, err=[... xone xconfig config failed, err=[...]] -- and the root
// cause they are looking for is buried in the innermost brackets.
type nested struct {
	e    *Error
	same bool
}

func (n nested) Error() string {
	if !n.same {
		return n.e.render(false)
	}
	if n.e.Err == nil {
		return n.e.Op + " failed"
	}
	return n.e.Op + ": " + n.e.Err.Error()
}

func (n nested) Unwrap() error { return n.e }

// Is reports whether the error tree of err holds an error produced by the given module.
//
// It walks the whole tree rather than looking only at the outermost layer: modules wrap each other's errors
// (an xgorm initialization failure holds an xconfig error inside), and comparing only the first Error would
// make Is(err, "xconfig") return false on such a chain, which is not what this function means.
//
// It is a "tree" because of errors.Join: xone.Run returns the start error and the stop error joined together.
// Using errors.As to find the first *Error and then following its .Err inward would never see the siblings
// that come later in the Join. So, like errors.Is, this accepts both kinds of Unwrap.
func Is(err error, module string) bool {
	if xe, ok := err.(*Error); ok {
		if xe == nil {
			return false // a typed nil: it reports nothing, and there is no inner layer to walk
		}
		if xe.Module == module {
			return true
		}
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return Is(u.Unwrap(), module)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if Is(e, module) {
				return true
			}
		}
	}
	return false
}

// Module returns the module name of the outermost error, or an empty string when it is not an error from this package.
//
// It takes the outermost rather than walking the chain: an error is wrapped on its way up, so the outermost layer
// says who finally reported it, which is what the caller branches on. To ask whether a module is involved anywhere in the chain, use Is.
//
// Across an errors.Join it takes the first error from this package in depth-first order (the same order as
// errors.As), skipping errors from other packages before it. xone.Run always puts the main cause first in the
// Join, so what comes back is that main cause.
func Module(err error) string {
	var xe *Error
	if errors.As(err, &xe) && xe != nil {
		return xe.Module
	}
	return ""
}
