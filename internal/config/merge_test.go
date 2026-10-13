package config

import (
	"strings"
	"testing"
	"time"
)

// ---- 空文件：叠上来什么都不改 ----

func TestLoad_EmptyProfileFileChangesNothing(t *testing.T) {
	// 空文件和只有注释的文件解析出来是一个 Kind 为 0 的节点，不是文档节点。
	// 从前它走不进「文档节点」那一支，被当成标量整个替换掉了前面所有文件——
	// application-prod.yml 里只写一行 # TODO，全部配置就悄悄退回默认值
	for name, body := range map[string]string{
		"空文件":   "",
		"只有注释":  "# TODO\n",
		"只有空行":  "\n\n",
		"只有文档头": "---\n",
	} {
		t.Run(name, func(t *testing.T) {
			withProfileEnv(t, "prod")
			base := files(t, "application.yml", map[string]string{
				"application.yml":      "Demo:\n  Addr: base:1\n  Timeout: 2s\n",
				"application-prod.yml": body,
			})
			c := listComps(t)
			if err := LoadInto(base, "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != "base:1" || c.Timeout != 2*time.Second {
				t.Errorf("空的 profile 文件不该动 base 的配置，got Addr=%q Timeout=%v", c.Addr, c.Timeout)
			}
		})
	}
}

func TestLoad_ImportOfEmptyFileChangesNothing(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: empty.yml\nDemo:\n  Addr: base:1\n",
		"empty.yml":       "# 以后再填\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "base:1" {
		t.Errorf("import 一个空文件不该动引它的文件，Addr=%q", c.Addr)
	}
}

func TestLoad_BaseEmpty_ProfileStillApplies(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "# 全在 profile 里\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

// ---- 重复 key：每个文件都查，不只第一个 ----

func TestLoad_DuplicateKeyInLaterFileIsError(t *testing.T) {
	// 从前只有第一个文件的重复能被发现：合并时 override 里的同名 key
	// 被按名字去重，后面的严格解码和顶层检查都再也看不见它
	cases := map[string]string{
		"顶层": "Demo:\n  Addr: prod:1\nDemo:\n  Addr: prod:2\n",
		"嵌套": "Demo:\n  Addr: prod:1\n  Addr: prod:2\n",
		"深层": "Demo:\n  Nested:\n    A: x\n    A: y\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			withProfileEnv(t, "prod")
			base := files(t, "application.yml", map[string]string{
				"application.yml":      "Demo:\n  Addr: base:1\n",
				"application-prod.yml": body,
			})
			err := LoadInto(base, "Demo", listComps(t))
			if err == nil {
				t.Fatal("profile 文件里的重复 key 该报错")
			}
			for _, want := range []string{"duplicate", "application-prod.yml", "line"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误里该出现 %q（要能定位到文件和行），got=%v", want, err)
				}
			}
		})
	}
}

func TestLoad_ImportDuplicateKeyInImportedFileIsError(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":      "Demo:\n  Addr: a:1\n  Timeout: 1s\n  Addr: a:2\n",
	})
	err := LoadInto(base, "Demo", listComps(t))
	if err == nil {
		t.Fatal("import 进来的文件里的重复 key 该报错")
	}
	for _, want := range []string{"duplicate", "shared.yml", "line 4", "line 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里该出现 %q，got=%v", want, err)
		}
	}
}

func TestLoad_DuplicateKeyInUnreadBlockIsError(t *testing.T) {
	// 嵌套的重复原本靠严格解码发现，那只在有人 Unmarshal 这一块时才发生
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Retries: 1\nOther:\n  X: 1\n  X: 2\n"), "Demo", c)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("重复 key 该在加载时就报出来，got=%v", err)
	}
}

// ---- null：在叠加的文件里写 null 等于没写 ----

func TestLoad_NullInOverlayDoesNotOverrideLowerPriority(t *testing.T) {
	// 单个文件里 null 保持结构体默认值；叠加时同样不该改变任何东西。
	// 从前 `Demo:` 这个空块整个替换掉了 base 的 mapping，
	// 而 `Demo: {}` 却保留 base——一个空块两种写法两种结果
	cases := map[string]string{
		"空块":       "Demo:\n",
		"显式null":   "Demo: null\n",
		"波浪号":      "Demo: ~\n",
		"空mapping": "Demo: {}\n",
		"字段为null":  "Demo:\n  Addr:\n",
		"列表为null":  "Demo:\n  Headers: ~\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			withProfileEnv(t, "prod")
			base := files(t, "application.yml", map[string]string{
				"application.yml":      "Demo:\n  Addr: base:1\n  Headers: [a]\n",
				"application-prod.yml": body,
			})
			c := listComps(t)
			if err := LoadInto(base, "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != "base:1" || len(c.Headers) != 1 {
				t.Errorf("null 不该覆盖 base，got Addr=%q Headers=%v", c.Addr, c.Headers)
			}
		})
	}
}

func TestLoad_OverlayClearsWithEmptyValueNotNull(t *testing.T) {
	// null 是「没写」，要清空就写出空的那个值
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n  Headers: [a]\n",
		"application-prod.yml": "Demo:\n  Addr: \"\"\n  Headers: []\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "" || len(c.Headers) != 0 {
		t.Errorf("空字符串和空列表该覆盖 base，got Addr=%q Headers=%v", c.Addr, c.Headers)
	}
}
