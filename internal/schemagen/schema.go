package main

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"
)

// 几种反复出现的类型写法
var (
	// stringOrList 一个字符串或一个字符串列表，XApp.Import 和 XApp.Profiles 都收这两种
	stringOrList = []string{"string", "array"}
	// objectOrNull 顶层块：`XRedis:` 什么都不写是合法的，等于没配这一块
	objectOrNull = []string{"object", "null"}
)

// placeholder 占位符的形状。${VAR} 能写在任何标量字段上，展开之后才按内容判定
// 类型，所以数字和布尔字段也得收一个带 ${ 的字符串，否则 `Port: ${PORT:8080}`
// 会被标红——而文档里它是推荐写法
const placeholder = `\$\{`

// configNode 由包里的 Config 类型生成这一块的 schema
func configNode(p *pkg) (*node, error) {
	st, ok := p.structs["Config"]
	if !ok {
		return nil, fmt.Errorf("没有找到 Config 类型")
	}

	// xgorm / xredis / xcache 是同一个形状：Config 里只有一个
	// Clients map[string]XxxConfig，而配置文件既可以直接写单实例、
	// 也可以写 Clients。schema 得把两种都认下来，否则单实例写法会被标红。
	//
	// 两支必须互斥，oneOf 才有意义——两种写法都同时满足两支的话，
	// 每一份配置都「恰好满足一支」失败，全被标红。区分的办法照搬运行时
	// （xconfig.UnmarshalClients）：有 Clients 就是多实例，而且此时不许有别的 key。
	// 单实例那一支的 additionalProperties: false 本身就拒绝了 Clients
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
					Description:          "按名字组织的多个实例，实例名就是 key",
					AdditionalProperties: single,
					MinProperties:        1, // 空的 Clients 运行时是错误
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

// onlyClients 判断这个 Config 是不是「只有一个 Clients map」的形状，
// 是的话返回元素类型名
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

// structNode 把一个结构体变成 schema 对象。
//
// 不认识的字段是错误（additionalProperties: false），和运行时的严格解码一致：
// 少了这一条，拼错的字段在编辑器里照样是绿的，要等到启动才发现
func structNode(p *pkg, st *ast.StructType, desc string) (*node, error) {
	out := &node{Type: "object", Description: desc, Properties: map[string]*node{}, AdditionalProperties: false}
	if st == nil || st.Fields == nil {
		return out, nil
	}
	for _, f := range st.Fields.List {
		name := yamlName(f)
		if name == "" {
			continue // 没有 yaml 标签的字段不进配置
		}
		n, err := typeNode(p, f.Type)
		if err != nil {
			return nil, fmt.Errorf("字段 %s: %w", name, err)
		}
		n.Description = withFormat(n, fieldDoc(f))
		out.Properties[name] = n
	}
	return out, nil
}

// yamlName 取 yaml 标签里的名字
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

// typeNode 把一个 Go 类型映射成 schema 节点
func typeNode(p *pkg, expr ast.Expr) (*node, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		return identNode(p, t.Name)

	case *ast.SelectorExpr:
		// 跨包的类型：标准库只认 time.Duration，其余的是仓库里别的包的结构体（如 xtls.Config）。
		// 按包名找，所以包名要和目录名一致
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
		return nil, fmt.Errorf("没见过的类型 %s.%s", exprName(t.X), t.Sel.Name)

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
		return nil, fmt.Errorf("没见过的类型 %T", expr)
	}
}

// identNode 处理内置类型、具名标量和同包内的结构体
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
		return typeNode(p, alias) // 如 type Driver string
	}
	return nil, fmt.Errorf("没见过的类型 %s", name)
}

// orPlaceholder 一个非字符串的标量类型，外加「写成占位符」这一种字符串。
// pattern 只约束字符串，所以真正的数字、布尔值不受它影响
func orPlaceholder(typ string) *node {
	return &node{Type: []string{typ, "string"}, Pattern: placeholder}
}

// withFormat 给时长字段补一句格式提示
func withFormat(n *node, desc string) string {
	if !n.duration {
		return desc
	}
	const hint = "时长，写成 30s / 1500ms / 1h30m 这种形式；写裸数字会启动失败"
	if desc == "" {
		return hint
	}
	return desc + "（" + hint + "）"
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%T", e)
}
