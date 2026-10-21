package main

import (
	"go/ast"
	"strings"
)

// fieldDoc turns the comment on a field into a one-line description.
//
// The comments are written in a fixed shape:
//
//	// Timeout is one attempt's timeout. It defaults to 60s.
//	//
//	// A longer explanation...
//	Timeout time.Duration `yaml:"Timeout"`
//
// The whole group is taken, the field name repeated at the start is removed, and the rest is joined
// into one line. Not only the first sentence, because the comments in this repository usually put
// the "why" in the lines after it, and those are exactly what belongs in an editor's hover
func fieldDoc(f *ast.Field) string {
	if f.Doc == nil {
		return ""
	}
	name := ""
	if len(f.Names) > 0 {
		name = f.Names[0].Name
	}
	return flatten(f.Doc, name)
}

// docOf takes the comment on a type
func docOf(p *pkg, typeName string) string {
	if d, ok := p.docs[typeName]; ok {
		return flatten(d, typeName)
	}
	return ""
}

// flatten squeezes a comment group into one line and removes the leading "<field name> "
func flatten(g *ast.CommentGroup, name string) string {
	lines := make([]string, 0, len(g.List))
	for _, c := range g.List {
		raw := c.Text // *ast.Comment.Text is a field and carries the "//" prefix
		// The lines of a code example (the indented ones) do not read in one line and are dropped
		if strings.HasPrefix(raw, "//\t") {
			continue
		}
		if t := strings.TrimSpace(strings.TrimPrefix(raw, "//")); t != "" {
			lines = append(lines, t)
		}
	}
	out := strings.Join(lines, " ")
	if name != "" {
		out = strings.TrimSpace(strings.TrimPrefix(out, name))
	}
	// Dropping a code example usually leaves a stub such as "Two ways to write it:" behind, whose
	// trailing colon now points at nothing
	return strings.TrimRight(strings.TrimSpace(out), ":")
}
