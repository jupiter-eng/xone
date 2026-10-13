# schemagen 的变异：module schemagen 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("配置")
mutate("schema 里拼错的字段标红", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('Properties: map[string]*node{}, AdditionalProperties: false}', 'Properties: map[string]*node{}}'))
mutate("schema 分得清单实例和多实例", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('\t\t\tRequired:             []string{"Clients"},\n', ''))
mutate("schema 里 Import 写一个字符串也行", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('stringOrList = []string{"string", "array"}', 'stringOrList = "array"'))
mutate("schema 里数字字段收占位符", "internal/schemagen/schema.go", "./internal/schemagen", "TestSchema|TestCheckDocs",
       swap('return &node{Type: []string{typ, "string"}, Pattern: placeholder}', 'return &node{Type: typ}'))
# 从前在整份文档里 grep 字段名，XLog.File.Name 没写也会因为别处写了 Name 而通过；
# 现在只认它 README 的「## 配置」一节，README 的其余几节不算
mutate("文档按节检查字段", "internal/schemagen/docs.go", "./internal/schemagen", "TestCheckDocs",
       swap('\t\tif !regexp.MustCompile(`\\b` + regexp.QuoteMeta(f) + `\\b`).MatchString(text) {',
     '\t\tif !regexp.MustCompile(`\\b` + regexp.QuoteMeta(f) + `\\b`).MatchString(md) {'))
mutate("文档里的 YAML 示例要过得了 schema", "internal/schemagen/docs.go", "./internal/schemagen", "TestCheckDocs",
       swap('\t\tfor _, p := range problems(rs, doc) {', '\t\tfor _, p := range problems(rs, map[string]any{}) {'))
