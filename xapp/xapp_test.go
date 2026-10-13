package xapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xonetest"
)

func TestRegister_OnlyReadsConfigBuildsNothing(t *testing.T) {
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xapp" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子，App 那一块就没人读")
	}
	if got.Stage != hook.StageLog {
		t.Errorf("链路要拿 App.Name 当服务名，所以这一块必须更早读好，got=%v", got.Stage)
	}
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xapp" {
			t.Error("本包没有要关的资源，登记停止钩子会让它出现在退出序列里")
		}
	}
}

func TestNameVersion(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })

	cfg = DefaultConfig()
	if Name() != "" || Version() != "" {
		t.Errorf("默认应为空，got=%q %q", Name(), Version())
	}
	cfg = Config{Name: "xone.demo.app", Version: "v1.2.0"}
	if Name() != "xone.demo.app" || Version() != "v1.2.0" {
		t.Errorf("读到的应是配置里的值，got=%q %q", Name(), Version())
	}
}

func keepCfg(t *testing.T) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
}

func TestLoadConfig_ReadsAppBlock(t *testing.T) {
	// 服务名和版本号会被链路和指标当成 service.name / service.version，
	// 这一块没读到的话，面板上整个服务就是匿名的
	keepCfg(t)
	cfg = DefaultConfig()
	xonetest.UseConfigYAML(t, "XApp:\n  Name: xone.demo.app\n  Version: v1.2.0\n")

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Name() != "xone.demo.app" || Version() != "v1.2.0" {
		t.Errorf("配置没读进来，got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_StaysEmptyWhenUnset(t *testing.T) {
	keepCfg(t)
	cfg = DefaultConfig()
	xonetest.UseConfigYAML(t, "XLog:\n  Level: info\n")

	if err := loadConfig(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	if Name() != "" || Version() != "" {
		t.Errorf("没配时应为空，got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_BadConfigFailsStartup(t *testing.T) {
	keepCfg(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Nmae: demo\n")

	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败，否则服务名会一直是空的而没人知道")
	}
}

func TestLoadConfig_DoesNotCarryPreviousValue(t *testing.T) {
	// 同一进程里跑第二次 Run（测试里常见）：这次没写的字段该回到默认值，
	// 而不是沿用上一次的服务名
	keepCfg(t)
	cfg = Config{Name: "last.run", Version: "v0"}
	xonetest.UseConfigYAML(t, "XApp:\n  Version: v1.2.0\n")

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Name() != "" || Version() != "v1.2.0" {
		t.Errorf("该只剩这次配置里的值，got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_DecodeFailureKeepsCurrentValue(t *testing.T) {
	keepCfg(t)
	cfg = Config{Name: "kept"}
	xonetest.UseConfigYAML(t, "XApp:\n  Name: half\n  Version: [1, 2]\n")

	if err := loadConfig(context.Background()); err == nil {
		t.Fatal("类型不对应当报错")
	}
	if Name() != "kept" || Version() != "" {
		t.Errorf("失败了就不该动现有的值，got=%q %q", Name(), Version())
	}
}

func TestLoadConfig_LoadsBeforeReadingIfNotLoaded(t *testing.T) {
	// 读得早拿到的也是文件里的值，不是一份静默的默认值
	keepCfg(t)
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte("XApp:\n  Name: early\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	t.Setenv(config.EnvKey, p)

	if err := loadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Name() != "early" {
		t.Errorf("读到的应是文件里的值，got=%q", Name())
	}
}
