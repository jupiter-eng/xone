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

// check 按整份 schema 校验一段 YAML。顶层不认识的 key 放行，与生成的 schema 一致
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
		"单实例":        "XGorm:\n  DSN: x\n  MaxOpenConns: 5\n",
		"多实例":        "XGorm:\n  Clients:\n    default: {DSN: x}\n    report: {DSN: y, MaxOpenConns: 5}\n",
		"XRedis单实例":  "XRedis:\n  Addr: a\n",
		"XCache多实例":  "XCache:\n  Clients:\n    hot: {MaxCost: 5}\n",
		"空块":         "XRedis:\nXLog:\nXGorm: {}\n",
		"Import一个":   "XApp:\n  Import: a.yml\n",
		"Import多个":   "XApp:\n  Import: [a.yml, optional:b.yml]\n",
		"Active字符串":  "XApp:\n  Profiles: dev,prod\n",
		"Active列表":   "XApp:\n  Profiles: [dev]\n",
		"整数字段写占位符":   "XGin:\n  Port: ${PORT:8080}\n",
		"布尔字段写占位符":   "XGin:\n  UseH2C: ${H2C:false}\n",
		"小数字段写占位符":   "XTrace:\n  SampleRatio: ${RATIO:1}\n",
		"map字段":      "XMetric:\n  ConstLabels: {env: dev}\n",
		"业务自己的块":     "MyApp:\n  Topic: x\n",
		"列表里的结构体":    "XTrace:\n  ForwardHeaderRules:\n    - Domains: [a.com]\n      Headers: [X-A]\n",
		"整数写进小数字段":   "XTrace:\n  SampleRatio: 1\n",
		"Postgres参数": "XGorm:\n  Postgres:\n    Params: {search_path: app}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if errs := check(t, r, body); len(errs) > 0 {
				t.Errorf("运行时接受的写法被标红：%v", errs)
			}
		})
	}
}

func TestSchema_FlagsEverythingRuntimeRejects(t *testing.T) {
	r := schemaOf(t)
	cases := map[string]string{
		"字段拼错":        "XLog:\n  Bogus: 1\n",
		"嵌套字段拼错":      "XLog:\n  File:\n    Enabel: true\n",
		"名字带数字的字段拼错":  "XGin:\n  UseH2c: true\n",
		"列表里的结构体拼错":   "XTrace:\n  ForwardHeaderRules:\n    - Domain: [a.com]\n",
		"两种写法混用":      "XGorm:\n  DSN: x\n  Clients:\n    a: {DSN: y}\n",
		"多实例里拼错":      "XGorm:\n  Clients:\n    a: {DSNN: y}\n",
		"单实例拼错":       "XRedis:\n  Adr: a\n",
		"空的Clients":   "XGorm:\n  Clients: {}\n",
		"Profiles拼错":  "XApp:\n  Profile: [a]\n",
		"整数字段写了字符串":   "XGin:\n  Port: abc\n",
		"布尔字段写了数字":    "XGin:\n  UseH2C: 1\n",
		"Import写成map": "XApp:\n  Import: {a: b}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if errs := check(t, r, body); len(errs) == 0 {
				t.Error("运行时会拒绝的写法没被标红")
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
		t.Fatal("一个示例配置都没找到，路径写错了")
	}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if errs := check(t, r, string(body)); len(errs) > 0 {
			t.Errorf("%s 过不了 schema：%v", p, errs)
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
		t.Errorf("文档与 Config 结构体对不上：\n%s", strings.Join(problems, "\n"))
	}
}

func TestCheckDocs_ChecksPerSectionNotWholeDoc(t *testing.T) {
	// 从前在整份文档里 grep 字段名：Name 在别处出现过，
	// XLog.File.Name 没写也照样通过。现在只看 xlog/README.md 的「## 配置」一节
	r := &root{Properties: map[string]*node{
		"XLog": {Type: "object", AdditionalProperties: false, Properties: map[string]*node{
			"File": {Type: "object", AdditionalProperties: false, Properties: map[string]*node{"Name": {Type: "string"}}},
		}},
	}}
	docs := map[string]string{
		"xlog/README.md": "# xlog\n\n## 配置\n\nFile 下面写文件。\n\n## 行为与实测\n\nName 是文件名。\n",
	}
	got := strings.Join(checkDocs(r, docs), "\n")
	if !strings.Contains(got, "XLog") || !strings.Contains(got, "Name") {
		t.Errorf("XLog 的「## 配置」没提到 Name，该报出来，got=%q", got)
	}
}

func TestCheckDocs_ReportsUnknownFieldsInExamples(t *testing.T) {
	r := &root{Properties: map[string]*node{
		"XGin": {Type: "object", AdditionalProperties: false, Properties: map[string]*node{"UseH2C": orPlaceholder("boolean")}},
	}}
	// 每种写坏的示例都要报出它自己的那一条，不能靠别的问题（比如缺了某份文档）凑数
	for name, c := range map[string]struct{ md, want string }{
		"不存在的字段":  {"## 配置\n\nUseH2C\n\n```yaml\nXGin:\n  UseH2C: true\n  Bogus: 1\n```\n", "YAML 示例"},
		"类型不对":    {"## 配置\n\nUseH2C\n\n```yaml\nXGin:\n  UseH2C: 1\n```\n", "YAML 示例"},
		"没有这一节":   {"## 行为与实测\n\nUseH2C\n", "没有 xgin/README.md「## 配置」"},
		"顶层key不对": {"## 配置\n\nUseH2C\n\n```yaml\nXGni:\n  UseH2C: true\n```\n", "XGni"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(checkDocs(r, map[string]string{"xgin/README.md": c.md}), "\n"); !strings.Contains(got, c.want) {
				t.Errorf("该报出含 %q 的问题，got=%q", c.want, got)
			}
		})
	}
}

func TestCheckDocs_ReportsUndocumentedConfigBlocks(t *testing.T) {
	r := &root{Properties: map[string]*node{"XNew": {Type: "object"}}}
	if got := strings.Join(checkDocs(r, nil), "\n"); !strings.Contains(got, "XNew") {
		t.Errorf("XNew 没登记在 configDocs 里，该报出来，got=%q", got)
	}
}
