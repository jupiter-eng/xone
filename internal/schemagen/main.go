// schemagen generates config_schema.json from the modules' Config structs.
//
//	go run ./internal/schemagen            # writes config_schema.json
//	go run ./internal/schemagen -check     # compares only, exiting non-zero on a difference
//
// Both run in the repository root (through go.work). Checking the configuration documentation
// against the schema is TestCheckDocs, see checkDocs.
//
// Why it is generated rather than written by hand: the schema holds hundreds of fields, a
// hand-written one sooner or later disagrees with the structs, and a disagreement shows up as an
// editor completing a field that does not exist at all -- worse than having no schema.
// The field descriptions are taken straight from the comments on the structs, so the comments, the
// documentation and the schema all come from one source.
//
// It parses the source with go/ast alone and imports no module: they are Go modules of their own
// and cannot be imported here, which parsing the source never needed anyway.
//
// It is a module of its own as well (for use inside the repository alone; release.sh does not
// release it): jsonschema-go, which validates the YAML examples, must not count towards the core's
// dependency footprint (check.sh, step 1).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const outFile = "config_schema.json"

func main() {
	check := flag.Bool("check", false, "compare against the checked-in file instead of writing it")
	flag.Parse()

	schema, err := build(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "generating failed:", err)
		os.Exit(1)
	}

	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "serializing failed:", err)
		os.Exit(1)
	}
	out = append(out, '\n')

	if *check {
		have, err := os.ReadFile(outFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %s failed: %v\n", outFile, err)
			os.Exit(1)
		}
		if !bytes.Equal(have, out) {
			fmt.Fprintf(os.Stderr, "%s no longer agrees with the structs; run go run ./internal/schemagen\n", outFile)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(outFile, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "writing the file failed:", err)
		os.Exit(1)
	}
	fmt.Printf("%s updated (%d top-level keys)\n", outFile, len(schema.Properties))
}

// ---- The schema's structure ----

type node struct {
	// Type is one type name, or a list of them (["integer", "string"], say)
	Type        any              `json:"type,omitempty"`
	Description string           `json:"description,omitempty"`
	Properties  map[string]*node `json:"properties,omitempty"`
	Required    []string         `json:"required,omitempty"`
	// AdditionalProperties is the schema of a map's values, or false (a struct: an unknown field is
	// an error)
	AdditionalProperties any     `json:"additionalProperties,omitempty"`
	MinProperties        int     `json:"minProperties,omitempty"`
	Items                *node   `json:"items,omitempty"`
	Pattern              string  `json:"pattern,omitempty"`
	OneOf                []*node `json:"oneOf,omitempty"`

	// duration marks this field as a duration. Durations are the most common kind of field here
	// (over thirty of them) and are written in YAML as strings such as 30s / 1500ms, so without a
	// hint, completing a "string" says nothing at all
	duration bool
}

type root struct {
	Schema     string           `json:"$schema"`
	ID         string           `json:"$id"`
	Title      string           `json:"title"`
	Type       string           `json:"type"`
	Properties map[string]*node `json:"properties"`
}

// ---- Parsing ----

// pkg is what is known about one module's package: the struct definitions and the ConfigKey
type pkg struct {
	structs   map[string]*ast.StructType
	aliases   map[string]ast.Expr          // named scalars, type Driver string for one
	docs      map[string]*ast.CommentGroup // the comments on the types
	configKey string

	// shared holds the repository's other packages by package name. A field whose type crosses
	// packages, such as xtls.Config, is looked up here
	shared map[string]*pkg
}

func build(repo string) (*root, error) {
	dirs, err := configDirs(repo)
	if err != nil {
		return nil, err
	}

	props := map[string]*node{}

	// Everything is parsed first: a configuration block's field may refer to a struct in another
	// package (xtls.Config, which the modules share)
	pkgs := make([]*pkg, 0, len(dirs))
	shared := map[string]*pkg{}
	for _, dir := range dirs {
		p, err := parsePkg(dir)
		if err != nil {
			return nil, err
		}
		p.shared = shared
		pkgs = append(pkgs, p)
		shared[filepath.Base(dir)] = p
	}

	for i, dir := range dirs {
		p := pkgs[i]
		if p.configKey == "" {
			continue // a package with no ConfigKey is not a configuration block
		}
		n, err := configNode(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		props[p.configKey] = n
	}

	// XApp.Profiles / XApp.Import are consumed by the loader itself and are not in xapp.Config, so
	// they are added here
	app, ok := props["XApp"]
	if !ok {
		return nil, fmt.Errorf("no XApp block: xapp.ConfigKey changed? Profiles and Import live under it")
	}
	app.Properties["Profiles"] = &node{Type: stringOrList, Items: &node{Type: "string"},
		Description: "Which profiles are active, as Spring's spring.profiles.active is: one name, a " +
			"comma-separated string, or a list, where a later one overrides an earlier one. It ranks " +
			"below --profile and XONE_PROFILE, and may be written in the main configuration file only"}
	app.Properties["Import"] = &node{Type: stringOrList, Items: &node{Type: "string"},
		Description: "Imports other configuration files, as Spring's spring.config.import does. An " +
			"imported file overrides the one importing it; a relative path resolves against the " +
			"directory of the file importing it; an optional: prefix skips a file that does not exist"}

	// No additionalProperties: false at the top level: the business's own configuration blocks (see
	// docs/guide.md, "Reading a configuration of your own") are written at the top level too, and
	// the schema cannot possibly know them.
	// A misspelling at the top level is caught at run time by "a block nobody claims fails
	// outright".
	//
	// $schema has to carry the trailing #: that is how the standard URI of the draft-07 meta schema
	// is written, and jsonschema-go v0.4.3 was measured accepting only the one with the #,
	// reporting cannot validate version without it
	return &root{
		Schema:     "http://json-schema.org/draft-07/schema#",
		ID:         "https://github.com/jupiter-eng/xone",
		Title:      "XOne configuration",
		Type:       "object",
		Properties: props,
	}, nil
}

// configDirs finds every directory that may declare a configuration block
func configDirs(repo string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(repo, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		// The directories that hold no configuration block are skipped. example holds the examples,
		// internal is the framework's inside, and the hidden directories and testdata have nothing to
		// do with the configuration
		if path != repo && (strings.HasPrefix(name, ".") || name == "example" ||
			name == "internal" || name == "testdata") {
			return filepath.SkipDir
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	return out, err
}

func parsePkg(dir string) (*pkg, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	p := &pkg{
		structs: map[string]*ast.StructType{},
		aliases: map[string]ast.Expr{},
		docs:    map[string]*ast.CommentGroup{},
	}
	for _, ap := range pkgs {
		for _, f := range ap.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						// A type's comment may hang off the TypeSpec, or off the GenDecl around it (the
						// latter when `type X struct` is declared on its own)
						if d := s.Doc; d != nil {
							p.docs[s.Name.Name] = d
						} else if gd.Doc != nil && len(gd.Specs) == 1 {
							p.docs[s.Name.Name] = gd.Doc
						}
						if st, ok := s.Type.(*ast.StructType); ok {
							p.structs[s.Name.Name] = st
						} else {
							p.aliases[s.Name.Name] = s.Type
						}
					case *ast.ValueSpec:
						if p.configKey == "" {
							p.configKey = configKeyOf(s)
						}
					}
				}
			}
		}
	}
	return p, nil
}

// configKeyOf takes the string out of `const ConfigKey = "XGorm"`
func configKeyOf(s *ast.ValueSpec) string {
	for i, name := range s.Names {
		if name.Name != "ConfigKey" || i >= len(s.Values) {
			continue
		}
		lit, ok := s.Values[i].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			continue
		}
		return v
	}
	return ""
}
