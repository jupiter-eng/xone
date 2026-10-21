package config

import (
	"fmt"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/xerror"
)

// DecodeStrict decodes strictly: a field that is not recognized is an error, not something ignored.
//
// An integration using xconfig.DecodeStrict uses this very implementation: the defaults of a
// collection's elements are spread by each element's own UnmarshalYAML, which has to get the same
// strict checking there, or "a misspelling fails" would quietly stop working inside a collection.
//
// yaml.v3's strict checking exists on a Decoder alone, and a Decoder takes text alone. So it is not
// borrowed: the nodes are walked with target's type first, looking for fields that are not
// recognized and values of the wrong type, and node.Decode follows. It walks the nodes themselves,
// so every error knows which node it is about: the line number is that node's, the file is the one
// load recorded it as coming from, and a value expanded from a placeholder is masked per node.
//
// A target that is not a non-nil pointer returns an xconfig config error (see checkTarget).
// Every other error is either nil or yaml's own; a problem at the field or type level is always a
// *yaml.TypeError -- including the one nested inside an UnmarshalYAML, which yaml then joins into
// the outer list of errors. That one received the node from the configuration file and the error
// said everything there.
func DecodeStrict(node *yaml.Node, target any) error {
	if err := checkTarget(target); err != nil {
		return err
	}
	c := newChecker()
	if err := c.walk(node, reflect.TypeOf(target)); err != nil {
		return err
	}
	if len(c.errs) > 0 {
		return &yaml.TypeError{Errors: c.errs}
	}
	return node.Decode(target)
}

// checkTarget: the decoding target has to be a non-nil pointer.
//
// With a value or nil passed in, yaml and reflect both panic on the spot and the whole process dies
// inside a start hook; the caller wrote it wrong, but it deserves an error that says so. Unmarshal
// checks before looking for the configuration block: it is reported even when the block is
// unconfigured, rather than crashing the day somebody configures it
func checkTarget(target any) error {
	if v := reflect.ValueOf(target); v.Kind() != reflect.Pointer || v.IsNil() {
		return xerror.Newf("xconfig", "config", "decode target must be a non-nil pointer, got %T", target)
	}
	return nil
}

// checker walks the nodes with the target type and gathers the problems at the field and type
// level
type checker struct {
	files map[*yaml.Node]string // a node -> the file it came from
	raw   map[*yaml.Node]string // a node whose placeholder was expanded -> the text written in the configuration
	errs  []string
}

// newChecker carries the node origins the last load recorded
func newChecker() *checker {
	c := &checker{}
	if m := origins.Load(); m != nil {
		c.files = *m
	}
	if m := expanded.Load(); m != nil {
		c.raw = *m
	}
	return c
}

var (
	nodeType        = reflect.TypeOf(yaml.Node{})
	unmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()
	// The v2-style UnmarshalYAML, which yaml.v3 still recognizes
	obsoleteType = reflect.TypeOf((*interface {
		UnmarshalYAML(func(any) error) error
	})(nil)).Elem()
)

// walk goes down by t's shape, the way yaml.v3 goes while decoding: a mapping into a struct or a
// map, a sequence into a slice or an array, and a shape that does not match, or a scalar, is tried
// on the spot to see what yaml says.
//
// A type that decodes itself (one implementing yaml.Unmarshaler) is not walked into and is tried as
// well: how strict it is, is its own UnmarshalYAML's decision -- the documentation asks for
// DecodeStrict there too, and what that reports is already a position in the configuration file and
// is taken as it is. It is tried here rather than left to node.Decode so that the problems inside a
// collection's elements are reported along with the rest, instead of one being fixed and the
// process restarted before the next appears.
// An encoding.TextUnmarshaler (time.Time, netip.Addr) is configured as a scalar and lands on the
// try branch.
func (c *checker) walk(n *yaml.Node, t reflect.Type) error {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return c.walk(n.Content[0], t)
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n.ShortTag() == "!!null" || t == nodeType || t.Kind() == reflect.Interface {
		return nil // null keeps the value it had; a Node and an any take everything
	}
	switch {
	case reflect.PointerTo(t).Implements(unmarshalerType) || reflect.PointerTo(t).Implements(obsoleteType):
		return c.try(n, t)
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Struct:
		return c.mapping(n, t, fieldsOf(t))
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Map:
		return c.mapping(n, t, nil)
	case n.Kind == yaml.SequenceNode && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array):
		for _, e := range n.Content {
			if err := c.walk(e, t.Elem()); err != nil {
				return err
			}
		}
		return nil
	}
	return c.try(n, t)
}

// mapping goes down key by key. With s nil, t is a map; otherwise t is a struct and s holds its
// fields
func (c *checker) mapping(n *yaml.Node, t reflect.Type, s *structFields) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		// The merge key << was flattened into ordinary keys while parsing (see flattenMerges) and is
		// never seen here
		k, v := n.Content[i], n.Content[i+1]
		if s == nil {
			if err := c.walk(k, t.Key()); err != nil {
				return err
			}
			if err := c.walk(v, t.Elem()); err != nil {
				return err
			}
			continue
		}
		ft, ok := s.byKey[k.Value]
		if !ok {
			ft = s.inline
		}
		if ft == nil {
			c.errs = append(c.errs, c.at(k)+": "+s.notFound(k.Value, t))
			continue
		}
		if err := c.walk(v, ft); err != nil {
			return err
		}
	}
	return nil
}

// try decodes n on its own into a new value of type t, and a type error yaml reports on n's line is
// recorded against n.
// The node is this one, so the line number, the file and whether an expanded value is masked all go
// by it.
// One not on this line (reported by a nested UnmarshalYAML) is taken as it is
func (c *checker) try(n *yaml.Node, t reflect.Type) error {
	err := n.Decode(reflect.New(t).Interface())
	te, ok := err.(*yaml.TypeError)
	if !ok {
		if err != nil { // what yaml treats as fatal (a failed UnmarshalText, say) still stops here
			return fmt.Errorf("%s: %w", c.at(n), err)
		}
		return nil
	}
	for _, e := range te.Errors {
		if msg, ok := strings.CutPrefix(e, fmt.Sprintf("line %d: ", n.Line)); ok {
			e = c.at(n) + ": " + c.redact(n, msg)
		}
		c.errs = append(c.errs, e)
	}
	return nil
}

// at is the position an error opens with: "file:line" when n was parsed out of a configuration
// file (and load recorded it), and yaml's own "line <number>" otherwise
func (c *checker) at(n *yaml.Node) string {
	if file := fileOf(n, c.files); file != "" {
		return fmt.Sprintf("%s:%d", file, n.Line)
	}
	return fmt.Sprintf("line %d", n.Line)
}

// fileOf is which file n came from, empty when that is unknown.
//
// A merged map node is a fresh copy (see mergeMapping) that load never recorded; its line number is
// taken from its first key, so its file goes by that first key as well
func fileOf(n *yaml.Node, files map[*yaml.Node]string) string {
	for ; n != nil; n = first(n) {
		if f, ok := files[n]; ok {
			return f
		}
	}
	return ""
}

func first(n *yaml.Node) *yaml.Node {
	if len(n.Content) == 0 {
		return nil
	}
	return n.Content[0]
}

// redact puts the text written in the configuration back in place of the value n's type error
// carried out with it.
//
// A yaml type error carries the first few characters of the value: a password written into an int
// field is reported as cannot unmarshal !!str `hunter2...` into int -- part of a credential in the
// startup log, while ${VAR} is exactly the recommended spelling for a credential. Replaced with
// `${DB_PASSWORD}`, which field is misconfigured is just as visible.
// It goes by whether this one node was expanded, so a literal elsewhere in the configuration that
// happens to hold the same value is unaffected.
func (c *checker) redact(n *yaml.Node, msg string) string {
	raw, ok := c.raw[n]
	if !ok {
		return msg
	}
	i, j := strings.Index(msg, "`"), strings.LastIndex(msg, "`")
	if i == j {
		return msg // the error carries no value (the !!map into string kind)
	}
	return msg[:i] + "`" + raw + "` (expanded value redacted)" + msg[j+1:]
}

// structFields is which keys a struct has in yaml's eyes, with the rules copied from yaml.v3's
// getStructInfo
type structFields struct {
	byKey    map[string]reflect.Type // key -> the field type, with an ,inline struct flattened already
	inline   reflect.Type            // the element type of an ,inline map, which every unrecognized key goes into; nil when there is none
	untagged map[string]bool         // the names of the fields with no yaml tag, for the hint added to an error
}

func fieldsOf(t reflect.Type) *structFields {
	s := &structFields{byKey: map[string]reflect.Type{}, untagged: map[string]bool{}}
	s.add(t)
	return s
}

func (s *structFields) add(t reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "" && !strings.Contains(string(f.Tag), ":") {
			tag = string(f.Tag) // yaml.v3 still recognizes the old spelling where the whole tag is the name
		}
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if strings.Contains(","+opts+",", ",inline,") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch {
			case ft.Kind() == reflect.Map:
				s.inline = ft.Elem()
			case !reflect.PointerTo(ft).Implements(unmarshalerType):
				s.add(ft) // yaml does not flatten an embedded struct that decodes itself, so its fields are not recognized here either
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		if tag == "" {
			s.untagged[f.Name] = true
		}
		s.byKey[name] = f.Type
	}
}

// notFound is how a field that is not recognized is reported: yaml.v3's own text, plus a sentence
// about how to fix it.
//
// A field whose yaml tag was forgotten: yaml.v3 recognizes an untagged field only in all lowercase,
// so the field Endpoint is recognized as endpoint and not as Endpoint. Writing it in the
// configuration as the field is named reports "field Endpoint not found in type C" -- the field is
// called exactly that, and the caller is left baffled.
//
// A key written one level too high: it is a field of some block (xgin's CertFile lives under TLS:),
// and where it belongs is pointed out.
func (s *structFields) notFound(key string, t reflect.Type) string {
	msg := fmt.Sprintf("field %s not found in type %s", key, t)
	if s.untagged[key] {
		msg += fmt.Sprintf(" (field %s has no yaml tag, so only %q is accepted: add `yaml:\"%s\"`)",
			key, strings.ToLower(key), key)
	} else if block := s.blockOf(key); block != "" {
		msg += fmt.Sprintf(" (did you mean %s.%s? move it under %s:)", block, key, block)
	}
	return msg
}

// blockOf is which block a key is a field of. It looks one level down and counts only when exactly
// one block holds it, guessing at nothing when several do
func (s *structFields) blockOf(key string) string {
	found := ""
	for name, ft := range s.byKey {
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		if _, ok := fieldsOf(ft).byKey[key]; ok {
			if found != "" {
				return ""
			}
			found = name
		}
	}
	return found
}
