package config

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/xerror"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fresh 回到「还没加载」，并清掉会影响 Locate 的环境变量
func fresh(t *testing.T) {
	t.Helper()
	Reset()
	t.Cleanup(Reset)
	t.Setenv(EnvKey, "")
}

func TestUnmarshal_LoadsFirstIfNotLoaded(t *testing.T) {
	// 要防的是：读得早就静默拿到空值，服务带着一套默认配置正常起来。
	// 现在第一次读就先加载：读得早拿到的也是文件里的最终值
	fresh(t)
	t.Setenv(EnvKey, write(t, "Demo:\n  Addr: from-file\n"))

	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "from-file" {
		t.Errorf("读到的应是文件里的值，got=%q", c.Addr)
	}
}

func TestEnsure_ReusesEarlierLoad(t *testing.T) {
	// 提前读过的块已经认领过了，Run 接手时不能把认领记录清掉，
	// 否则它会被报成「没人读」而让启动失败
	fresh(t)
	p := write(t, "Demo:\n  Addr: x\n")
	t.Setenv(EnvKey, p)

	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}
	if err := Ensure("", quiet()); err != nil {
		t.Fatalf("不点名时应沿用已加载的那一份，got=%v", err)
	}
	if err := Ensure(p, quiet()); err != nil {
		t.Fatalf("点名的正是已加载的那个文件，不该报错，got=%v", err)
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("提前读过的块应当算认领过，got=%v", got)
	}
}

func TestEnsure_SameFileSpelledDifferentlyIsFine(t *testing.T) {
	// 从前按字符串比：dir/./b.yml 和 dir/b.yml 被当成两个文件，Run 启动失败
	p := write(t, "Demo:\n  Addr: x\n")
	dir := filepath.Dir(p)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.yml")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	for name, same := range map[string]string{
		"带 ./":  dir + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(p),
		"带 ../": filepath.Join(dir, "sub") + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(p),
		"相对路径":  rel,
		"经符号链接": link,
	} {
		t.Run(name, func(t *testing.T) {
			fresh(t)
			t.Setenv(EnvKey, p)
			c := defaults()
			if err := Unmarshal("Demo", &c); err != nil {
				t.Fatal(err)
			}
			if err := Ensure(same, quiet()); err != nil {
				t.Errorf("%s 和 %s 是同一个文件，不该报错，got=%v", same, p, err)
			}
		})
	}
}

func TestEnsure_FailsWhenNamedFileDiffersFromEarlierLoad(t *testing.T) {
	// 悄悄换一份的话，提前读到的值和之后组件里读到的对不上
	fresh(t)
	t.Setenv(EnvKey, write(t, "Demo:\n  Addr: a\n"))
	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}

	err := Ensure(write(t, "Demo:\n  Addr: b\n"), quiet())
	if err == nil {
		t.Fatal("点名另一个文件应当报错，而不是悄悄换掉已经读过的那一份")
	}
	if !strings.Contains(err.Error(), "--"+ArgKey) || !strings.Contains(err.Error(), EnvKey) {
		t.Errorf("错误里要告诉使用者该怎么改，got=%v", err)
	}
}

func TestEnsure_UsesDefaultsWhenNoConfigFile(t *testing.T) {
	// 从前这种情况只打一条告警，配置却一直停在「还没加载」：之后每个集成
	// 读配置都报「读早了」，一个不需要任何配置的服务根本起不来
	fresh(t)
	chdir(t, t.TempDir()) // 约定路径下什么都没有

	if err := Ensure("", quiet()); err != nil {
		t.Fatalf("没有配置文件应当只告警，got=%v", err)
	}
	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatalf("没有配置文件时读配置应当拿到默认值，got=%v", err)
	}
	if c != defaults() {
		t.Errorf("默认值应原样保留，got=%+v", c)
	}
}

func TestEnsure_MissingNamedFileIsError(t *testing.T) {
	fresh(t)
	err := Ensure(filepath.Join(t.TempDir(), "nope.yml"), quiet())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("点名要的文件找不到应当报错，got=%v", err)
	}
}

func TestUnmarshal_SameErrorOnEveryReadAfterLoadFailure(t *testing.T) {
	// 写坏了的配置不该在第二次读时悄悄当成没配，也不该每读一次重新解析一遍
	fresh(t)
	t.Setenv(EnvKey, write(t, "Demo: [\n"))

	c := defaults()
	first := Unmarshal("Demo", &c)
	if first == nil {
		t.Fatal("写坏了的配置应当报错")
	}
	if again := Unmarshal("Demo", &c); !errors.Is(again, first) {
		t.Errorf("第二次读应报同一个错，got=%v", again)
	}
	if err := Ensure("", quiet()); !errors.Is(err, first) {
		t.Errorf("Run 接手时也应报同一个错，got=%v", err)
	}
}

func TestReset_RelocatesAndReloadsAfterward(t *testing.T) {
	// 配置跟着一次 Run 走：同一个进程里的下一次 Run 要读它自己的那一份
	fresh(t)
	if err := Load(write(t, "Demo:\n  Addr: a\n")); err != nil {
		t.Fatal(err)
	}
	Reset()
	t.Setenv(EnvKey, write(t, "Demo:\n  Addr: b\n"))

	c := defaults()
	if err := Unmarshal("Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b" {
		t.Errorf("Reset 之后应读新的那一份，got=%q", c.Addr)
	}
}

func TestUnmarshalAndDecodeStrict_BadTargetIsErrorNotPanic(t *testing.T) {
	// 传了值而不是指针、或者传了 nil：从前当场 panic（reflect 在 nil 类型上取 Kind、
	// yaml 往不可寻址的值里写），整个进程在启动钩子里崩掉
	if err := Load(write(t, "Demo:\n  Addr: a:1\n")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Reset)
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("Addr: a:1\n"), &node); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]any{
		"nil": nil, "value": demoConf{}, "nil pointer": (*demoConf)(nil),
	} {
		check := func(what string, err error) {
			t.Helper()
			var xe *xerror.Error
			if !errors.As(err, &xe) || xe.Module != "xconfig" || xe.Op != "config" ||
				!strings.Contains(err.Error(), "non-nil pointer") {
				t.Errorf("%s(%s) 该返回 xconfig 的 config 错误，got=%v", what, name, err)
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Unmarshal(%s) panic 了：%v", name, r)
				}
			}()
			check("Unmarshal", Unmarshal("Demo", target))
			check("Unmarshal 没配的块", Unmarshal("NotConfigured", target))
		}()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DecodeStrict(%s) panic 了：%v", name, r)
				}
			}()
			check("DecodeStrict", DecodeStrict(&node, target))
		}()
	}
}
