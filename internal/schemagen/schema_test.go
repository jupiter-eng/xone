package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const repo = "../.."

func schemaOf(t *testing.T) *root {
	t.Helper()
	r, err := build(repo)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// check validates a piece of YAML against the whole schema. An unknown top-level key is let
// through, as the generated schema does
func check(t *testing.T, r *root, body string) []string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	rs, err := compile(r)
	if err != nil {
		t.Fatal(err)
	}
	return problems(rs, doc)
}

func TestSchema_AcceptsEverythingRuntimeAccepts(t *testing.T) {
	r := schemaOf(t)
	cases := map[string]string{
		"one instance":                    "XGorm:\n  DSN: x\n  MaxOpenConns: 5\n",
		"several instances":               "XGorm:\n  Clients:\n    default: {DSN: x}\n    report: {DSN: y, MaxOpenConns: 5}\n",
		"one XRedis instance":             "XRedis:\n  Addr: a\n",
		"several XCache instances":        "XCache:\n  Clients:\n    hot: {MaxCost: 5}\n",
		"an empty block":                  "XRedis:\nXLog:\nXGorm: {}\n",
		"one Import":                      "XApp:\n  Import: a.yml\n",
		"several Imports":                 "XApp:\n  Import: [a.yml, optional:b.yml]\n",
		"Profiles as a string":            "XApp:\n  Profiles: dev,prod\n",
		"Profiles as a list":              "XApp:\n  Profiles: [dev]\n",
		"a placeholder in an int field":   "XGin:\n  Port: ${PORT:8080}\n",
		"a placeholder in a bool field":   "XGin:\n  UseH2C: ${H2C:false}\n",
		"a placeholder in a float field":  "XTrace:\n  SampleRatio: ${RATIO:1}\n",
		"a map field":                     "XMetric:\n  ConstLabels: {env: dev}\n",
		"the business's own block":        "MyApp:\n  Topic: x\n",
		"a struct inside a list":          "XTrace:\n  ForwardHeaderRules:\n    - Domains: [a.com]\n      Headers: [X-A]\n",
		"an int written in a float field": "XTrace:\n  SampleRatio: 1\n",
		"Postgres parameters":             "XGorm:\n  Postgres:\n    Params: {search_path: app}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if errs := check(t, r, body); len(errs) > 0 {
				t.Errorf("a spelling the runtime accepts was flagged: %v", errs)
			}
		})
	}
}

func TestSchema_FlagsEverythingRuntimeRejects(t *testing.T) {
	r := schemaOf(t)
	cases := map[string]string{
		"a misspelled field":                        "XLog:\n  Bogus: 1\n",
		"a misspelled nested field":                 "XLog:\n  File:\n    Enabel: true\n",
		"a misspelled field whose name has a digit": "XGin:\n  UseH2c: true\n",
		"a misspelled struct inside a list":         "XTrace:\n  ForwardHeaderRules:\n    - Domain: [a.com]\n",
		"the two spellings mixed":                   "XGorm:\n  DSN: x\n  Clients:\n    a: {DSN: y}\n",
		"a misspelling among several instances":     "XGorm:\n  Clients:\n    a: {DSNN: y}\n",
		"a misspelling in one instance":             "XRedis:\n  Adr: a\n",
		"empty Clients":                             "XGorm:\n  Clients: {}\n",
		"a misspelled Profiles":                     "XApp:\n  Profile: [a]\n",
		"a string written in an int field":          "XGin:\n  Port: abc\n",
		"a number written in a bool field":          "XGin:\n  UseH2C: 1\n",
		"Import written as a map":                   "XApp:\n  Import: {a: b}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if errs := check(t, r, body); len(errs) == 0 {
				t.Error("a spelling the runtime refuses was not flagged")
			}
		})
	}
}

func TestSchema_ExampleConfigsPass(t *testing.T) {
	r := schemaOf(t)
	top, _ := filepath.Glob(filepath.Join(repo, "example", "*.yml"))
	nested, _ := filepath.Glob(filepath.Join(repo, "example", "*", "*.yml"))
	paths := append(top, nested...)
	if len(paths) == 0 {
		t.Fatal("not one example configuration was found; the path must be wrong")
	}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if errs := check(t, r, string(body)); len(errs) > 0 {
			t.Errorf("%s does not pass the schema: %v", p, errs)
		}
	}
}

func TestCheckDocs_ConfigDocsMatchStructs(t *testing.T) {
	docs := map[string]string{}
	for _, d := range append(slices.Collect(maps.Values(configDocs)), exampleDocs...) {
		md, err := os.ReadFile(filepath.Join(repo, d.path))
		if err != nil {
			t.Fatal(err)
		}
		docs[d.path] = string(md)
	}
	if problems := checkDocs(schemaOf(t), docs); len(problems) > 0 {
		t.Errorf("the documentation does not agree with the Config structs:\n%s", strings.Join(problems, "\n"))
	}
}

func TestCheckDocs_ChecksPerSectionNotWholeDoc(t *testing.T) {
	// It used to grep the field names across the whole document: Name appeared elsewhere,
	// so an unwritten XLog.File.Name passed anyway. Now only xlog/README.md's "## Configuration" section is looked at.
	r := &root{Properties: map[string]*node{
		"XLog": {Type: "object", AdditionalProperties: false, Properties: map[string]*node{
			"File": {Type: "object", AdditionalProperties: false, Properties: map[string]*node{"Name": {Type: "string"}}},
		}},
	}}
	docs := map[string]string{
		"xlog/README.md": "# xlog\n\n## Configuration\n\nFile is where the file settings go.\n\n## Behavior (measured)\n\nName is the file name.\n",
	}
	got := strings.Join(checkDocs(r, docs), "\n")
	if !strings.Contains(got, "XLog") || !strings.Contains(got, "Name") {
		t.Errorf("XLog's \"## Configuration\" does not mention Name and that must be reported, got=%q", got)
	}
}

func TestCheckDocs_ReportsUnknownFieldsInExamples(t *testing.T) {
	r := &root{Properties: map[string]*node{
		"XGin": {Type: "object", AdditionalProperties: false, Properties: map[string]*node{"UseH2C": orPlaceholder("boolean")}},
	}}
	// Each broken example has to be reported on its own merits, not padded out by some other
	// problem (a missing document, say).
	//
	for name, c := range map[string]struct{ md, want string }{
		"unknown field":   {"## Configuration\n\nUseH2C\n\n```yaml\nXGin:\n  UseH2C: true\n  Bogus: 1\n```\n", "YAML example"},
		"wrong type":      {"## Configuration\n\nUseH2C\n\n```yaml\nXGin:\n  UseH2C: 1\n```\n", "YAML example"},
		"section missing": {"## Behavior (measured)\n\nUseH2C\n", `there is no xgin/README.md "## Configuration" section`},
		"wrong top key":   {"## Configuration\n\nUseH2C\n\n```yaml\nXGni:\n  UseH2C: true\n```\n", "XGni"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(checkDocs(r, map[string]string{"xgin/README.md": c.md}), "\n"); !strings.Contains(got, c.want) {
				t.Errorf("a problem containing %q is expected, got=%q", c.want, got)
			}
		})
	}
}

func TestCheckDocs_ReportsUndocumentedConfigBlocks(t *testing.T) {
	r := &root{Properties: map[string]*node{"XNew": {Type: "object"}}}
	if got := strings.Join(checkDocs(r, nil), "\n"); !strings.Contains(got, "XNew") {
		t.Errorf("XNew is not registered in configDocs and that must be reported, got=%q", got)
	}
}
