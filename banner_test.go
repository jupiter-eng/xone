package xone

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
)

// plain 去掉颜色码，只看文字
func plain(s string) string { return regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(s, "") }

func TestPrintBanner_PrintsArtAndVersionOnTerminal(t *testing.T) {
	var b bytes.Buffer
	printBanner(&b, true)
	out := plain(b.String())
	for _, want := range append(bannerText, ":: xone ::", version()) {
		if !strings.Contains(out, want) {
			t.Errorf("banner 里该有 %q，got=\n%s", want, out)
		}
	}
}

func TestPrintBanner_WritesNothingWhenNotTerminal(t *testing.T) {
	// 容器、重定向、日志采集器后面：多行字符画就是日志平台里解析失败的垃圾
	var b bytes.Buffer
	printBanner(&b, false)
	if b.Len() != 0 {
		t.Errorf("不是终端时不该写 banner，got=%q", b.String())
	}
}

func TestIsTerminal_PipesAndFilesAreNotTerminals(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(w) {
		t.Error("管道不该被当成终端")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("普通文件不该被当成终端")
	}
}

func TestRun_StderrNotTerminal_NoBanner(t *testing.T) {
	// 调用点：Run 得真的拿 stderr 去判断，而不是不管三七二十一都打
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := stderr
	stderr = w
	t.Cleanup(func() { stderr = old })

	comps(t)
	if err := Run(Func(func(context.Context) error { return nil }), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	w.Close()
	out, _ := io.ReadAll(r)
	if strings.Contains(plain(string(out)), bannerText[0]) {
		t.Errorf("stderr 是管道时不该打 banner，got=%q", out)
	}
}

func TestModuleVersion_ComesFromBuildInfo(t *testing.T) {
	for name, c := range map[string]struct {
		bi   debug.BuildInfo
		want string
	}{
		"作为依赖发布版":     {debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v0.2.0"}}}, "v0.2.0"},
		"replace 到本地": {debug.BuildInfo{Deps: []*debug.Module{{Path: modulePath, Version: "v0.2.0", Replace: &debug.Module{Path: "../xone"}}}}, "(devel)"},
		"本仓库自己":       {debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, "(devel)"},
		"没依赖它":        {debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}}, "(devel)"},
	} {
		if got := moduleVersion(&c.bi); got != c.want {
			t.Errorf("%s：got=%q want=%q", name, got, c.want)
		}
	}
}

func TestRun_XONE_DEBUGListsStartHookOrder(t *testing.T) {
	var b bytes.Buffer
	old := config.DebugOut
	config.DebugOut = &b
	t.Cleanup(func() { config.DebugOut = old })
	t.Setenv(config.DebugEnvKey, "1")

	r := &recorder{}
	comps(t, comp("db", hook.StageClient, r, nil), comp("log", hook.StageLog, r, nil))
	if err := Run(Func(func(context.Context) error { return nil }), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	i, j := strings.Index(out, "Log"), strings.Index(out, "Client")
	if !strings.Contains(out, "start hooks, in order") || i < 0 || j < 0 || i > j {
		t.Errorf("该按执行顺序（Log 在 Client 前面）列出启动钩子，got=\n%s", out)
	}
}

func TestRun_NoDebugOutputWithoutXONE_DEBUG(t *testing.T) {
	var b bytes.Buffer
	old := config.DebugOut
	config.DebugOut = &b
	t.Cleanup(func() { config.DebugOut = old })
	t.Setenv(config.DebugEnvKey, "")

	comps(t)
	if err := Run(Func(func(context.Context) error { return nil }), WithConfigPath(emptyConf(t)), WithLogger(quietLogger())); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 0 {
		t.Errorf("没开 XONE_DEBUG 不该有调试输出，got=\n%s", b.String())
	}
}
