package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// README 的快速开始是使用者照抄的第一段代码。改了公开 API 却没改它，
// 使用者复制下来的第一件事就是编译失败——这里把它当成代码编一遍。

func TestREADME_QuickStartCodeCompiles(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	src := firstGoBlock(string(readme))
	if src == "" {
		t.Fatal("README.md 里没找到 ```go 代码块，快速开始被挪走了？")
	}
	if !strings.Contains(src, "package main") {
		t.Fatalf("README 的第一个 go 代码块该是完整的 main.go，实际是：\n%s", src)
	}

	// 放进临时目录按文件编译：包名是 command-line-arguments，
	// import 照当前目录所在的 example 模块解析，用的就是仓库里这份代码
	path := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := exec.Command("go", "vet", path) // vet 先编译再检查，比 build 多拦一层
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("README 快速开始的代码编不过：%v\n%s\n---- 代码 ----\n%s", err, out.String(), src)
	}
}

// firstGoBlock 取 markdown 里第一个 ```go 代码块的内容，没有就返回空串
func firstGoBlock(md string) string {
	_, rest, ok := strings.Cut(md, "\n```go\n")
	if !ok {
		return ""
	}
	body, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		return ""
	}
	return body + "\n"
}
