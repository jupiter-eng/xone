package e2e

import (
	"sort"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// xmetric/README.md XMetric：Namespace「指标名前缀」，ConstLabels「附加到所有指标上」。
// 查的是真的 /metrics：框架自带的请求指标、业务用快捷方法建的指标、Go 运行时与进程指标
func TestCoverage_NamespaceAndConstLabelsOnRealMetrics(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Overlay: `XMetric:
  Namespace: cov
  ConstLabels:
    env: e2e-cov
    team: infra
`})
	p.PostJSON(t, "/login", map[string]string{"username": "metric"})
	m := waitMetrics(t, p, "cov_http_requests_total 里出现 /login", func(m harness.Metrics) bool {
		return m.Sum("cov_http_requests_total", "route", "/login") == 1
	})
	if m.Has("e2e_http_requests_total") {
		t.Errorf("Namespace 换成 cov 之后不该还有 e2e_ 前缀的请求指标")
	}
	for _, name := range []string{"cov_http_requests_total", "cov_http_request_duration_seconds_count", "cov_logins_total"} {
		ss := m.Find(name)
		if len(ss) == 0 {
			t.Errorf("Namespace: cov 时应有 %s", name)
			continue
		}
		for _, s := range ss {
			if s.Labels["env"] != "e2e-cov" || s.Labels["team"] != "infra" {
				t.Errorf("ConstLabels 应附加到 %s 上（env=e2e-cov team=infra），实际 %v", name, s.Labels)
				break
			}
		}
	}
	// 其余指标（Go 运行时、进程、连接池、log_errors_total……）同样是「所有指标」
	var missing []string
	seen := map[string]bool{}
	for _, s := range m.Samples {
		if s.Labels["env"] != "e2e-cov" && !seen[s.Name] {
			seen[s.Name] = true
			missing = append(missing, s.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		// 从前 go_* / process_* 两组没有 ConstLabels：xmetric 的 New 把 client_golang 现成的
		// collector 直接注册在 Registry 上。按 env 过滤的看板查 go_goroutines{env="prod"} 什么都查不到
		t.Errorf("ConstLabels 应附加到所有指标上，没带的 %d 个：%s", len(missing), strings.Join(missing, ", "))
	}
	for _, name := range []string{"go_goroutines", "process_cpu_seconds_total"} {
		if !m.Has(name) {
			t.Errorf("默认应采集 %s，才能说明它也带上了 ConstLabels", name)
		}
	}
}

// xmetric/README.md「行为与实测」那张表
func TestCoverage_NamespaceOrConstLabelsTypoFailsStartup(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, c := range []struct {
		name, overlay string
		want          []string
	}{
		{"Namespace 带连字符", "XMetric:\n  Namespace: my-app\n", []string{"xmetric", `Namespace "my-app" is not a valid metric name prefix`}},
		{"Namespace 以数字开头", "XMetric:\n  Namespace: 1app\n", []string{`Namespace "1app" is not a valid metric name prefix`}},
		{"ConstLabels 撞上 route", "XMetric:\n  ConstLabels:\n    route: x\n", []string{`ConstLabels key "route" is reserved`}},
		{"ConstLabels 撞上 level", "XMetric:\n  ConstLabels:\n    level: x\n", []string{`ConstLabels key "level" is reserved`}},
		{"ConstLabels 是 le", "XMetric:\n  ConstLabels:\n    le: x\n", []string{`ConstLabels key "le" is reserved`}},
		{"ConstLabels 撞上 go_info 的 version", "XMetric:\n  ConstLabels:\n    version: x\n", []string{`ConstLabels key "version" is reserved`, "go_info"}},
		{"ConstLabels 以 __ 开头", "XMetric:\n  ConstLabels:\n    __x: y\n", []string{"__x"}},
		{"ConstLabels 带连字符", "XMetric:\n  ConstLabels:\n    my-env: y\n", []string{"my-env"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			faultMustContain(t, c.name+" 的启动错误", covStartupError(t, harness.Options{Overlay: c.overlay}), c.want...)
		})
	}
}

// xapp/README.md App：「指标不读这一块，/metrics 上没有应用名和版本：要在指标上区分应用，用 XMetric.ConstLabels」。
// 文档从前写「指标的默认标题取自 App」，指标上并没有这回事（xmetric 不 import xapp），改的是文档。
// 链路那一半见 TestCoverage_service_NameFromAppOverriddenByOTELEnv，接口文档那一半见 swagger_ui_test.go；
// ConstLabels 附加到所有指标上见 TestCoverage_NamespaceAndConstLabelsOnRealMetrics
func TestCoverage_MetricsCarryNoAppName(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{})
	p.Get(t, "/ping")
	waitMetrics(t, p, "/metrics 里有 /ping 的请求指标", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/ping") >= 1
	})
	if body := string(p.Get(t, "/metrics").Body); strings.Contains(body, "xone.e2e.service") {
		t.Errorf("文档说指标不读 App，/metrics 里却出现了 App.Name（xone.e2e.service）：改了行为就要同步 xapp/README.md 的 App 一节")
	}
}
