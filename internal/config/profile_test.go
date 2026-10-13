package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// listConf 带一个列表字段，用来验证「列表整体替换、不逐元素合并」
type listConf struct {
	Addr    string        `yaml:"Addr"`
	Timeout time.Duration `yaml:"Timeout"`
	Headers []string      `yaml:"Headers"`
	Nested  struct {
		A string `yaml:"A"`
		B string `yaml:"B"`
	} `yaml:"Nested"`
}

func listComps(t *testing.T) *listConf {
	t.Helper()
	c := listConf{Addr: "default:1", Timeout: time.Second}
	return &c
}

// files 在同一个临时目录里写好一组文件，返回其中 base 的路径
func files(t *testing.T, base string, m map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range m {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, base)
}

// withProfileEnv 设置 XONE_PROFILE，退出时还原
func withProfileEnv(t *testing.T, v string) {
	t.Helper()
	t.Setenv(ProfileEnvKey, v)
}

func TestLoad_ProfileFileOverridesBase(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n  Timeout: 1s\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("profile 文件该压过 base，Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("profile 没写的字段该保留 base 的值，Timeout=%v", c.Timeout)
	}
}

func TestLoad_LaterProfileOverridesEarlier(t *testing.T) {
	withProfileEnv(t, "a, b")
	base := files(t, "application.yml", map[string]string{
		"application.yml":   "Demo:\n  Addr: base:1\n",
		"application-a.yml": "Demo:\n  Addr: a:1\n  Timeout: 2s\n",
		"application-b.yml": "Demo:\n  Addr: b:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b:1" {
		t.Errorf("靠后的 profile 该压过靠前的，Addr=%q", c.Addr)
	}
	if c.Timeout != 2*time.Second {
		t.Errorf("a 写了而 b 没写的字段该保留 a 的，Timeout=%v", c.Timeout)
	}
}

func TestLoad_ListsReplacedWholeNotMerged(t *testing.T) {
	// 逐元素合并的话 [A,B] 叠上 [C] 会变成 [C,B]——使用者以为换掉了整张表，
	// 实际只换掉第一项，剩下那项来自另一个文件。Spring 也是整体替换
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Headers: [A, B]\n",
		"application-prod.yml": "Demo:\n  Headers: [C]\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Headers, []string{"C"}) {
		t.Errorf("列表该被整体替换，got=%v want=[C]", c.Headers)
	}
}

func TestLoad_MapMergedRecursively(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Nested:\n    A: base-a\n    B: base-b\n",
		"application-prod.yml": "Demo:\n  Nested:\n    A: prod-a\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Nested.A != "prod-a" || c.Nested.B != "base-b" {
		t.Errorf("map 该递归合并，A=%q B=%q", c.Nested.A, c.Nested.B)
	}
}

func TestLoad_ProfileFileMissingFailsFast(t *testing.T) {
	// 与 Spring 不同：那边静默跳过。点名要了某个 profile 文件却不在，
	// 几乎总是名字写错了，静默跳过的结果是一份谁都没看过的配置以默认值起来
	withProfileEnv(t, "typo")
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Demo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	err := LoadInto(base, "Demo", c)
	if err == nil {
		t.Fatal("profile 文件不存在该报错")
	}
	if !strings.Contains(err.Error(), "application-typo.yml") {
		t.Errorf("错误里该指出是哪个文件，got=%v", err)
	}
}

func TestLoad_ImportOverridesImporter(t *testing.T) {
	// 与 Spring 一致：import 相当于插在声明它的那份文档正下方，下面的压过上面的
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n  Timeout: 1s\n",
		"shared.yml":      "Demo:\n  Addr: shared:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "shared:1" {
		t.Errorf("import 进来的该压过引它的文件，Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("import 没写的字段该保留，Timeout=%v", c.Timeout)
	}
}

func TestLoad_ProfileOverridesBaseImport(t *testing.T) {
	// 顺序：base < base 的 import < profile 文件 < profile 的 import
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":           "Demo:\n  Addr: shared:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("profile 文件该压过 base 引进来的，Addr=%q", c.Addr)
	}
}

func TestLoad_ImportMultipleAppliedInOrder(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import:\n    - a.yml\n    - b.yml\nDemo:\n  Addr: base:1\n",
		"a.yml":           "Demo:\n  Addr: a:1\n  Timeout: 3s\n",
		"b.yml":           "Demo:\n  Addr: b:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b:1" {
		t.Errorf("靠后的 import 该压过靠前的，Addr=%q", c.Addr)
	}
	if c.Timeout != 3*time.Second {
		t.Errorf("a 写了而 b 没写的字段该保留 a 的，Timeout=%v", c.Timeout)
	}
}

func TestLoad_ImportRelativePathResolvedAgainstImporter(t *testing.T) {
	// 按进程工作目录解析的话，配置目录整个搬个位置里面的引用就失效了
	base := files(t, "conf/application.yml", map[string]string{
		"conf/application.yml": "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"conf/parts/db.yml":    "Demo:\n  Addr: db:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "db:1" {
		t.Errorf("相对路径该按引它的文件所在目录解析，Addr=%q", c.Addr)
	}
}

func TestLoad_ImportMissingFileIsError(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: missing.yml\nDemo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err == nil {
		t.Fatal("import 的文件不存在该报错")
	}
}

func TestLoad_OptionalImportMayBeMissing(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: optional:local.yml\nDemo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("optional: 的文件不存在不该报错：%v", err)
	}
	if c.Addr != "base:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ImportCycleTerminates(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: a.yml\nDemo:\n  Addr: base:1\n",
		"a.yml":           "XApp:\n  Import: application.yml\nDemo:\n  Addr: a:1\n",
	})

	c := listComps(t)
	// 同一个文件只算一次，所以环会自己断掉而不是无限递归
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("成环该被同一文件只读一次挡住：%v", err)
	}
	if c.Addr != "a:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ProfilesOnlyAllowedInBase(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":      "XApp:\n  Profiles: [prod]\nDemo:\n  Addr: shared:1\n",
	})

	c := listComps(t)
	err := LoadInto(base, "Demo", c)
	if err == nil {
		t.Fatal("被引进来的文件再去激活 profile 该报错")
	}
	if !strings.Contains(err.Error(), ProfilesKey) {
		t.Errorf("错误里该提到是哪个 key，got=%v", err)
	}
}

func TestLoad_BaseProfilesTakeEffect(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Profiles: [prod]\nDemo:\n  Addr: base:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("配置文件里声明的 profile 该生效，Addr=%q", c.Addr)
	}
}

func TestLoad_EnvVarOverridesProfilesInFile(t *testing.T) {
	withProfileEnv(t, "dev")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Profiles: [prod]\nDemo:\n  Addr: base:1\n",
		"application-dev.yml":  "Demo:\n  Addr: dev:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "dev:1" {
		t.Errorf("环境变量该压过文件里声明的 profile，Addr=%q", c.Addr)
	}
}

func TestLoad_ImportAndProfilesAreNotUnclaimed(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Profiles: []\n  Import: optional:x.yml\nDemo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("这两个是保留 key，不该被当成没人认领：%v", err)
	}
}

func TestLoad_RequiredPlaceholderOverriddenByProfileNotRequired(t *testing.T) {
	// 占位符在全部合并完之后才展开。逐个文件展开的话，
	// base 里那个 ${SECRET} 即便已经被 prod 换掉了，也还是会要求必须设置
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: \"${SECRET}\"\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("已经被覆盖掉的占位符不该再要求设置：%v", err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ImportPathPlaceholdersAreExpanded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.yml"), []byte("Demo:\n  Addr: shared:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(dir, "application.yml")
	if err := os.WriteFile(basePath, []byte("XApp:\n  Import: \"${CONF_DIR}/shared.yml\"\nDemo:\n  Addr: base:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONF_DIR", dir)

	c := listComps(t)
	if err := LoadInto(basePath, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "shared:1" {
		t.Errorf("import 路径里的占位符该展开，Addr=%q", c.Addr)
	}
}

func TestLoad_ImportFilesAlsoGetProfileVariants(t *testing.T) {
	// 配置拆成片段之后，最该按环境变的恰恰是片段里的内容（连接串之类），
	// 只有主文件有变体的话，拆分就变得很别扭
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"parts/db.yml":         "Demo:\n  Addr: db:1\n  Timeout: 1s\n",
		"parts/db-prod.yml":    "Demo:\n  Addr: db-prod:1\n",
		"application-prod.yml": "Demo:\n  Headers: [x]\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "db-prod:1" {
		t.Errorf("片段的 profile 变体该生效，Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("变体没写的字段该保留片段里的值，Timeout=%v", c.Timeout)
	}
}

func TestLoad_FragmentProfileVariantMayBeMissing(t *testing.T) {
	// 与主文件不同：拆成十个片段之后，要求每个片段都备齐每个环境的变体没法用。
	// profile 名写错这件事已经由主文件的变体挡住了
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"parts/db.yml":         "Demo:\n  Addr: db:1\n",
		"application-prod.yml": "Demo:\n  Timeout: 9s\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("片段没有 prod 变体不该报错：%v", err)
	}
	if c.Addr != "db:1" || c.Timeout != 9*time.Second {
		t.Errorf("Addr=%q Timeout=%v", c.Addr, c.Timeout)
	}
}

func TestLoad_MainProfileVariantOverridesFragments(t *testing.T) {
	// 顺序：base、base 的 import（含其变体）、application-prod、它的 import
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"parts/db.yml":         "Demo:\n  Addr: db:1\n",
		"parts/db-prod.yml":    "Demo:\n  Addr: db-prod:1\n",
		"application-prod.yml": "Demo:\n  Addr: app-prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "app-prod:1" {
		t.Errorf("主文件的 profile 变体该压过片段的，Addr=%q", c.Addr)
	}
}

func TestLoad_ProfilesActiveAcceptsBothForms(t *testing.T) {
	// 注释和文档都说逗号分隔的字符串也行，从前那种写法以
	// cannot unmarshal !!str into []string 失败
	for name, active := range map[string]string{
		"列表":      "[a, b]",
		"字符串":     "\"a,b\"",
		"不加引号":    "a, b",
		"列表元素带逗号": "[\"a,b\"]",
	} {
		t.Run(name, func(t *testing.T) {
			base := files(t, "application.yml", map[string]string{
				"application.yml":   "XApp:\n  Profiles: " + active + "\nDemo:\n  Addr: base:1\n",
				"application-a.yml": "Demo:\n  Addr: a:1\n  Timeout: 2s\n",
				"application-b.yml": "Demo:\n  Addr: b:1\n",
			})
			c := listComps(t)
			if err := LoadInto(base, "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != "b:1" || c.Timeout != 2*time.Second {
				t.Errorf("a、b 两个 profile 都该生效且 b 压过 a，got Addr=%q Timeout=%v", c.Addr, c.Timeout)
			}
		})
	}
}

func TestLoad_ProfilesPlaceholdersAreExpanded(t *testing.T) {
	// 要先知道激活哪些 profile 才知道读哪些文件，所以这一块和 Import 一样，
	// 在读它的时候就展开，而不是等到全部合并完
	t.Setenv("XONE_T_ENV", "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Profiles: ${XONE_T_ENV:dev}\nDemo:\n  Addr: base:1\n",
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

func TestLoad_ProfilesUnsetPlaceholderIsError(t *testing.T) {
	os.Unsetenv("XONE_T_ENV_MISSING")
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Profiles: ${XONE_T_ENV_MISSING}\n",
	})
	err := LoadInto(base, "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "XONE_T_ENV_MISSING") {
		t.Fatalf("未设置的变量该点名报错，got=%v", err)
	}
}

func TestLoad_EmptyProfilesInImportedFileIsError(t *testing.T) {
	// 从前只在它真的声明了 profile 时才报，Active: [] 能混过去
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\n",
		"shared.yml":      "XApp:\n  Profiles: []\n",
	})
	if err := LoadInto(base, "Demo", listComps(t)); err == nil {
		t.Fatal("被引进来的文件里出现 Profiles 就该报错")
	}
}

func TestLoad_ImportNestedTooDeepIsError(t *testing.T) {
	m := map[string]string{}
	for i := 0; i <= maxImportDepth+1; i++ {
		m[fmt.Sprintf("f%d.yml", i)] = fmt.Sprintf("XApp:\n  Import: f%d.yml\n", i+1)
	}
	m[fmt.Sprintf("f%d.yml", maxImportDepth+2)] = "Demo:\n  Addr: deep:1\n"
	err := LoadInto(files(t, "f0.yml", m), "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "levels deep") {
		t.Fatalf("超过 %d 层的 import 该报错，got=%v", maxImportDepth, err)
	}
	// 成环不会走到这里（同一个文件只读一次），所以错误里不该让人去查环
	if strings.Contains(err.Error(), "cycle") {
		t.Errorf("嵌套过深和成环无关，错误里不该提环：%v", err)
	}
}

func TestLoad_XAppWithOnlyProfilesAndImportIsNotUnclaimed(t *testing.T) {
	// 没 import xapp 的程序：XApp 里的两项被加载器取走之后是个空块，不该被当成拼错的 key
	t.Cleanup(Reset)
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Profiles: []\n  Import: optional:x.yml\nDemo:\n  Addr: base:1\n",
	})
	if err := Load(base); err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal("Demo", listComps(t)); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("只剩 Profiles / Import 的 XApp 不该算没人读，got=%v", got)
	}
}

func TestLoad_XAppNameLeftToXapp_ProfilesAndImportTakenByLoader(t *testing.T) {
	// xapp 严格解码 XApp：Profiles、Import 要是还在，它会报不认识的字段
	t.Cleanup(Reset)
	withProfileEnv(t, "")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Name: demo\n  Profiles: prod\n  Import: part.yml\n",
		"part.yml":             "Demo:\n  Addr: part:1\n",
		"application-prod.yml": "XApp:\n  Version: v1\n",
	})
	if err := Load(base); err != nil {
		t.Fatal(err)
	}
	var app struct {
		Name    string `yaml:"Name"`
		Version string `yaml:"Version"`
	}
	if err := Unmarshal(AppKey, &app); err != nil {
		t.Fatalf("XApp 里只该剩 Name / Version：%v", err)
	}
	if app.Name != "demo" || app.Version != "v1" {
		t.Errorf("got=%+v，want Name=demo（base）Version=v1（prod 文件）", app)
	}
	c := listComps(t)
	if err := Unmarshal("Demo", c); err != nil || c.Addr != "part:1" {
		t.Errorf("XApp.Import 该生效，Addr=%q err=%v", c.Addr, err)
	}
}

// ---- YAML 合并键（<<）跨文件 ----
//
// yaml 的规矩是「写明的 key 压过 << 并进来的」。这条规矩要在每个文件内部用：
// 从前是在几个文件合并完之后才由解码器套用，于是 profile 文件用 << 写的值
// 输给了 base 里写明的同名 key——优先级高的文件反而没生效

func TestLoad_ProfileMergeKeyBeatsBaseExplicitKeys(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n  Timeout: 1s\n",
		"application-prod.yml": "Shared: &s\n  Addr: prod:1\nDemo:\n  <<: *s\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" || c.Timeout != time.Second {
		t.Errorf("profile 里 << 并进来的值该压过 base，got Addr=%q Timeout=%v", c.Addr, c.Timeout)
	}
}

func TestLoad_ImportedMergeKeyBeatsImporterExplicitKeys(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: extra.yml\nDemo:\n  Addr: base:1\n",
		"extra.yml":       "Demo:\n  <<: {Addr: extra:1}\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "extra:1" {
		t.Errorf("import 进来的文件里 << 并进来的值该压过引它的文件，Addr=%q", c.Addr)
	}
}

func TestLoad_ProfileExplicitKeyBeatsBaseMergeKey(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Shared: &s\n  Addr: base:1\n  Timeout: 2s\nDemo:\n  <<: *s\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" || c.Timeout != 2*time.Second {
		t.Errorf("got Addr=%q Timeout=%v", c.Addr, c.Timeout)
	}
}

func TestLoad_ProfileNestedMergeKeyBeatsBase(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Nested:\n    A: base-a\n    B: base-b\n",
		"application-prod.yml": "Demo:\n  Nested:\n    <<: {A: prod-a}\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Nested.A != "prod-a" || c.Nested.B != "base-b" {
		t.Errorf("got %+v", c.Nested)
	}
}

func TestLoad_MergeKeyRulesWithinOneFile(t *testing.T) {
	// 同一个文件里仍是 yaml 的规矩：写明的 key 压过并进来的，不论写在 << 之前还是之后；
	// << 后面是列表时，靠前的压过靠后的；并进来的 mapping 自己也能带 <<
	body := "A: &a {Addr: a:1, Timeout: 1s}\n" +
		"B: &b {Addr: b:1, Timeout: 2s, Headers: [b]}\n" +
		"W: &w {<<: {Nested: {A: deep}}, Addr: w:1}\n" +
		"Demo:\n  Addr: own:1\n  <<: [*a, *b]\n" +
		"Deep:\n  <<: *w\n"
	c := listComps(t)
	if err := LoadInto(write(t, body), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "own:1" || c.Timeout != time.Second || !reflect.DeepEqual(c.Headers, []string{"b"}) {
		t.Errorf("got Addr=%q Timeout=%v Headers=%v", c.Addr, c.Timeout, c.Headers)
	}
	d := listComps(t)
	if err := Unmarshal("Deep", d); err != nil {
		t.Fatal(err)
	}
	if d.Addr != "w:1" || d.Nested.A != "deep" {
		t.Errorf("got Addr=%q Nested=%+v", d.Addr, d.Nested)
	}
}

func TestLoad_MergeKeyWithScalarValueIsError(t *testing.T) {
	err := LoadInto(write(t, "Demo:\n  <<: oops\n"), "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "<<") {
		t.Errorf("<< 后面不是 mapping 该报错，got=%v", err)
	}
}

func TestLoad_TypoInMergedFieldsIsError(t *testing.T) {
	// << 摊平之后并进来的字段就是普通的 key，拼错照样启动失败
	err := LoadInto(write(t, "Shared: &s\n  Adr: x:1\nDemo:\n  <<: *s\n"), "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "field Adr not found") {
		t.Errorf("并进来的字段拼错该报错，got=%v", err)
	}
}

func TestLoad_MultipleDocumentsInProfileFileIsError(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n---\nDemo:\n  Addr: other:1\n",
	})
	err := LoadInto(base, "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "application-prod.yml") || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("profile 文件里的多份文档同样启动失败，并指出是哪个文件、哪一行，got=%v", err)
	}
}
