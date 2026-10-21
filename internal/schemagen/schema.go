package main

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"
)

// A few type spellings that keep coming up
var (
	// stringOrList is one string or a list of strings, which XApp.Import and XApp.Profiles both
	// accept
	stringOrList = []string{"string", "array"}
	// objectOrNull is a top-level block: `XRedis:` with nothing under it is valid and means the
	// block is unconfigured
	objectOrNull = []string{"object", "null"}
)

// placeholder is a placeholder's shape. ${VAR} may be written in any scalar field and its type is
// decided from the content only after it is expanded, so a numeric or boolean field has to accept a
// string holding ${ as well, or `Port: ${PORT:8080}` would be flagged -- while the documentation
// recommends exactly that
const placeholder = `\$\{`

// configNode generates this block's schema from the package's Config type
func configNode(p *pkg) (*node, error) {
	st, ok := p.structs["Config"]
	if !ok {
		return nil, fmt.Errorf("no Config type found")
	}

	// xgorm / xredis / xcache all have the same shape: Config holds nothing but a
	// Clients map[string]XxxConfig, while the configuration file may spell out a single instance
	// directly or write Clients. The schema has to accept both, or the single-instance spelling
	// would be flagged.
	//
	// The two branches have to be mutually exclusive for oneOf to mean anything -- were both
	// spellings to satisfy both branches, every configuration would fail "satisfies exactly one"
	// and be flagged. They are told apart the way the runtime does it
	// (xconfig.UnmarshalClients): Clients means several instances, and no other key is allowed
	// alongside it. The single-instance branch's additionalProperties: false already refuses
	// Clients
	if elem := onlyClients(st); elem != "" {
		single, err := structNode(p, p.structs[elem], "")
		if err != nil {
			return nil, err
		}
		multi := &node{
			Type:                 "object",
			Required:             []string{"Clients"},
			AdditionalProperties: false,
			Properties: map[string]*node{
				"Clients": {
					Type:                 "object",
					Description:          "Several instances organized by name, where the instance name is the key",
					AdditionalProperties: single,
					MinProperties:        1, // empty Clients is an error at run time
				},
			},
		}
		return &node{
			Description: docOf(p, "Config"),
			OneOf:       []*node{single, multi, {Type: "null"}},
		}, nil
	}

	n, err := structNode(p, st, docOf(p, "Config"))
	if err != nil {
		return nil, err
	}
	n.Type = objectOrNull
	return n, nil
}

// onlyClients reports whether this Config has the "nothing but a Clients map" shape and, when it
// does, returns the element type's name
func onlyClients(st *ast.StructType) string {
	if st.Fields == nil || len(st.Fields.List) != 1 {
		return ""
	}
	f := st.Fields.List[0]
	if len(f.Names) != 1 || f.Names[0].Name != "Clients" {
		return ""
	}
	m, ok := f.Type.(*ast.MapType)
	if !ok {
		return ""
	}
	id, ok := m.Value.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

// structNode turns a struct into a schema object.
//
// An unknown field is an error (additionalProperties: false), as it is under the runtime's strict
// decoding: without it, a misspelled field stays green in the editor and is only found at
// startup
func structNode(p *pkg, st *ast.StructType, desc string) (*node, error) {
	out := &node{Type: "object", Description: desc, Properties: map[string]*node{}, AdditionalProperties: false}
	if st == nil || st.Fields == nil {
		return out, nil
	}
	for _, f := range st.Fields.List {
		name := yamlName(f)
		if name == "" {
			continue // a field without a yaml tag is not part of the configuration
		}
		n, err := typeNode(p, f.Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", name, err)
		}
		n.Description = withFormat(n, fieldDoc(f))
		out.Properties[name] = n
	}
	return out, nil
}

// yamlName takes the name out of a yaml tag
func yamlName(f *ast.Field) string {
	if f.Tag == nil || len(f.Names) == 0 {
		return ""
	}
	tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
	v := tag.Get("yaml")
	if v == "" || v == "-" {
		return ""
	}
	return strings.Split(v, ",")[0]
}

// typeNode maps a Go type to a schema node
func typeNode(p *pkg, expr ast.Expr) (*node, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		return identNode(p, t.Name)

	case *ast.SelectorExpr:
		// A type from another package: of the standard library only time.Duration is recognized, and
		// the rest are structs in this repository's other packages (xtls.Config, say).
		// They are looked up by package name, so a package name has to match its directory name
		x, ok := t.X.(*ast.Ident)
		if ok && x.Name == "time" && t.Sel.Name == "Duration" {
			return &node{Type: "string", duration: true}, nil
		}
		if ok {
			if sp, found := p.shared[x.Name]; found {
				if st, found := sp.structs[t.Sel.Name]; found {
					return structNode(sp, st, docOf(sp, t.Sel.Name))
				}
			}
		}
		return nil, fmt.Errorf("unknown type %s.%s", exprName(t.X), t.Sel.Name)

	case *ast.ArrayType:
		item, err := typeNode(p, t.Elt)
		if err != nil {
			return nil, err
		}
		return &node{Type: "array", Items: item}, nil

	case *ast.MapType:
		val, err := typeNode(p, t.Value)
		if err != nil {
			return nil, err
		}
		return &node{Type: "object", AdditionalProperties: val}, nil

	case *ast.StarExpr:
		return typeNode(p, t.X)

	default:
		return nil, fmt.Errorf("unknown type %T", expr)
	}
}

// identNode handles the built-in types, the named scalars and the structs of the same package
func identNode(p *pkg, name string) (*node, error) {
	switch name {
	case "string":
		return &node{Type: "string"}, nil
	case "bool":
		return orPlaceholder("boolean"), nil
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		return orPlaceholder("integer"), nil
	case "float32", "float64":
		return orPlaceholder("number"), nil
	}
	if st, ok := p.structs[name]; ok {
		return structNode(p, st, docOf(p, name))
	}
	if alias, ok := p.aliases[name]; ok {
		return typeNode(p, alias) // type Driver string, for one
	}
	return nil, fmt.Errorf("unknown type %s", name)
}

// orPlaceholder is a non-string scalar type plus the one string spelling "written as a
// placeholder".
// A pattern constrains strings alone, so a real number or boolean is unaffected by it
func orPlaceholder(typ string) *node {
	return &node{Type: []string{typ, "string"}, Pattern: placeholder}
}

// withFormat adds a sentence about the format to a duration field
func withFormat(n *node, desc string) string {
	if !n.duration {
		return desc
	}
	const hint = "a duration, written as 30s / 1500ms / 1h30m; a bare number fails startup"
	if desc == "" {
		return hint
	}
	return desc + " (" + hint + ")"
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%T", e)
}
