package main

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"go.yaml.in/yaml/v3"
)

// configDocs is which section each configuration block is documented in: a module's block lives
// under "## Configuration" in that module's README.
// A new configuration block is registered here, and checkDocs reports one that is not
var configDocs = map[string]docSection{
	"XApp":        {"xapp/README.md", "Configuration"},
	"XLog":        {"xlog/README.md", "Configuration"},
	"XTrace":      {"xtrace/README.md", "Configuration"},
	"XMetric":     {"xmetric/README.md", "Configuration"},
	"XGorm":       {"xgorm/README.md", "Configuration"},
	"XRedis":      {"xredis/README.md", "Configuration"},
	"XKafka":      {"xkafka/README.md", "Configuration"},
	"XCache":      {"xcache/README.md", "Configuration"},
	"XHttp":       {"xhttp/README.md", "Configuration"},
	"XGin":        {"xgin/README.md", "Configuration"},
	"XGinSwagger": {"xginswagger/README.md", "Configuration"},
	"XEcho":       {"xecho/README.md", "Configuration"},
	"XBeego":      {"xbeego/README.md", "Configuration"},
	"XFlow":       {"xflow/README.md", "Configuration"},
}

// exampleDocs are the sections that have no configuration block of their own yet hold YAML
// examples: only the examples are validated.
// xgorm/clickhouse's examples are of the XGorm block, and the Profiles and Import sections of
// docs/config.md are about two fields of XApp
var exampleDocs = []docSection{
	{"xgorm/clickhouse/README.md", "Configuration"},
	{"docs/config.md", "Profiles"},
	{"docs/config.md", "Import"},
}

// docSection is one second-level heading inside one document: path is relative to the repository
// root and title is the heading's first word
type docSection struct{ path, title string }

func (d docSection) String() string { return fmt.Sprintf("%s \"## %s\"", d.path, d.title) }

// checkDocs checks each configuration block's documentation (the section configDocs registers)
// against the schema and returns every problem.
// docs maps a document's path (relative to the repository root) to its whole text.
//
// Both directions are checked, and both per section:
//
//   - every field has to appear in **its own section**, not somewhere else in that README. The
//     check used to grep the whole document for the field name, and a name such as Name, Timeout or
//     Enable is always found somewhere else, so it always passed.
//   - every YAML example in a section has to be a configuration that really works: the fields
//     exist, the types are right, and the single-instance and multi-instance spellings are not
//     mixed. One key too many in the documentation means somebody configuring it that way finds it
//     has no effect -- at run time it fails startup outright.
func checkDocs(r *root, docs map[string]string) []string {
	rs, err := compile(r)
	if err != nil {
		return []string{fmt.Sprintf("the schema itself is invalid: %v", err)}
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(r.Properties)) {
		d, ok := configDocs[key]
		if !ok {
			out = append(out, fmt.Sprintf("configuration block %s is not registered in configDocs", key))
			continue
		}
		md := docs[d.path]
		text, ok := docSections(md)[d.title]
		if !ok {
			out = append(out, fmt.Sprintf("there is no %s section", d))
			continue
		}
		for _, f := range fieldNames(r.Properties[key]) {
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(f) + `\b`).MatchString(text) {
				out = append(out, fmt.Sprintf("%s does not mention %s's field %s", d, key, f))
			}
		}
		out = append(out, checkExamples(r, rs, d, text)...)
	}
	for _, d := range exampleDocs {
		text, ok := docSections(docs[d.path])[d.title]
		if !ok {
			out = append(out, fmt.Sprintf("there is no %s section", d))
			continue
		}
		out = append(out, checkExamples(r, rs, d, text)...)
	}
	return out
}

// checkExamples: every YAML example in a section has to pass the schema, and its top-level keys
// have to be configuration blocks that exist
func checkExamples(r *root, rs *jsonschema.Resolved, d docSection, text string) []string {
	var out []string
	for i, block := range yamlBlocks(text) {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
			out = append(out, fmt.Sprintf("%s YAML example %d could not be parsed: %v", d, i+1, err))
			continue
		}
		for _, top := range slices.Sorted(maps.Keys(doc)) {
			if _, ok := r.Properties[top]; !ok {
				out = append(out, fmt.Sprintf("%s YAML example %d holds the top-level key %s, which does not exist", d, i+1, top))
			}
		}
		for _, p := range problems(rs, doc) {
			out = append(out, fmt.Sprintf("%s YAML example %d: %s", d, i+1, p))
		}
	}
	return out
}

// docSections cuts a document up by its second-level headings: the heading's first word maps to
// that section's whole text (what is under its third-level headings included)
func docSections(md string) map[string]string {
	out := map[string]string{}
	var key string
	var body strings.Builder
	flush := func() {
		if key != "" {
			out[key] = body.String()
		}
		body.Reset()
	}
	for _, line := range strings.SplitAfter(md, "\n") {
		if title, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			key = strings.Fields(title + " ")[0]
			continue
		}
		body.WriteString(line)
	}
	flush()
	return out
}

// yamlBlocks takes the contents of every ```yaml block in a piece of Markdown
func yamlBlocks(text string) []string {
	var out []string
	var cur *strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case cur == nil && trimmed == "```yaml":
			cur = &strings.Builder{}
		case cur != nil && trimmed == "```":
			out = append(out, cur.String())
			cur = nil
		case cur != nil:
			cur.WriteString(line)
		}
	}
	return out
}

// fieldNames is every field name appearing in one configuration block, deduplicated and sorted.
// A map's keys are names the caller chose (an instance name, a label name) and do not count as
// fields
func fieldNames(n *node) []string {
	seen := map[string]bool{}
	var walk func(*node)
	walk = func(n *node) {
		if n == nil {
			return
		}
		for name, p := range n.Properties {
			seen[name] = true
			walk(p)
		}
		walk(n.Items)
		if ap, ok := n.AdditionalProperties.(*node); ok {
			walk(ap)
		}
		for _, b := range n.OneOf {
			walk(b)
		}
	}
	walk(n)
	return slices.Sorted(maps.Keys(seen))
}
