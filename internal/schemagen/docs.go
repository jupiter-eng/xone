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

// configDocs 每个配置块写在哪一节：模块的配置块在那个模块 README 的「## 配置」。
// 加了一个配置块就在这里登记，没登记的 checkDocs 会报出来
var configDocs = map[string]docSection{
	"XApp":        {"xapp/README.md", "配置"},
	"XLog":        {"xlog/README.md", "配置"},
	"XTrace":      {"xtrace/README.md", "配置"},
	"XMetric":     {"xmetric/README.md", "配置"},
	"XGorm":       {"xgorm/README.md", "配置"},
	"XRedis":      {"xredis/README.md", "配置"},
	"XKafka":      {"xkafka/README.md", "配置"},
	"XCache":      {"xcache/README.md", "配置"},
	"XHttp":       {"xhttp/README.md", "配置"},
	"XGin":        {"xgin/README.md", "配置"},
	"XGinSwagger": {"xginswagger/README.md", "配置"},
	"XEcho":       {"xecho/README.md", "配置"},
	"XFlow":       {"xflow/README.md", "配置"},
}

// exampleDocs 自己没有配置块、却写了 YAML 示例的节：只校验示例。
// xgorm/clickhouse 的示例写的是 XGorm 块；docs/config.md 的 Profiles、Import 两节讲的是 XApp 的两项
var exampleDocs = []docSection{
	{"xgorm/clickhouse/README.md", "配置"},
	{"docs/config.md", "Profiles"},
	{"docs/config.md", "Import"},
}

// docSection 一份文档里的一个二级标题：path 相对仓库根，title 是标题的第一个词
type docSection struct{ path, title string }

func (d docSection) String() string { return fmt.Sprintf("%s「## %s」", d.path, d.title) }

// checkDocs 对照 schema 检查每个配置块的文档（configDocs 登记的那一节），返回全部问题。
// docs 是文档的路径（相对仓库根）→ 全文。
//
// 两个方向都查，而且都按节查：
//
//   - 每个字段都要在**它自己那一节**里出现，不是那份 README 的别处。从前的检查是在整份
//     文档里 grep 字段名，Name、Timeout、Enable 这种名字总能在别处找到，永远通过。
//   - 每一节里的 YAML 示例都得是真的能用的配置：字段存在、类型对、
//     单实例和多实例的写法没混。文档里多写一个不存在的 key，
//     照着配的人会发现它不生效——运行时直接启动失败。
func checkDocs(r *root, docs map[string]string) []string {
	rs, err := compile(r)
	if err != nil {
		return []string{fmt.Sprintf("schema 本身不合法：%v", err)}
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(r.Properties)) {
		d, ok := configDocs[key]
		if !ok {
			out = append(out, fmt.Sprintf("配置块 %s 没有登记在 configDocs 里", key))
			continue
		}
		md := docs[d.path]
		text, ok := docSections(md)[d.title]
		if !ok {
			out = append(out, fmt.Sprintf("没有 %s 这一节", d))
			continue
		}
		for _, f := range fieldNames(r.Properties[key]) {
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(f) + `\b`).MatchString(text) {
				out = append(out, fmt.Sprintf("%s 里没有提到 %s 的字段 %s", d, key, f))
			}
		}
		out = append(out, checkExamples(r, rs, d, text)...)
	}
	for _, d := range exampleDocs {
		text, ok := docSections(docs[d.path])[d.title]
		if !ok {
			out = append(out, fmt.Sprintf("没有 %s 这一节", d))
			continue
		}
		out = append(out, checkExamples(r, rs, d, text)...)
	}
	return out
}

// checkExamples 一节里的每个 YAML 示例都要过得了 schema，顶层 key 也得是认识的配置块
func checkExamples(r *root, rs *jsonschema.Resolved, d docSection, text string) []string {
	var out []string
	for i, block := range yamlBlocks(text) {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
			out = append(out, fmt.Sprintf("%s 第 %d 个 YAML 示例解析失败：%v", d, i+1, err))
			continue
		}
		for _, top := range slices.Sorted(maps.Keys(doc)) {
			if _, ok := r.Properties[top]; !ok {
				out = append(out, fmt.Sprintf("%s 第 %d 个 YAML 示例里有不存在的顶层 key %s", d, i+1, top))
			}
		}
		for _, p := range problems(rs, doc) {
			out = append(out, fmt.Sprintf("%s 第 %d 个 YAML 示例：%s", d, i+1, p))
		}
	}
	return out
}

// docSections 按二级标题切开文档：标题的第一个词 → 这一节的全文（含三级标题下的内容）
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

// yamlBlocks 取出一段 Markdown 里所有 ```yaml 代码块的内容
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

// fieldNames 一个配置块里出现的全部字段名，去重排序。
// map 的 key 是使用者自己起的名字（实例名、标签名），不算字段
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
