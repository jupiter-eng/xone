// Package xonetest 给使用者的测试用：换一份配置、跑一遍启动钩子，不起整个服务。
//
//	func TestInitXKV(t *testing.T) {
//		xonetest.UseConfigYAML(t, "XKV:\n  Path: /tmp/kv.json\n")
//		xonetest.StartHooks(t) // 测试结束时自动跑配对的停止钩子
//		if xkv.C() == nil { ... }
//	}
//
// 配置和钩子都是进程级的全局状态，所以用了本包的测试不能 t.Parallel。
//
// 本包不引入任何新的依赖。
package xonetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
)

// UseConfig 让这个测试读 path 这份配置：xconfig.Unmarshal 读到的就是它，
// 和 xone.Run 加载时走的是同一条路（Import、profile、${VAR} 都照常生效）。
//
// 加载失败时测试直接失败。测试结束时清掉，下一个测试从「还没加载」开始。
func UseConfig(t testing.TB, path string) {
	t.Helper()
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		t.Fatalf("xonetest: load config %s: %v", path, err)
	}
}

// UseConfigYAML 同 UseConfig，配置内容直接写在测试里：
// 它被写进临时目录下的 application.yml 再加载。
func UseConfigYAML(t testing.TB, yml string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatalf("xonetest: write config: %v", err)
	}
	UseConfig(t, p)
}

// StartHooks 按档位跑一遍已登记的全部启动钩子，和 xone.Run 的顺序一样；
// 测试结束时逆序跑停止钩子——同样只跑和成功了的启动钩子配对的那些。
//
// 跑的是这个测试二进制里登记过的全部钩子，也就是被测包连同它 import 的集成。
// 一个启动钩子失败时测试直接失败，已经成功的照样会被关掉。
//
// 不起服务、不接管退出信号、不检查没人读的配置 key、停止钩子也不限时：
// 这些是 xone.Run 的事，要测它们就直接调 Run。
func StartHooks(t testing.TB) {
	t.Helper()
	started := map[int]bool{}
	t.Cleanup(func() {
		for _, e := range hook.StopAfter(started) {
			if err := e.Run(context.Background()); err != nil {
				t.Errorf("xonetest: stop hook %s: %v", e.Name, err)
			}
		}
	})
	for _, e := range hook.Start() {
		if err := e.Run(context.Background()); err != nil {
			t.Fatalf("xonetest: start hook %s: %v", e.Name, err)
		}
		started[e.Seq] = true
	}
}
