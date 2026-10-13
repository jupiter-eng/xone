package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

type demoConf struct {
	Addr    string        `yaml:"Addr"`
	Timeout time.Duration `yaml:"Timeout"`
	Retries int           `yaml:"Retries"`
	Enable  bool          `yaml:"Enable"`
}

func defaults() demoConf {
	return demoConf{Addr: "127.0.0.1:5432", Timeout: 5 * time.Second, Retries: 3, Enable: true}
}

// comps 造一份填好默认值的配置，供 LoadInto 解进去
func comps(t *testing.T) *demoConf {
	t.Helper()
	c := defaults()
	return &c
}

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_DefaultsKeptOnUnsetFields(t *testing.T) {
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: \"10.0.0.1:1\"\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "10.0.0.1:1" {
		t.Errorf("写了的字段应被覆盖，got=%q", c.Addr)
	}
	if c.Timeout != 5*time.Second || c.Retries != 3 || !c.Enable {
		t.Errorf("没写的字段应保留默认值，got=%+v", *c)
	}
}

func TestLoad_ExplicitZeroOverridesDefault(t *testing.T) {
	// 这正是 *bool 指针模式想解决的问题，预填默认值天然就有这个语义
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Enable: false\n  Retries: 0\n  Addr: \"\"\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Enable || c.Retries != 0 || c.Addr != "" {
		t.Errorf("显式写的零值应覆盖默认值，got=%+v", *c)
	}
}

func TestLoad_DurationParsedNatively(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want time.Duration
	}{
		{"Demo:\n  Timeout: 30s\n", 30 * time.Second},
		{"Demo:\n  Timeout: \"1500ms\"\n", 1500 * time.Millisecond},
		{"Demo:\n  Timeout: 2m\n", 2 * time.Minute},
	} {
		c := comps(t)
		if err := LoadInto(write(t, tc.yaml), "Demo", c); err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if c.Timeout != tc.want {
			t.Errorf("%q -> %v, want %v", tc.yaml, c.Timeout, tc.want)
		}
	}
}

func TestLoad_BadDurationFormatFailsFast(t *testing.T) {
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Timeout: 那么久\n"), "Demo", &c)
	if err == nil {
		t.Fatal("时长格式不对应该启动失败，而不是静默变成 0")
	}
}

func TestLoad_MisspelledFieldIsError(t *testing.T) {
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Adrr: \"x\"\n"), "Demo", &c)
	if err == nil {
		t.Fatal("拼错的字段应该报错，而不是被忽略")
	}
	if !strings.Contains(err.Error(), "Demo") {
		t.Errorf("错误里应指明是哪个块出的问题，got=%v", err)
	}
}

func TestUnclaimed_ReportsBlocksNobodyRead(t *testing.T) {
	// 读是各集成包自己在启动钩子里做的，所以「没人要」这件事只有等钩子
	// 全跑完才知道。框架据此报错，它同时覆盖「key 拼错」和「忘了 import」
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\nXRedis:\n  Addr: y\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	got := Unclaimed()
	if len(got) != 1 || got[0] != "XRedis" {
		t.Errorf("只有没人读过的 XRedis 该被报出来，got=%v", got)
	}
}

func TestLoad_Placeholder(t *testing.T) {
	t.Run("环境变量已设置", func(t *testing.T) {
		t.Setenv("XONE_T_ADDR", "10.1.1.1:9")
		c := comps(t)
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_ADDR}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "10.1.1.1:9" {
			t.Errorf("got=%q", c.Addr)
		}
	})

	t.Run("未设置时用默认值", func(t *testing.T) {
		os.Unsetenv("XONE_T_UNSET")
		c := comps(t)
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_UNSET:1.2.3.4:5}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "1.2.3.4:5" {
			t.Errorf("默认值里含冒号也应完整保留，got=%q", c.Addr)
		}
	})

	t.Run("必填未设置则启动失败", func(t *testing.T) {
		os.Unsetenv("XONE_T_REQUIRED")
		c := comps(t)
		err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_REQUIRED}\"\n"), "Demo", &c)
		if err == nil || !strings.Contains(err.Error(), "XONE_T_REQUIRED") {
			t.Fatalf("必填占位符缺失应报错并指出变量名，got=%v", err)
		}
	})

	t.Run("显式空串覆盖默认值", func(t *testing.T) {
		t.Setenv("XONE_T_EMPTY", "")
		c := comps(t)
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_EMPTY:fallback}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "" {
			t.Errorf("显式设为空串是有效取值，应覆盖默认值，got=%q", c.Addr)
		}
	})
}

func TestLoad_PlaceholderFillsNonStringField(t *testing.T) {
	// 文档把 ${VAR} 写成一条通用规则，那它就得对所有字段类型成立。
	// 解析时整个 ${PORT:8080} 是一段文本，标量因此被打上 !!str；
	// 替换之后不重新判定类型的话，这些字段全都以
	// "cannot unmarshal !!str into int" 失败——占位符沦为字符串字段专用。
	type demo struct {
		Retries int           `yaml:"Retries"`
		Enable  bool          `yaml:"Enable"`
		Ratio   float64       `yaml:"Ratio"`
		Timeout time.Duration `yaml:"Timeout"`
		Addr    string        `yaml:"Addr"`
	}

	t.Run("用默认值", func(t *testing.T) {
		os.Unsetenv("XONE_T_N1")
		c := demo{}
		body := "Demo:\n  Retries: ${XONE_T_N1:7}\n  Enable: ${XONE_T_N1:true}\n" +
			"  Ratio: ${XONE_T_N1:0.25}\n  Timeout: ${XONE_T_N1:90s}\n  Addr: ${XONE_T_N1:h:1}\n"
		if err := LoadInto(write(t, body), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		want := demo{Retries: 7, Enable: true, Ratio: 0.25, Timeout: 90 * time.Second, Addr: "h:1"}
		if c != want {
			t.Errorf("got=%+v want=%+v", c, want)
		}
	})

	t.Run("取环境变量", func(t *testing.T) {
		t.Setenv("XONE_T_N2", "42")
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Retries: ${XONE_T_N2}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Retries != 42 {
			t.Errorf("Retries 应取自环境变量，got=%d", c.Retries)
		}
	})

	t.Run("裸数字进时长字段照样报错", func(t *testing.T) {
		// 重新判定类型不能顺手把这条保证放掉：${T:30} 和直接写 30 是一回事，
		// 写的人想要 30 秒，不是 30 纳秒
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Timeout: ${XONE_T_N3:30}\n"), "Demo", &c); err == nil {
			t.Fatal("占位符里的裸数字同样应当报错")
		}
	})

	t.Run("加了引号就仍按字符串处理", func(t *testing.T) {
		// 数字形态的密码、版本号指望这一条：显式引号是明确的意图，不该被重新判定
		t.Setenv("XONE_T_N4", "0123456")
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_N4}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "0123456" {
			t.Errorf("引号里的值应原样进字符串字段，got=%q", c.Addr)
		}
	})

	t.Run("不加引号的数字进字符串字段也不丢前导零", func(t *testing.T) {
		t.Setenv("XONE_T_N5", "0123456")
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_N5}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "0123456" {
			t.Errorf("重新判定类型不该改变字符串字段读到的内容，got=%q", c.Addr)
		}
	})

	t.Run("空值不重新判定", func(t *testing.T) {
		// "" 重新判定会变成 null，把结构体里预填的默认值清成零值
		t.Setenv("XONE_T_N6", "")
		c := demo{Retries: 3}
		if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_N6}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "" || c.Retries != 3 {
			t.Errorf("got=%+v", c)
		}
	})
}

func TestLoad_PlaceholderInMapAndSliceTypedByExpandedContent(t *testing.T) {
	// 展开是在整棵节点树上做的，map 的 value、切片元素、嵌套结构体都要对。
	// 尤其是 map[string]string：重新判定类型不该把 "123" 变成读不进 string 的东西
	os.Unsetenv("XONE_T_C1")
	type demo struct {
		Labels  map[string]string `yaml:"Labels"`
		Names   []string          `yaml:"Names"`
		Buckets []float64         `yaml:"Buckets"`
		Nested  struct {
			Max int `yaml:"Max"`
		} `yaml:"Nested"`
	}

	body := "Demo:\n" +
		"  Labels:\n    env: ${XONE_T_C1:dev}\n    ver: ${XONE_T_C1:123}\n    on: ${XONE_T_C1:true}\n" +
		"  Names:\n    - ${XONE_T_C1:a}\n    - ${XONE_T_C1:7}\n" +
		"  Buckets:\n    - ${XONE_T_C1:0.1}\n    - ${XONE_T_C1:1}\n" +
		"  Nested:\n    Max: ${XONE_T_C1:9}\n"

	c := demo{}
	if err := LoadInto(write(t, body), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"env": "dev", "ver": "123", "on": "true"}
	if !reflect.DeepEqual(c.Labels, want) {
		t.Errorf("map 的 string value 应原样是文本，got=%v", c.Labels)
	}
	if !reflect.DeepEqual(c.Names, []string{"a", "7"}) {
		t.Errorf("字符串切片，got=%v", c.Names)
	}
	if !reflect.DeepEqual(c.Buckets, []float64{0.1, 1}) {
		t.Errorf("数值切片，got=%v", c.Buckets)
	}
	if c.Nested.Max != 9 {
		t.Errorf("嵌套结构体，got=%d", c.Nested.Max)
	}
}

func TestLoad_PlaceholderSpecialCharsDoNotBreakStructure(t *testing.T) {
	// 在解析后的节点上展开，而不是对原始字节做文本替换 —— 否则这是条注入路径
	t.Setenv("XONE_T_INJECT", "a: b\nEvil: true")
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_INJECT}\"\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "a: b\nEvil: true" {
		t.Errorf("环境变量的值应原样进字段，不该被当成 YAML 解析，got=%q", c.Addr)
	}

	// 不加引号时标量会被重新判定类型，那一步同样不能让值逃出标量本身
	c2 := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_INJECT}\n"), "Demo", c2); err != nil {
		t.Fatal(err)
	}
	if c2.Addr != "a: b\nEvil: true" {
		t.Errorf("重新判定类型后仍应是一个标量，got=%q", c2.Addr)
	}
}

func TestLoad_EmptyFileUsesAllDefaults(t *testing.T) {
	c := comps(t)
	if err := LoadInto(write(t, ""), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if *c != defaults() {
		t.Errorf("空文件应保持全部默认值，got=%+v", *c)
	}
}

func TestLoad_MissingBlockKeepsDefaults(t *testing.T) {
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if *c != defaults() {
		t.Errorf("块为空应保持默认值，got=%+v", *c)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	c := comps(t)
	if err := LoadInto(filepath.Join(t.TempDir(), "nope.yml"), "Demo", c); err == nil {
		t.Fatal("文件不存在应报错")
	}
}

func TestLoad_DurationFields(t *testing.T) {
	// 各模块的超时/周期都直接用 time.Duration，靠的是 yaml.v3 原生认识时长字符串。
	// 这是个横跨所有模块的假设，在这里钉住：改了依赖会先在这里炸，
	// 而不是等到某个模块的超时悄悄变成 0
	type demo struct {
		Timeout time.Duration `yaml:"Timeout"`
		Rotate  time.Duration `yaml:"Rotate"`
	}

	t.Run("认识时长字符串", func(t *testing.T) {
		c := demo{Rotate: 24 * time.Hour} // 预填的默认值
		if err := LoadInto(write(t, "Demo:\n  Timeout: 1h30m\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Timeout != 90*time.Minute {
			t.Errorf("Timeout 应为 90m，got=%v", c.Timeout)
		}
		if c.Rotate != 24*time.Hour {
			t.Errorf("没配的字段应保持默认值，got=%v", c.Rotate)
		}
	})

	t.Run("裸数字要报错", func(t *testing.T) {
		// 写 Timeout: 30 的人想要 30 秒，Go 的零值语义会给他 30 纳秒。
		// 启动就失败好过线上超时形同虚设
		c := demo{}
		err := LoadInto(write(t, "Demo:\n  Timeout: 30\n"), "Demo", &c)
		if err == nil {
			t.Fatal("裸数字应当报错，要求写明单位")
		}
		if !strings.Contains(err.Error(), "Demo") {
			t.Errorf("错误应指明是哪一块配置，got=%v", err)
		}
	})
}

func TestLoad_UnclaimedTopLevelKeyIsError(t *testing.T) {
	// 多半是拼错了，或者忘了 import 对应的集成包。
	// 静默忽略的话，使用者会盯着一份"明明配了"的文件查半天
	type demo struct {
		Addr string `yaml:"Addr"`
	}
	c := demo{}

	if err := LoadInto(write(t, "Demo:\n  Addr: a\nXGrom:\n  DSN: x\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	// 读是各集成包自己在启动钩子里做的，所以这件事只有等钩子全跑完才知道，
	// 由框架在那时检查 Unclaimed
	got := Unclaimed()
	if len(got) != 1 || got[0] != "XGrom" {
		t.Errorf("拼错的顶层 key 要被报出来，got=%v", got)
	}
}

func TestLoad_DefaultOverrideSemantics(t *testing.T) {
	// 默认值预填在结构体里，所以「文件里写了会发生什么」必须钉死。
	// 两种容器的行为不一样，写默认值的人必须知道
	type demo struct {
		Buckets []float64         `yaml:"Buckets"`
		Labels  map[string]string `yaml:"Labels"`
	}
	fresh := func() demo {
		return demo{
			Buckets: []float64{1, 2, 3},
			Labels:  map[string]string{"pre": "filled"},
		}
	}

	t.Run("切片是整体替换", func(t *testing.T) {
		c := fresh()
		if err := LoadInto(write(t, "Demo:\n  Buckets: [0.1, 1]\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Buckets, []float64{0.1, 1}) {
			t.Errorf("配了就该整体换掉，不能和默认值混在一起，got=%v", c.Buckets)
		}
	})

	t.Run("map 是合并而不是替换", func(t *testing.T) {
		// 所以 map 类型的字段不要预填默认值：使用者删不掉预填的条目。
		// 这条钉在这里，免得哪天有人给 ConstLabels 之类加个「合理的默认」
		c := fresh()
		if err := LoadInto(write(t, "Demo:\n  Labels:\n    a: \"1\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Labels["pre"] != "filled" {
			t.Errorf("map 的默认值不会被覆盖掉，这正是不该给 map 预填默认值的原因，got=%v", c.Labels)
		}
	})

	t.Run("没配的字段保持默认", func(t *testing.T) {
		c := fresh()
		if err := LoadInto(write(t, "Demo: {}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Buckets, []float64{1, 2, 3}) {
			t.Errorf("没配就该保持默认，got=%v", c.Buckets)
		}
	})
}

func TestLoad_DuplicateTopLevelKeyIsError(t *testing.T) {
	// 转成 map 的那一刻重复信息就没了，后面的严格解码再也看不见它——
	// 于是同一个块写两遍能正常加载，静默地以后一份为准
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Retries: 1\nOther: {}\nDemo:\n  Retries: 2\n"), "Demo", &c)
	if err == nil {
		t.Fatalf("重复的顶层 key 应当报错，实际解出 Retries=%d", c.Retries)
	}
	for _, want := range []string{"Demo", "duplicate", "line 4", "line 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里该出现 %q（要能定位到行），got=%v", want, err)
		}
	}
}

// ---- Has：「配了才初始化」的判据 ----

func TestHas_DistinguishesSetFromUnset(t *testing.T) {
	// 判错一次，可选组件要么白建一套，要么配了也不起
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XRedis:\n  Addr: 127.0.0.1:6379\n")); err != nil {
		t.Fatal(err)
	}

	if !Has("XRedis") {
		t.Error("写了的块应当报告有")
	}
	if Has("XGorm") {
		t.Error("没写的块不该报告有")
	}
}

func TestHas_EmptyBlockCountsAsUnset(t *testing.T) {
	// 「XCache:」后面什么都不写，是「我先占个位」而不是「按默认值给我建一套」
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XCache:\nXRedis:\n  Addr: 127.0.0.1:6379\n")); err != nil {
		t.Fatal(err)
	}

	if Has("XCache") {
		t.Error("空块不该让可选组件白建一套")
	}
	if !Has("XRedis") {
		t.Error("同一个文件里写实了的块要照常报告有")
	}
}

func TestHas_LoadsFirstIfNotLoaded(t *testing.T) {
	// 可选组件靠 Has 决定建不建。还没加载就报告没有的话，一个在 Run 之前
	// 问了一句的组件会以为自己没配，从此静默缺席
	Reset()
	t.Cleanup(Reset)
	t.Setenv(EnvKey, write(t, "XRedis:\n  Addr: x\n"))

	if !Has("XRedis") {
		t.Error("第一次问的时候该先加载，而不是报告没有")
	}
}

// ---- Unclaimed 的其余语义 ----

func TestUnclaimed_SortedByName(t *testing.T) {
	// 顺序稳定，错误文案才可复现
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\nXTypo:\n  Foo: 1\nXAlso:\n  Bar: 2\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 2 || got[0] != "XAlso" || got[1] != "XTypo" {
		t.Errorf("want [XAlso XTypo]，got=%v", got)
	}
}

func TestUnclaimed_ReadingAbsentBlockCountsAsClaim(t *testing.T) {
	// 可选组件都是「先读了再看有没有」，那种读不该把别的块连累成没人认领
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal("XRedis", &c); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("want 空，got=%v", got)
	}
}

func TestUnclaimed_EmptyBeforeLoad(t *testing.T) {
	Reset()
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("want 空，got=%v", got)
	}
}

func TestLoad_EachLoadResetsClaims(t *testing.T) {
	// 同一个进程里跑第二次时，上一轮的认领记录不能留下来——
	// 那会让这一轮真正没人读的块被漏报
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}

	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "Demo:\n  Addr: y\n")); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 1 || got[0] != "Demo" {
		t.Errorf("上一轮的认领记录不该留到这一轮，got=%v", got)
	}
}

func TestUnmarshal_EmptyBlockKeepsDefaults(t *testing.T) {
	c := comps(t)
	want := c.Addr
	if err := LoadInto(write(t, "Demo:\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != want {
		t.Errorf("空块不该把默认值抹掉，got=%q", c.Addr)
	}
}

func TestHas_AskingCountsAsClaim(t *testing.T) {
	// 「XRedis:」这样的空块曾经让启动直接失败：Has 返回 false，那个包就此
	// 跳过、不再 Unmarshal，于是这个 key 没人认领。而 Unclaimed 报出来的
	// 两条原因（拼错了、忘了 import）都不成立，照着查什么都查不出来
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XRedis:\n")); err != nil {
		t.Fatal(err)
	}

	if Has("XRedis") {
		t.Fatal("空块应当报告没配")
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("问过就算有人要，不该再被报成没人认领，got=%v", got)
	}
}

func TestHas_AskingNonEmptyBlockCountsAsClaim(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XRedis:\n  Addr: a\n")); err != nil {
		t.Fatal(err)
	}

	if !Has("XRedis") {
		t.Fatal("写实了的块应当报告有")
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("want 空，got=%v", got)
	}
}

func TestLoad_PlaceholderExpandingToEmptyKeepsDefault(t *testing.T) {
	// 展开成空等于这一项没写：非字符串字段从前以
	// cannot unmarshal !!str "" into int 失败，字符串字段则被清成空串
	os.Unsetenv("XONE_T_EMPTY")
	t.Setenv("XONE_T_BLANK", "")
	for name, v := range map[string]string{
		"默认值为空":  "${XONE_T_EMPTY:}",
		"环境变量为空": "${XONE_T_BLANK}",
	} {
		t.Run(name, func(t *testing.T) {
			body := "Demo:\n  Addr: " + v + "\n  Timeout: " + v + "\n  Retries: " + v + "\n  Enable: " + v + "\n"
			c := comps(t)
			if err := LoadInto(write(t, body), "Demo", c); err != nil {
				t.Fatal(err)
			}
			if *c != defaults() {
				t.Errorf("展开为空的字段该保持默认值，got=%+v", *c)
			}
		})
	}
}

func TestLoad_QuotedEmptyPlaceholderIsEmptyString(t *testing.T) {
	// 引号是明确的「按字符串处理」，真想要空串就这样写
	os.Unsetenv("XONE_T_EMPTY")
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_EMPTY:}\"\n"), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_AnchorsWorkAcrossTopLevelBlocks(t *testing.T) {
	// 各块是分开解码的，从前别名指向别的块里的锚点时报 unknown anchor
	body := "Shared: &timeouts\n  Timeout: 7s\n  Retries: 9\n" +
		"Demo:\n  <<: *timeouts\n  Addr: h:1\n" +
		"Other:\n  Addr: &addr o:1\n" +
		"Third:\n  Addr: *addr\n"
	c := comps(t)
	if err := LoadInto(write(t, body), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 7*time.Second || c.Retries != 9 || c.Addr != "h:1" {
		t.Errorf("got=%+v", *c)
	}
	var third demoConf
	if err := Unmarshal("Third", &third); err != nil {
		t.Fatal(err)
	}
	if third.Addr != "o:1" {
		t.Errorf("标量别名该解出锚点的值，got=%q", third.Addr)
	}
}

func TestLoad_AnchorPlaceholdersExpandAtEveryAlias(t *testing.T) {
	t.Setenv("XONE_T_ANCHOR", "env:1")
	body := "Base: &b\n  Addr: ${XONE_T_ANCHOR}\nDemo: *b\n"
	c := comps(t)
	if err := LoadInto(write(t, body), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "env:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ProfileFileCanOverrideAliasExpandedFields(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Base: &b\n  Addr: base:1\n  Retries: 5\nDemo: *b\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})
	c := comps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" || c.Retries != 5 {
		t.Errorf("got=%+v", *c)
	}
}

func TestLoad_TooManyAliasExpansionsIsError(t *testing.T) {
	// 别名在加载时就地展开成副本，一份刻意构造的「十亿笑」会把内存吃光
	var b strings.Builder
	b.WriteString("L0: &l0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "L%d: &l%d [", i, i)
		for j := 0; j < 10; j++ {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*l%d", i-1)
		}
		b.WriteString("]\n")
	}
	err := LoadInto(write(t, b.String()), "Demo", comps(t))
	if err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("展开后的规模该有上限，got=%v", err)
	}
}

func TestDecodeStrict_MissingTagErrorExplainsFix(t *testing.T) {
	// yaml.v3 对没写 tag 的字段只认全小写：照着字段名写 Endpoint，报的是
	// 「field Endpoint not found」——字段明明就叫这个。嵌套的一样要提示到
	type inner struct{ Host string }
	var c struct {
		Endpoint string
		Items    map[string]inner `yaml:"Items"`
	}
	for _, src := range []string{"Endpoint: a", "Items: {a: {Host: h}}"} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(src), &n); err != nil {
			t.Fatal(err)
		}
		err := DecodeStrict(n.Content[0], &c)
		if err == nil {
			t.Fatalf("%s: 没写 tag 的字段照着字段名写，yaml.v3 本来就不认", src)
		}
		var te *yaml.TypeError
		if !errors.As(err, &te) {
			t.Errorf("%s: 原始的 yaml 错误要用 %%w 保住，got=%v", src, err)
		}
		if !strings.Contains(err.Error(), "has no yaml tag") || !strings.Contains(err.Error(), "`yaml:") {
			t.Errorf("%s: 报错里该说字段没写 tag、该怎么写，got=%v", src, err)
		}
	}
}

func TestDecodeStrict_KeyOneLevelTooHighPointsToItsBlock(t *testing.T) {
	// 字段从平铺挪进子块之后（XGin 的 CertFile 挪进 TLS:），照旧写法写的使用者
	// 该看到往哪挪，而不只是「没有这个字段」。两个子块都有同名字段时不猜
	type tlsBlock struct {
		CertFile string `yaml:"CertFile"`
		CAFile   string `yaml:"CAFile"`
	}
	type other struct {
		CAFile string `yaml:"CAFile"`
	}
	var c struct {
		Port  int      `yaml:"Port"`
		TLS   tlsBlock `yaml:"TLS"`
		Proxy *other   `yaml:"Proxy"`
	}
	for src, want := range map[string]string{
		"CertFile: a": "field CertFile not found in type struct", // 前缀照旧
		"Nope: a":     "field Nope not found in type struct",
		"CAFile: a":   "field CAFile not found in type struct",
	} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(src), &n); err != nil {
			t.Fatal(err)
		}
		err := DecodeStrict(n.Content[0], &c)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: 认不出的字段该报出来，got=%v", src, err)
		}
		hinted := strings.Contains(err.Error(), "did you mean")
		switch src {
		case "CertFile: a":
			if !strings.Contains(err.Error(), "(did you mean TLS.CertFile? move it under TLS:)") {
				t.Errorf("%s: 该指出挪到 TLS: 下，got=%v", src, err)
			}
		default:
			if hinted {
				t.Errorf("%s: 子块里没有或不止一个子块有，不该猜，got=%v", src, err)
			}
		}
	}
}

func TestLoad_PlaceholderNullLiteralIsNotTreatedAsUnset(t *testing.T) {
	// 变量的值恰好是 null / ~ 时，重新判定会把它当成「这一项没写」：
	// 字段悄悄留在默认值上，启动一切正常，配的值却没生效
	for _, v := range []string{"null", "~", "Null", "NULL"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("XONE_T_NULLISH", v)

			c := comps(t)
			if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_NULLISH}\n"), "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != v {
				t.Errorf("字符串字段该拿到字面量 %q，got=%q", v, c.Addr)
			}

			c = comps(t)
			err := LoadInto(write(t, "Demo:\n  Retries: ${XONE_T_NULLISH}\n"), "Demo", c)
			var te *yaml.TypeError
			if !errors.As(err, &te) {
				t.Errorf("非字符串字段该报类型错误，而不是悄悄保持默认值，got err=%v Retries=%d", err, c.Retries)
			}
		})
	}
}

// secretElem 自己写 UnmarshalYAML 的集合元素：走的是嵌套的那次 DecodeStrict
type secretElem struct {
	N int `yaml:"N"`
}

func (e *secretElem) UnmarshalYAML(n *yaml.Node) error {
	type raw secretElem
	return DecodeStrict(n, (*raw)(e))
}

func TestUnmarshal_ExpandedPlaceholderValueNotInError(t *testing.T) {
	// yaml 的类型错误会带上值的前几个字符。${VAR} 是凭证的推荐写法，
	// 密码填错了字段，报出来的就是 cannot unmarshal !!str `hunter2...` into int
	t.Setenv("XONE_T_SECRET", "hunter2-very-secret")
	t.Setenv("XONE_T_SHORT", "s3cr3t")
	type conf struct {
		Retries int                   `yaml:"Retries"`
		Enable  bool                  `yaml:"Enable"`
		Items   map[string]secretElem `yaml:"Items"`
	}
	for name, body := range map[string]string{
		"长值":     "Demo:\n  Retries: ${XONE_T_SECRET}\n",
		"短值":     "Demo:\n  Enable: ${XONE_T_SHORT}\n",
		"拼在字面量里": "Demo:\n  Retries: pre-${XONE_T_SHORT}\n",
		"嵌套的元素":  "Demo:\n  Items:\n    a:\n      N: ${XONE_T_SECRET}\n",
	} {
		t.Run(name, func(t *testing.T) {
			var c conf
			err := LoadInto(write(t, body), "Demo", &c)
			if err == nil {
				t.Fatal("类型不对该报错")
			}
			for _, leak := range []string{"hunter2", "s3cr3t"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("报错里不该有展开出来的值 %q，got=%v", leak, err)
				}
			}
			if !strings.Contains(err.Error(), "${XONE_T_") {
				t.Errorf("该换成配置里写的占位符，好看出是哪一项，got=%v", err)
			}
			var te *yaml.TypeError
			if !errors.As(err, &te) {
				t.Errorf("errors.As 该照样取得到 yaml 的类型错误，got=%v", err)
			}
		})
	}
}

func TestDecodeStrict_NoTagHintOnRealTypo(t *testing.T) {
	var c struct {
		Endpoint string `yaml:"Endpoint"`
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("Endpont: a"), &n); err != nil {
		t.Fatal(err)
	}
	err := DecodeStrict(n.Content[0], &c)
	if err == nil || strings.Contains(err.Error(), "yaml tag") {
		t.Errorf("拼错的字段该原样报拼错，不该提示 tag，got=%v", err)
	}
}

func TestLoad_BadFieldReportsFileAndLine(t *testing.T) {
	// 严格解码从前是把节点序列化成文本再解的，yaml 报的行号是那段文本里的行号：
	// 第 6 行的拼错报成 line 2，也不说是哪个文件，profile 文件里的更是无从找起
	type conf struct {
		Addr    string                `yaml:"Addr"`
		Retries int                   `yaml:"Retries"`
		Items   map[string]secretElem `yaml:"Items"`
	}
	for _, c := range []struct {
		name    string
		profile string            // 激活的 profile，空则只有 base
		files   map[string]string // application.yml 必有
		file    string            // 报错该点名的文件
		want    string            // 报错里该有的「:行号: 原因」
	}{
		{"base 文件里拼错", "", map[string]string{
			"application.yml": "# 注释\nOther:\n  X: 1\nDemo:\n  Addr: a\n  Adrr: b\n",
		}, "application.yml", ":6: field Adrr not found"},
		{"类型不对", "", map[string]string{
			"application.yml": "Demo:\n\n  Retries: abc\n",
		}, "application.yml", ":3: cannot unmarshal"},
		{"profile 文件里拼错", "prod", map[string]string{
			"application.yml":      "Demo:\n  Addr: a\n  Retries: 1\n",
			"application-prod.yml": "\n\nDemo:\n  Retries: 2\n  Retires: 3\n",
		}, "application-prod.yml", ":5: field Retires not found"},
		{"profile 文件里类型不对", "prod", map[string]string{
			"application.yml":      "Demo:\n  Addr: a\n",
			"application-prod.yml": "Demo:\n  Retries: x\n",
		}, "application-prod.yml", ":2: cannot unmarshal"},
		{"两个文件合并出来的块本身类型不对", "prod", map[string]string{
			"application.yml":      "Demo:\n\n  Addr:\n    a: 1\n",
			"application-prod.yml": "Demo:\n  Addr:\n    b: 2\n",
		}, "application.yml", ":4: cannot unmarshal !!map"},
		{"import 进来的文件里拼错", "", map[string]string{
			"application.yml": "XApp:\n  Import: part.yml\nDemo:\n  Addr: a\n",
			"part.yml":        "Demo:\n  Adrr: b\n",
		}, "part.yml", ":2: field Adrr not found"},
		{"嵌套在 UnmarshalYAML 里的那一次", "", map[string]string{
			"application.yml": "Demo:\n  Items:\n    a:\n      N: 1\n    b:\n      M: 2\n",
		}, "application.yml", ":6: field M not found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.profile != "" {
				withProfileEnv(t, c.profile)
			}
			base := files(t, "application.yml", c.files)
			var got conf
			err := LoadInto(base, "Demo", &got)
			if err == nil {
				t.Fatal("写错了应当报错")
			}
			want := filepath.Join(filepath.Dir(base), c.file) + c.want
			if !strings.Contains(err.Error(), want) {
				t.Errorf("报错该点名文件和行号 %q，got=%v", want, err)
			}
			var te *yaml.TypeError
			if !errors.As(err, &te) {
				t.Errorf("errors.As 该照样取得到 yaml 的类型错误，got=%v", err)
			}
		})
	}
}

func TestDecodeStrict_NonFileNodeReportsItsOwnLine(t *testing.T) {
	// 没有文件可说时，行号也得是调用方手里那个节点的行号
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("a: 1\nDemo:\n  Addr: x\n\n  Adrr: y\n"), &n); err != nil {
		t.Fatal(err)
	}
	var c demoConf
	err := DecodeStrict(n.Content[0].Content[3], &c)
	if err == nil || !strings.Contains(err.Error(), "line 5: field Adrr not found") {
		t.Errorf("该报原节点的 line 5，got=%v", err)
	}
}

func TestLoad_CollectionElementErrorsReportedWithOthers(t *testing.T) {
	// 自己写 UnmarshalYAML 的元素在检查那一遍里就试解，而不是等到最后才解：
	// 否则外面有一处写错，元素里的就要等改完、重启一次才看得见
	type conf struct {
		Addr  string                `yaml:"Addr"`
		Items map[string]secretElem `yaml:"Items"`
	}
	var got conf
	err := LoadInto(write(t, "Demo:\n  Adrr: a\n  Items:\n    a:\n      M: 1\n"), "Demo", &got)
	for _, want := range []string{":2: field Adrr not found", ":5: field M not found"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("两处都该报出来，缺 %q，got=%v", want, err)
		}
	}
}

func TestDecodeStrict_FieldRulesMatchYAML(t *testing.T) {
	// 未知字段是自己按类型查的，认字段的规则必须和 yaml.v3 一模一样：
	// 多认一个是拼错放行，少认一个是合法的配置起不来
	type base struct {
		Host string `yaml:"Host"`
	}
	type conf struct {
		base  `yaml:",inline"`
		Ptr   *base            `yaml:"Ptr"`
		List  []base           `yaml:"List"`
		Skip  string           `yaml:"-"`
		Any   any              `yaml:"Any"`
		Extra map[string]int   `yaml:",inline"`
		Named map[string]*base `yaml:"Named"`
	}
	ok := "Host: h\nPtr: {Host: p}\nList: [{Host: l}]\nAny: {whatever: 1}\nNamed: {a: {Host: n}}\nother: 7\n"
	bad := map[string]string{
		"内嵌结构体摊平后的拼错":       "Host: h\nHots: x\n",
		"指针里的拼错":            "Ptr: {Hots: p}\n",
		"切片元素里的拼错":          "List: [{Hots: l}]\n",
		"map 值里的拼错":         "Named: {a: {Hots: n}}\n",
		"写了 - 的字段":          "Skip: x\n",
		"inline map 的值类型不对": "other: abc\n",
	}
	decode := func(src string) (conf, error) {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(src), &n); err != nil {
			t.Fatal(err)
		}
		var c conf
		return c, DecodeStrict(n.Content[0], &c)
	}
	c, err := decode(ok)
	if err != nil {
		t.Fatalf("合法的配置不该报错：%v", err)
	}
	if c.Host != "h" || c.Ptr.Host != "p" || c.List[0].Host != "l" || c.Named["a"].Host != "n" || c.Extra["other"] != 7 {
		t.Errorf("got=%+v", c)
	}
	for name, src := range bad {
		if _, err := decode(src); err == nil {
			t.Errorf("%s：应当报错", name)
		}
	}
}

func TestLoad_MultipleDocumentsIsError(t *testing.T) {
	// 从前只读第一份文档，--- 之后的整段静默丢掉，连 Unclaimed 都看不见它
	for _, body := range []string{
		"Demo:\n  Addr: a:1\n---\nDemo:\n  Addr: b:1\n",
		"Demo:\n  Addr: a:1\n---\nOther: 1\n---\n",
		"---\n---\nDemo:\n  Addr: a:1\n---\nDemo:\n  Addr: b:1\n",
	} {
		err := LoadInto(write(t, body), "Demo", comps(t))
		if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") ||
			!strings.Contains(err.Error(), "application-{profile}.yml") {
			t.Errorf("%q: 多份文档该启动失败，got=%v", body, err)
		}
	}
}

func TestLoad_LeadingOrTrailingDocumentMarkerStillLoads(t *testing.T) {
	// 空文档（--- 后面什么都没写、只有注释、~）不算一份：开头一个 ---、结尾多一个 --- 照常加载
	for _, body := range []string{
		"---\n---\nDemo:\n  Addr: a:1\n",
		"---\nDemo:\n  Addr: a:1\n",
		"# head\n---\nDemo:\n  Addr: a:1\n",
		"Demo:\n  Addr: a:1\n---\n",
		"Demo:\n  Addr: a:1\n---\n# nothing here\n",
		"Demo:\n  Addr: a:1\n--- ~\n",
		"Demo:\n  Addr: a:1\n...\n",
		"---\nDemo:\n  Addr: a:1\n---\n---\n",
	} {
		c := comps(t)
		if err := LoadInto(write(t, body), "Demo", c); err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if c.Addr != "a:1" {
			t.Errorf("%q: Addr=%q", body, c.Addr)
		}
	}
}

func TestLoad_MissingVariablesListedOnceSorted(t *testing.T) {
	// 同一个变量用了好几处时从前报成 ZZMISS_B, ZZMISS_A, ZZMISS_B
	body := "Demo:\n  Addr: ${ZZMISS_B}\n  Retries: ${ZZMISS_A}\nOther:\n  A: ${ZZMISS_B}\n  B: ${ZZMISS_B}-${ZZMISS_A}\n"
	err := LoadInto(write(t, body), "Demo", comps(t))
	if err == nil || !strings.HasSuffix(err.Error(), "environment variables not set: ZZMISS_A, ZZMISS_B]") {
		t.Errorf("没设的变量该按名字排好、每个只报一次，got=%v", err)
	}
}
