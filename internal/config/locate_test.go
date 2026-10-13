package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withArgs 临时替换命令行参数
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	old := os.Args
	os.Args = append([]string{"svc"}, args...)
	t.Cleanup(func() { os.Args = old })
}

func TestLocate_FlagTakesPrecedence(t *testing.T) {
	withArgs(t, "--config=/a/b.yml")
	t.Setenv(EnvKey, "/from/env.yml")

	if got, _ := locate(); got != "/a/b.yml" {
		t.Errorf("启动参数应优先于环境变量，got=%q", got)
	}
}

func TestLocate_FlagBothForms(t *testing.T) {
	t.Run("等号", func(t *testing.T) {
		withArgs(t, "--config=/a.yml")
		if got, _ := locate(); got != "/a.yml" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("空格", func(t *testing.T) {
		withArgs(t, "--config", "/b.yml")
		if got, _ := locate(); got != "/b.yml" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("单横线", func(t *testing.T) {
		withArgs(t, "-config=/c.yml")
		if got, _ := locate(); got != "/c.yml" {
			t.Errorf("got=%q", got)
		}
	})
}

func TestLocate_LeavesOtherArgsAlone(t *testing.T) {
	// 使用者的程序有自己的命令行参数，框架不该误读
	withArgs(t, "--port", "8080", "--configx=/x.yml", "--verbose")
	t.Setenv(EnvKey, "")

	if got, _ := locate(); got != "" && got != SearchPaths[0] {
		t.Errorf("不该把别的参数当成配置路径，got=%q", got)
	}
}

func TestLocate_EnvVarIsSecond(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "/from/env.yml")

	if got, _ := locate(); got != "/from/env.yml" {
		t.Errorf("got=%q", got)
	}
}

func TestLocate_SearchesConventionalPaths(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "")

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "conf", "application.yml"), []byte("{}"), 0o600)
	chdir(t, dir)

	if got, _ := locate(); got != "conf/application.yml" {
		t.Errorf("应命中约定路径，got=%q", got)
	}
}

func TestLocate_ReturnsEmptyWhenNothingFound(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "")
	chdir(t, t.TempDir())

	if got, _ := locate(); got != "" {
		t.Errorf("找不到时应返回空串，由调用方决定怎么办，got=%q", got)
	}
}

// chdir 切换工作目录，测试结束后切回。
//
// 不用 testing.T.Chdir：它要 Go 1.24，而核心模块的 go 指令定的是所有使用者的
// 语言版本下限——为一个测试助手把下限抬两个版本不值得。
// 代价是没有它对 t.Parallel 的检查，所以用到它的测试不要并行。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestEnsure_FlagWithoutValueIsError(t *testing.T) {
	// 从前 --config 写在最后、忘了带值时被静默忽略，接着按 XONE_CONFIG、约定路径找，
	// 起来的是另一份配置——或者一份全是默认值的
	for _, key := range []string{ArgKey, ProfileArgKey} {
		fresh(t)
		t.Setenv(EnvKey, write(t, "Demo:\n  Addr: from-env\n"))
		withArgs(t, "serve", "--"+key)
		err := Ensure("", quiet())
		if err == nil || !strings.Contains(err.Error(), "--"+key+" needs a value") {
			t.Errorf("--%s 没带值该报错，got=%v", key, err)
		}
	}
}
