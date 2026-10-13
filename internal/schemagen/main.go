// schemagen 从各模块的 Config 结构体生成 config_schema.json。
//
//	go run ./internal/schemagen            # 写到 config_schema.json
//	go run ./internal/schemagen -check     # 只比对，不一致就非零退出
//
// 两条命令都在仓库根目录跑（走 go.work）。配置文档对照 schema 的检查是
// TestCheckDocs，见 checkDocs。
//
// 为什么生成而不是手写：schema 有上百个字段，手写的那份迟早和结构体对不上，
// 而对不上的表现是 IDE 里补全出一个根本不存在的字段——比没有 schema 更糟。
// 字段说明直接取结构体上的注释，于是注释、文档、schema 是同一个来源。
//
// 用 go/ast 纯解析源码，不 import 各模块：它们是独立的 Go module，
// 这里 import 不到，而解析源码本来就不需要。
//
// 它自己也是一个独立的 module（只给仓库内部用，release.sh 不发布它）：
// 校验 YAML 示例用的 jsonschema-go 不该算进核心的依赖足迹（check.sh 第 1 步）。
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
	check := flag.Bool("check", false, "只比对已签入的文件，不写")
	flag.Parse()

	schema, err := build(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成失败:", err)
		os.Exit(1)
	}

	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "序列化失败:", err)
		os.Exit(1)
	}
	out = append(out, '\n')

	if *check {
		have, err := os.ReadFile(outFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读 %s 失败: %v\n", outFile, err)
			os.Exit(1)
		}
		if !bytes.Equal(have, out) {
			fmt.Fprintf(os.Stderr, "%s 与结构体对不上了，跑一次 go run ./internal/schemagen\n", outFile)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(outFile, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写文件失败:", err)
		os.Exit(1)
	}
	fmt.Printf("%s 已更新（%d 个顶层 key）\n", outFile, len(schema.Properties))
}

// ---- schema 结构 ----

type node struct {
	// Type 一个类型名，或者几个类型名的列表（如 ["integer", "string"]）
	Type        any              `json:"type,omitempty"`
	Description string           `json:"description,omitempty"`
	Properties  map[string]*node `json:"properties,omitempty"`
	Required    []string         `json:"required,omitempty"`
	// AdditionalProperties map 的 value 的 schema，或者 false（结构体：不认识的字段是错误）
	AdditionalProperties any     `json:"additionalProperties,omitempty"`
	MinProperties        int     `json:"minProperties,omitempty"`
	Items                *node   `json:"items,omitempty"`
	Pattern              string  `json:"pattern,omitempty"`
	OneOf                []*node `json:"oneOf,omitempty"`

	// duration 标记这个字段是时长。时长是这里最多的一种字段（三十多个），
	// 而 YAML 里写的是 30s / 1500ms 这种字符串，不提示的话
	// 补全出一个 "string" 等于没说
	duration bool
}

type root struct {
	Schema     string           `json:"$schema"`
	ID         string           `json:"$id"`
	Title      string           `json:"title"`
	Type       string           `json:"type"`
	Properties map[string]*node `json:"properties"`
}

// ---- 解析 ----

// pkg 一个模块的包信息：结构体定义和 ConfigKey
type pkg struct {
	structs   map[string]*ast.StructType
	aliases   map[string]ast.Expr          // 具名标量，如 type Driver string
	docs      map[string]*ast.CommentGroup // 类型上的注释
	configKey string

	// shared 仓库里别的包，按包名。字段的类型写成 xtls.Config 这样跨包的，去这里找
	shared map[string]*pkg
}

func build(repo string) (*root, error) {
	dirs, err := configDirs(repo)
	if err != nil {
		return nil, err
	}

	props := map[string]*node{}

	// 先全部解一遍：配置块的字段可以引用别的包里的结构体（各模块共用的 xtls.Config）
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
			continue // 没有 ConfigKey 的包不是配置块
		}
		n, err := configNode(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		props[p.configKey] = n
	}

	// XApp.Profiles / XApp.Import 由加载器自己消费，不在 xapp.Config 里，所以在这里补上
	app, ok := props["XApp"]
	if !ok {
		return nil, fmt.Errorf("no XApp block: xapp.ConfigKey changed? Profiles and Import live under it")
	}
	app.Properties["Profiles"] = &node{Type: stringOrList, Items: &node{Type: "string"},
		Description: "激活哪些 profile，对应 Spring 的 spring.profiles.active：一个名字、逗号分隔的字符串或列表，靠后的压过靠前的。" +
			"优先级低于 --profile 和 XONE_PROFILE；只能写在主配置文件里"}
	app.Properties["Import"] = &node{Type: stringOrList, Items: &node{Type: "string"},
		Description: "引入别的配置文件，对应 Spring 的 spring.config.import。引进来的压过引它的那个文件；" +
			"相对路径按引它的文件所在目录解析；optional: 前缀表示文件不存在就跳过"}

	// 顶层不设 additionalProperties: false：业务自己的配置块（见 docs/guide.md
	// 「读自己的配置」）也写在顶层，schema 不可能认识它们。
	// 顶层拼错由运行时的「没人认领的块直接失败」拦住
	//
	// $schema 必须带结尾的 #：draft-07 元 schema 的标准 URI 就是这么写的，
	// jsonschema-go v0.4.3 实测只认带 # 的那一个，不带就报 cannot validate version
	return &root{
		Schema:     "http://json-schema.org/draft-07/schema#",
		ID:         "https://github.com/jupiter-eng/xone",
		Title:      "XOne 配置",
		Type:       "object",
		Properties: props,
	}, nil
}

// configDirs 找出所有可能声明了配置块的目录
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
		// 跳过不放配置块的目录。example 是示例、internal 是框架内部、
		// 隐藏目录和 testdata 与配置无关
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
						// 类型注释可能挂在 TypeSpec 上，也可能挂在外层的
						// GenDecl 上（`type X struct` 单独声明时是后者）
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

// configKeyOf 从 `const ConfigKey = "XGorm"` 取出那个字符串
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
