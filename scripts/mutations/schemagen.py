# The schemagen mutations: the promises of the schemagen module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Configuration")
mutate("the schema flags a misspelled field", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('Properties: map[string]*node{}, AdditionalProperties: false}', 'Properties: map[string]*node{}}'))
mutate("the schema tells one instance from several", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('\t\t\tRequired:             []string{"Clients"},\n', ''))
mutate("the schema accepts Import written as one string", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('stringOrList = []string{"string", "array"}', 'stringOrList = "array"'))
mutate("the schema accepts a placeholder in a numeric field", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('return &node{Type: []string{typ, "string"}, Pattern: placeholder}', 'return &node{Type: typ}'))
# The field name used to be grepped for in the whole document, so an undocumented XLog.File.Name
# passed because a Name appeared somewhere else; now only that README's "## Configuration" section
# counts, and the README's other sections do not
mutate("the documentation is checked per section", "internal/schemagen/docs.go", "./internal/schemagen", "TestCheckDocs",
       swap('\t\tif !regexp.MustCompile(`\\b` + regexp.QuoteMeta(f) + `\\b`).MatchString(text) {',
     '\t\tif !regexp.MustCompile(`\\b` + regexp.QuoteMeta(f) + `\\b`).MatchString(md) {'))
mutate("the YAML examples in the documentation have to pass the schema", "internal/schemagen/docs.go", "./internal/schemagen", "TestCheckDocs",
       swap('\t\tfor _, p := range problems(rs, doc) {', '\t\tfor _, p := range problems(rs, map[string]any{}) {'))
