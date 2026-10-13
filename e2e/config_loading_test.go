package e2e

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covSeen GET /probe/config：main 里（xone.Run 之前）读到的 Service 块，启动钩子里读到的 Cov 块
type covSeen struct {
	Service struct {
		Downstream string `json:"downstream"`
		UserTTL    string `json:"user_ttl"`
	} `json:"service"`
	Cov struct {
		Label  string            `json:"Label"`
		Items  []string          `json:"Items"`
		Labels map[string]string `json:"Labels"`
	} `json:"cov"`
}

func covReadConfig(t *testing.T, p *harness.Process) covSeen {
	t.Helper()
	var s covSeen
	p.Get(t, "/probe/config").JSON(t, &s)
	return s
}

// covBase 一份 service/application.yml 加上 Cov 块（和 extra 里的其它顶层块），写进一个新的临时目录，返回路径。
// profile 文件、Import 的片段都相对它所在的目录写
func covBase(t *testing.T, extra map[string]string) string {
	t.Helper()
	blocks := map[string]string{"Cov": "Cov:\n  Label: base\n  Items: [x, y]\n  Labels: {b: base}\n"}
	for k, v := range extra {
		blocks[k] = v
	}
	return covConfig(t, blocks)
}

// docs/config.md「Profiles —— 按环境分文件」：
//
//	优先级：--profile > XONE_PROFILE > 文件里的 XApp.Profiles
//	--profile=prod,eu 多个用逗号分隔，靠后的压过靠前的
//	点名的 profile 文件不存在时直接启动失败
//
// 「合并规则」：map 递归合并、列表整体替换、标量覆盖。
// 「通用规则·什么时候读」：在 main 里、xone.Run 之前读到的也是最终值——e2e 服务的 Service 块就是在 main 里读的
func TestCoverage_ProfileThreeWaysToSetAndPrecedence_MergeRules(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	cfg := covBase(t, map[string]string{"XApp": "XApp:\n  Name: xone.e2e.service\n  Version: e2e\n  Profiles: [fromfile]\n"})
	dir := filepath.Dir(cfg)
	covWrite(t, dir, "application-fromfile.yml", "Cov:\n  Label: fromfile\n")
	covWrite(t, dir, "application-envp.yml", "Cov:\n  Label: envp\n  Labels: {e: envp}\n")
	covWrite(t, dir, "application-argp.yml", "Cov:\n  Label: argp\n  Items: [z]\n  Labels: {a: argp}\nService:\n  Downstream: http://from-argp.invalid\n  UserTTL: 7s\n")

	for _, c := range []struct {
		name   string
		args   []string
		env    map[string]string
		label  string
		items  []string
		labels map[string]string
	}{
		{name: "只有 XApp.Profiles", label: "fromfile", items: []string{"x", "y"}, labels: map[string]string{"b": "base"}},
		{name: "XONE_PROFILE 压过 XApp.Profiles", env: map[string]string{"XONE_PROFILE": "envp"},
			label: "envp", items: []string{"x", "y"}, labels: map[string]string{"b": "base", "e": "envp"}},
		{name: "--profile 压过 XONE_PROFILE", args: []string{"--profile=argp"}, env: map[string]string{"XONE_PROFILE": "envp"},
			label: "argp", items: []string{"z"}, labels: map[string]string{"b": "base", "a": "argp"}},
		{name: "--profile=envp,argp：靠后的压过靠前的，map 逐层合并，列表整体替换", args: []string{"--profile=envp,argp"},
			label: "argp", items: []string{"z"}, labels: map[string]string{"b": "base", "e": "envp", "a": "argp"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Config: cfg, Args: c.args, Env: c.env})
			s := covReadConfig(t, p)
			if s.Cov.Label != c.label || !slices.Equal(s.Cov.Items, c.items) || fmt.Sprint(s.Cov.Labels) != fmt.Sprint(c.labels) {
				t.Errorf("生效的 Cov 应是 Label=%s Items=%v Labels=%v，实际 %+v", c.label, c.items, c.labels, s.Cov)
			}
			// Service 块是 main 里、xone.Run 之前读的：argp 生效时读到的也得是 argp 里的值
			wantDS := ""
			if c.label == "argp" {
				wantDS = "http://from-argp.invalid"
			}
			if s.Service.Downstream != wantDS {
				t.Errorf("docs/config.md：在 main 里、xone.Run 之前读到的也是最终值；Service.Downstream 应是 %q，实际 %q", wantDS, s.Service.Downstream)
			}
		})
	}

	t.Run("点名的 profile 文件不存在时启动失败", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Config: cfg, Args: []string{"--profile=nosuch"}})
		faultMustContain(t, "--profile=nosuch 的启动错误", stderr, "application-nosuch.yml")
	})
}

// docs/config.md「Import —— 引入别的配置文件」：
//
//	引进来的压过引它的那个文件；引进来的文件同样有 profile 变体（不存在不算错）
//	相对路径按引它的文件所在目录解析，不是进程的工作目录
//	optional: 前缀，文件不存在就跳过；没有前缀的不存在是错误
//
// 「合并规则」：application.yml < 它 Import 的（含片段自己的 -prod 变体）< application-prod.yml
func TestCoverage_ImportPrecedence_RelativePath_Optional_FragmentProfiles(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	cfg := covBase(t, map[string]string{"XApp": "XApp:\n  Name: xone.e2e.service\n  Version: e2e\n  Import:\n    - parts/shared.yml\n    - optional:parts/missing.yml\n"})
	dir := filepath.Dir(cfg)
	covWrite(t, dir, "parts/shared.yml", "Cov:\n  Label: imported\n  Items: [imp]\n  Labels: {i: imp}\n")
	covWrite(t, dir, "parts/shared-prod.yml", "Cov:\n  Labels: {iv: variant}\n")
	covWrite(t, dir, "application-prod.yml", "Cov:\n  Label: prod\n")

	t.Run("没有 profile：片段压过 base", func(t *testing.T) {
		t.Parallel()
		// 工作目录是 Process.Dir，和配置文件不在一处：parts/ 能找到说明是按引它的文件解析的
		s := covReadConfig(t, harness.Start(t, harness.Options{Config: cfg}))
		if s.Cov.Label != "imported" || !slices.Equal(s.Cov.Items, []string{"imp"}) || fmt.Sprint(s.Cov.Labels) != fmt.Sprint(map[string]string{"b": "base", "i": "imp"}) {
			t.Errorf("引进来的压过引它的那个文件，应是 Label=imported Items=[imp] Labels={b i}，实际 %+v", s.Cov)
		}
	})
	t.Run("--profile=prod：profile 压过片段，片段的变体也叠上来", func(t *testing.T) {
		t.Parallel()
		s := covReadConfig(t, harness.Start(t, harness.Options{Config: cfg, Args: []string{"--profile=prod"}}))
		want := map[string]string{"b": "base", "i": "imp", "iv": "variant"}
		if s.Cov.Label != "prod" || !slices.Equal(s.Cov.Items, []string{"imp"}) || fmt.Sprint(s.Cov.Labels) != fmt.Sprint(want) {
			t.Errorf("应是 Label=prod（profile 压过片段）Items=[imp] Labels=%v（片段的 -prod 变体叠上来），实际 %+v", want, s.Cov)
		}
	})
	t.Run("没有 optional: 前缀的片段不存在时启动失败", func(t *testing.T) {
		t.Parallel()
		bad := covBase(t, map[string]string{"XApp": "XApp:\n  Name: xone.e2e.service\n  Version: e2e\n  Import: parts/nowhere.yml\n"})
		faultMustContain(t, "Import 的文件不存在时的启动错误", covStartupError(t, harness.Options{Config: bad}), "nowhere.yml")
	})
}

// docs/config.md「通用规则」：
//
//	${VAR:default}  可选，未设置时用默认值
//	${VAR}          必填（functional_config_test.go 测过未设置时启动失败）
//	占位符展开为空  ${VAR:} 等于这一项没写，保持结构体里的默认值；真要空串就加引号 "${VAR:}"
func TestCoverage_PlaceholderDefaults_EmptyExpansionKeepsStructDefault(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	cfg := covBase(t, map[string]string{"Cov": "Cov:\n  Label: ${COV_LABEL:fallback}\n  Items: [\"${COV_ITEM:}\", \"b\"]\n"})
	empty := covBase(t, map[string]string{"Cov": "Cov:\n  Label: ${COV_LABEL:}\n"})
	quoted := covBase(t, map[string]string{"Cov": "Cov:\n  Label: \"${COV_LABEL:}\"\n"})
	for _, c := range []struct {
		name, cfg string
		env       map[string]string
		label     string
	}{
		{"没设置：用冒号后面的默认值", cfg, nil, "fallback"},
		{"设置了：用变量的值", cfg, map[string]string{"COV_LABEL": "from-env"}, "from-env"},
		{"${VAR:} 展开为空：保持结构体默认值", empty, nil, "default"},
		{"\"${VAR:}\" 加了引号：空串", quoted, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := covReadConfig(t, harness.Start(t, harness.Options{Config: c.cfg, Env: c.env}))
			if s.Cov.Label != c.label {
				t.Errorf("Cov.Label 应是 %q，实际 %q", c.label, s.Cov.Label)
			}
			// 列表元素里加了引号的 "${COV_ITEM:}" 是空串，不是「没写」
			if c.cfg == cfg && !slices.Equal(s.Cov.Items, []string{"", "b"}) {
				t.Errorf("Items 里加了引号的 \"${COV_ITEM:}\" 应展开成空串，实际 %q", s.Cov.Items)
			}
		})
	}
}

// docs/config.md 开头：「配置文件位置：--config=<path> > XONE_CONFIG > conf/application.yml 等约定路径」；
// README：「显式指定的找不到是错误」
func TestCoverage_ConfigFileLocation_ConfigFlagBeatsXONE_CONFIG(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	a := covBase(t, map[string]string{"Cov": "Cov:\n  Label: file-a\n"})
	b := covBase(t, map[string]string{"Cov": "Cov:\n  Label: file-b\n"})

	t.Run("只有 XONE_CONFIG", func(t *testing.T) {
		t.Parallel()
		p := harness.StartArgs(t, harness.Options{Env: map[string]string{"XONE_CONFIG": a}})
		if got := covReadConfig(t, p).Cov.Label; got != "file-a" {
			t.Errorf("没给 --config 时读 XONE_CONFIG 指的文件（file-a），实际 %q", got)
		}
	})
	t.Run("--config 压过 XONE_CONFIG", func(t *testing.T) {
		t.Parallel()
		p := harness.StartArgs(t, harness.Options{Env: map[string]string{"XONE_CONFIG": a}}, "--config="+b)
		if got := covReadConfig(t, p).Cov.Label; got != "file-b" {
			t.Errorf("--config 优先于 XONE_CONFIG，应读 file-b，实际 %q", got)
		}
	})
	t.Run("XONE_CONFIG 指的文件不存在时启动失败", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "nope.yml")
		p := harness.StartArgs(t, harness.Options{NoWait: true, Env: map[string]string{"XONE_CONFIG": missing}})
		exit, ok := p.Wait(30 * time.Second)
		if !ok || exit.Code == 0 {
			t.Fatalf("显式指定的配置文件找不到应启动失败，实际 %v（退出了=%v）", exit, ok)
		}
		faultMustContain(t, "XONE_CONFIG 指向不存在的文件时的启动错误", p.Stderr(), missing)
	})
}

// covAppRun 跑一次 covapp（一次性任务，读完就退出），返回退出情况、各次读到的 App.Name 和 stderr
func covAppRun(t *testing.T, env map[string]string, args ...string) (harness.Exit, map[string]string, string) {
	t.Helper()
	p := harness.StartCovApp(t, harness.Options{Env: env}, args...)
	exit, ok := p.Wait(30 * time.Second)
	if !ok {
		t.Fatalf("covapp 30s 了还没退出\n%s", p.Output())
	}
	seen := map[string]string{}
	for _, l := range p.FindLogs(func(l harness.Log) bool { return l.Msg() == "covapp read" }) {
		seen[l.Str("phase")] = l.Str("name")
	}
	return exit, seen, p.Stderr()
}

// xone.WithConfigPath：「指定配置文件，优先于 --config、XONE_CONFIG 和约定路径。
// 在 Run 之前就读过配置的话，配置已经按那几种方式加载过了，这里再点名另一个文件会让 Run 直接报错」。
// e2e 服务不用 WithConfigPath，这一条用 e2e/covapp 测
func TestCoverage_WithConfigPathBeatsConfigFlag_OtherFileAfterReadErrors(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	dir := t.TempDir()
	a := covWrite(t, dir, "a.yml", "XApp:\n  Name: from-a\n")
	b := covWrite(t, dir, "b.yml", "XApp:\n  Name: from-b\n")

	t.Run("没提前读：WithConfigPath 压过 --config", func(t *testing.T) {
		t.Parallel()
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": a}, "--config="+b)
		if exit.Code != 0 || seen["in_run"] != "from-a" {
			t.Errorf("WithConfigPath 优先于 --config，应读 a（from-a）、以 0 退出，实际 %v、读到 %q\n%s", exit, seen["in_run"], stderr)
		}
	})
	t.Run("提前读过（按 --config 加载了 b），再点名 a：Run 报错", func(t *testing.T) {
		t.Parallel()
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": a, "COVAPP_READ_EARLY": "1"}, "--config="+b)
		if exit.Code == 0 {
			t.Errorf("提前读过之后 WithConfigPath 点名另一个文件，Run 应报错，实际以 0 退出、Start 读到 %q", seen["in_run"])
		}
		if seen["early"] != "from-b" {
			t.Errorf("提前的那次读按 --config 加载 b，应读到 from-b，实际 %q", seen["early"])
		}
		faultMustContain(t, "WithConfigPath 冲突时的错误", stderr, "config was already loaded by an earlier read", b, a, "--config", "XONE_CONFIG")
		if _, ran := seen["in_run"]; ran {
			t.Errorf("Run 报错时不该跑到 Start")
		}
	})
	t.Run("提前读过，WithConfigPath 点名的就是那个文件：照常", func(t *testing.T) {
		t.Parallel()
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": b, "COVAPP_READ_EARLY": "1"}, "--config="+b)
		if exit.Code != 0 || seen["early"] != "from-b" || seen["in_run"] != "from-b" {
			t.Errorf("点名的和已经加载的是同一个文件时不该报错，两次都读到 from-b，实际 %v early=%q in_run=%q\n%s", exit, seen["early"], seen["in_run"], stderr)
		}
	})
	t.Run("提前读过，WithConfigPath 是同一个文件的另一种写法", func(t *testing.T) {
		t.Parallel()
		// 同一个文件：dir/./b.yml。「点名的文件和它不是同一个时报错」——是同一个就不该报错
		same := filepath.Join(dir, ".") + string(filepath.Separator) + "." + string(filepath.Separator) + "b.yml"
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": same, "COVAPP_READ_EARLY": "1"}, "--config="+b)
		if exit.Code != 0 {
			// 从前 Ensure 按字符串比，同一个文件换一种写法（./、相对路径、符号链接）就被当成另一个文件
			t.Errorf("同一个文件换一种写法不该报错，实际 %v\n%s", exit, stderr)
		}
		if seen["in_run"] != "from-b" {
			t.Errorf("Start 里读到的应是 from-b，实际 %q", seen["in_run"])
		}
	})
}
