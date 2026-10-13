package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 配置写错时进程起不来，错误里点名是哪一项。docs/config.md「通用规则」：
//
//	字段拼错             启动失败，不是静默忽略
//	多配了没人认领的块    启动失败，并提示可能是忘了 import 对应的包
//	${VAR}               必填，未设置则启动失败
//
// 写错的那一项用 Options.Overlay 叠上去（一份 profile），和生产上的写法一样
func TestFunctional_BadConfigFailsStartupAndNamesTheField(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	unset := "E2E_UNSET_" + strings.ToUpper(harness.NewID())
	for _, c := range []struct {
		name    string
		overlay string
		want    []string // stderr 里必须有的片段
	}{
		{"顶层 key 拼错", "XGinn:\n  Port: 1\n", []string{"XGinn", "not read by anyone", "whether the matching package is imported"}},
		{"业务的顶层块拼错", "Servcie:\n  Table: x\n", []string{"Servcie", "not read by anyone"}},
		{"XRedis 里的字段拼错", "XRedis:\n  Adrr: 127.0.0.1:6379\n", []string{"XRedis", "field Adrr not found"}},
		{"XGin 里的字段拼错", "XGin:\n  Prot: 1\n", []string{"XGin", "field Prot not found"}},
		{"嵌套的字段拼错", "XLog:\n  File:\n    Enabel: true\n", []string{"XLog", "field Enabel not found"}},
		{"业务配置块里的字段拼错", "Service:\n  Tabel: x\n", []string{"Service", "field Tabel not found"}},
		{"${VAR} 没设置", "Service:\n  Downstream: \"${" + unset + "}\"\n", []string{"environment variables not set", unset}},
		{"${VAR} 没设置（框架的块里、列表元素里）", "XGin:\n  TrustedProxies: [\"${" + unset + "}\"]\n", []string{"environment variables not set", unset}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Overlay: c.overlay, NoWait: true})
			exit, ok := p.Wait(30 * time.Second)
			if !ok {
				t.Fatalf("文档说这种写法启动失败，进程 30s 了还在跑\n%s", p.Output())
			}
			if exit.Code == 0 || exit.Signal != nil {
				t.Fatalf("文档说启动失败，实际 %v\n%s", exit, p.Output())
			}
			stderr := p.Stderr()
			for _, w := range c.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("错误信息应点名是哪一项（含 %q），实际 stderr：\n%s", w, stderr)
				}
			}
			// 起不来的意思是一个请求都没接：xgin 从没开始监听
			if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" })) != 0 {
				t.Errorf("配置写错时服务不该开始监听，实际看到了 xgin listening")
			}
			t.Logf("数字：%s → %v 后退出（%v）；错误：%s", c.name, exit.Uptime, exit, lastLine(stderr))
		})
	}

	// 字段拼错的报错点名文件和行号。docs/config.md 对重复 key 承诺「报出文件和两处的行号」，
	// 行号是给人去配置文件里找那一行的；报出来的应该就是那个文件的那一行。
	// 从前严格解码先把那一段 yaml.Marshal 成文本再解，报的是那段文本里的行号（第 70 行报成 line 2），
	// 也没有文件名——写在 profile 文件里的拼错更是无从找起
	wantAt := func(t *testing.T, p *harness.Process, file string, line int) {
		t.Helper()
		if _, ok := p.Wait(30 * time.Second); !ok {
			t.Fatalf("字段拼错应启动失败，30s 了还在跑")
		}
		want := fmt.Sprintf("%s:%d: field Prot not found", file, line)
		if !strings.Contains(p.Stderr(), want) {
			t.Errorf("报错该点名文件和行号（含 %q），实际 stderr：\n%s", want, p.Stderr())
		}
		t.Logf("数字：报错 %s", lastLine(p.Stderr()))
	}
	t.Run("字段拼错时报出配置文件和那一行", func(t *testing.T) {
		t.Parallel()
		base, err := os.ReadFile(filepath.Join(harness.ModuleDir(), "service", "application.yml"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(base), "\n")
		at := -1
		for i, l := range lines {
			if l == "  Port: ${E2E_PORT}" {
				lines[i], at = "  Prot: ${E2E_PORT}", i+1
			}
		}
		if at < 0 {
			t.Fatal("service/application.yml 里找不到 XGin 的 Port 那一行")
		}
		cfg := filepath.Join(t.TempDir(), "typo.yml")
		if err := os.WriteFile(cfg, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		wantAt(t, harness.Start(t, harness.Options{Config: cfg, NoWait: true}), cfg, at)
	})
	t.Run("profile 文件里拼错时报出 profile 文件和那一行", func(t *testing.T) {
		t.Parallel()
		// XGin 块和 base 里的合并，Prot 这一项来自 profile 文件的第 4 行
		p := harness.Start(t, harness.Options{Overlay: "# overlay\n\nXGin:\n  Prot: 1\n", NoWait: true})
		wantAt(t, p, filepath.Join(p.Dir, "application-e2e.yml"), 4)
	})
}

// lastLine 最后一个非空行，打日志用
func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return ls[len(ls)-1]
}
