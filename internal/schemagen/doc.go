package main

import (
	"go/ast"
	"strings"
)

// fieldDoc 把字段上的注释变成一行说明。
//
// 注释的写法是固定的：
//
//	// Timeout 单次尝试的超时。默认 60s。
//	//
//	// 更长的解释……
//	Timeout time.Duration `yaml:"Timeout"`
//
// 取整段、去掉开头重复的字段名、合并成一行。不只取第一句是因为
// 这个仓库的注释常把「为什么」写在后面几行，而那几行恰恰是最该
// 出现在 IDE 悬浮提示里的内容
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

// docOf 取类型上的注释
func docOf(p *pkg, typeName string) string {
	if d, ok := p.docs[typeName]; ok {
		return flatten(d, typeName)
	}
	return ""
}

// flatten 把注释组压成一行，去掉开头的 "字段名 "
func flatten(g *ast.CommentGroup, name string) string {
	lines := make([]string, 0, len(g.List))
	for _, c := range g.List {
		raw := c.Text // *ast.Comment.Text 是字段，带着 "//" 前缀
		// 代码示例那几行（缩进过的）在一行里读不通，丢掉
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
	// 代码示例被丢掉之后常留下一个「两种写法：」这样的残句，
	// 结尾的冒号在这里已经指不到任何东西了
	return strings.TrimRight(strings.TrimSpace(out), "：:")
}
