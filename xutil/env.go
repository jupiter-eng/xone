package xutil

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unsafe"
)

// placeholder is ${NAME} or ${NAME:default}. A default runs up to the first }, so it never holds one; it may
// hold a colon (${URL:http://localhost:8080}) and it may be empty (${PW:}).
var placeholder = regexp.MustCompile(`\$\{([^}:]+)(?::([^}]*))?\}`)

// ExpandEnv replaces the ${NAME} and ${NAME:default} placeholders in every string reachable from v with the
// value of the environment variable NAME, in place.
//
// v is a non-nil pointer, usually to a struct. From there it follows every level: nested and embedded
// structs, pointers, slices, arrays, map values and the values held in interfaces. A named string type
// (type Mode string) counts as a string. Unexported fields, map keys and []byte are left alone, and so is
// everything that is not a string.
//
// A placeholder may sit anywhere in a string and a string may hold several: "http://${HOST}:${PORT:8080}/api".
// A variable that is set wins over the default, also when it is set to the empty string. The replacement is
// not expanded again, so a variable whose value happens to hold ${ comes through as it is.
//
// A placeholder with no default whose variable is unset is an error naming every such variable, sorted and
// listed once each. Everything else is still expanded and that placeholder stays as written, so the error
// tells which variables to set and the struct shows where they are used; v is not rolled back.
//
// A value is expanded once however many paths lead to it, so a structure that points back at itself ends,
// and so does a chain of any length: the walk does not recurse. Only a string whose text changes is
// written, and a map entry or an interface only when a string under it changed.
//
// Three things to know before calling it:
//   - Expand trusted text only, such as a configuration the program ships with. Whoever writes the string
//     chooses the variable, so on input from outside ${DB_PASSWORD} reads the process environment.
//   - Call it once per value. A second call sees what the first one put there, and a variable whose value
//     holds ${ would be expanded after all.
//   - There is no escape: a literal ${NAME} cannot be written, and ${A${B}} is not nested expansion.
func ExpandEnv(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("expand env: want a non-nil pointer, got %T", v)
	}
	e := expander{seen: map[mark]struct{}{}}
	e.run(rv)
	if len(e.missing) > 0 {
		slices.Sort(e.missing)
		return errors.New("expand env: environment variable not set: " + strings.Join(slices.Compact(e.missing), ", "))
	}
	return nil
}

// mark identifies something already walked: a string or an interface by its own address, a pointer by
// where it points, a map by its table, a slice by its first element and length (two slices of one array
// may start at the same element).
//
// A pointer and a slice carry their type, because a pointer to a struct and a pointer to its first field
// are the same address. The other three do not: one map converted to a second named type, or one string
// seen through *string and through a pointer to a named string type, is still one value.
//
// ptr is an unsafe.Pointer rather than the uintptr reflect.Value.Pointer returns, and that is the reason
// this file imports unsafe: the copies made for interface and map values are garbage once written back,
// and a uintptr would let the next copy be allocated at a marked address and be skipped. Held as a
// pointer, the mark keeps the address from being reused.
type mark struct {
	kind reflect.Kind
	typ  reflect.Type
	ptr  unsafe.Pointer
	len  int
}

// step is one entry of the work stack. With dst unset, v is still to be walked. With dst set, v is a copy
// that has been walked and goes back where it came from: into the map dst under key, or into the
// interface dst when key is unset -- provided a string was replaced since the copy was made, which
// replaced records.
type step struct {
	v, dst, key reflect.Value
	replaced    int
}

// expander walks with a work stack of its own rather than by recursion: a goroutine stack that runs out
// is a fatal error no caller can recover from, and a linked list of a million nodes gets there.
type expander struct {
	missing  []string
	seen     map[mark]struct{}
	todo     []step
	replaced int // strings whose text changed so far
}

// first reports whether m is seen for the first time. It is what ends a cycle, and what keeps a value
// reachable along two paths from being expanded twice -- the second pass would expand what the first one
// put there.
func (e *expander) first(m mark) bool {
	if _, ok := e.seen[m]; ok {
		return false
	}
	e.seen[m] = struct{}{}
	return true
}

func (e *expander) run(root reflect.Value) {
	e.push(root)
	for len(e.todo) > 0 {
		s := e.todo[len(e.todo)-1]
		e.todo = e.todo[:len(e.todo)-1]
		switch {
		case !s.dst.IsValid():
			e.visit(s.v)
		case s.replaced == e.replaced:
			// Nothing under the copy changed, so there is nothing to write. A map or an interface that
			// holds no placeholder is only ever read, which matters when something else is reading it.
		case s.key.IsValid():
			s.dst.SetMapIndex(s.key, s.v)
		default:
			s.dst.Set(s.v)
		}
	}
}

// push handles a string on the spot and queues what may hold one. Everything else is dropped here, so a
// large []int or []byte costs one look at each element and no memory.
func (e *expander) push(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		if !v.CanSet() {
			return
		}
		s := v.String()
		if !strings.Contains(s, "${") || !e.first(mark{kind: reflect.String, ptr: v.Addr().UnsafePointer()}) {
			return
		}
		if expanded := e.expand(s); expanded != s {
			v.SetString(expanded)
			e.replaced++
		}
	case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Slice, reflect.Array, reflect.Map:
		e.todo = append(e.todo, step{v: v})
	}
}

// pushCopy queues a settable copy of v to be walked and then written back to dst. What an interface holds
// and what a map holds under a key are not addressable, so neither can be expanded where it is. The
// write-back goes on the stack first: it comes off only after everything the copy leads to, so whatever
// was replaced in between was replaced under the copy.
func (e *expander) pushCopy(v, dst, key reflect.Value) {
	cp := reflect.New(v.Type()).Elem()
	cp.Set(v)
	e.todo = append(e.todo, step{v: cp, dst: dst, key: key, replaced: e.replaced})
	e.push(cp)
}

func (e *expander) visit(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() && e.first(mark{kind: reflect.Pointer, typ: v.Type(), ptr: v.UnsafePointer()}) {
			e.push(v.Elem())
		}
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		elem := v.Elem()
		if elem.Kind() == reflect.Pointer {
			e.push(elem)
			return
		}
		if v.CanSet() && e.first(mark{kind: reflect.Interface, ptr: v.Addr().UnsafePointer()}) {
			e.pushCopy(elem, v, reflect.Value{})
		}
	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			if walkable(t.Field(i)) {
				e.push(v.Field(i))
			}
		}
	case reflect.Slice:
		if v.Len() == 0 || !e.first(mark{kind: reflect.Slice, typ: v.Type(), ptr: v.UnsafePointer(), len: v.Len()}) {
			return
		}
		fallthrough
	case reflect.Array:
		for i := range v.Len() {
			e.push(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() || !e.first(mark{kind: reflect.Map, ptr: v.UnsafePointer()}) {
			return
		}
		for it := v.MapRange(); it.Next(); {
			// An entry under a NaN key cannot be looked up, so it cannot be written back either:
			// SetMapIndex would add a second entry beside it on every call.
			if v.MapIndex(it.Key()).IsValid() {
				e.pushCopy(it.Value(), v, it.Key())
			}
		}
	}
}

// walkable reports whether a struct field is one to go into. An unexported field is not: it cannot be
// set, and reflect panics on a write to a map reached through one. The exception is an embedded struct of
// an unexported type, directly or behind a pointer: its exported fields are promoted and settable, the
// same fields encoding/json fills.
func walkable(f reflect.StructField) bool {
	if f.IsExported() {
		return true
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return f.Anonymous && t.Kind() == reflect.Struct
}

func (e *expander) expand(value string) string {
	return placeholder.ReplaceAllStringFunc(value, func(m string) string {
		idx := placeholder.FindStringSubmatchIndex(m)
		name := m[idx[2]:idx[3]]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if idx[4] >= 0 {
			return m[idx[4]:idx[5]]
		}
		e.missing = append(e.missing, name)
		return m
	})
}
