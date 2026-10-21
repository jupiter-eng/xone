package e2e

import (
	"sort"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// xmetric/README.md, XMetric: Namespace is "the metric name prefix" and ConstLabels is "added to
// every metric".
// What is checked is the real /metrics: the framework's own request metrics, a metric the business
// made with the shortcut, and the Go runtime and process metrics
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
	m := waitMetrics(t, p, "/login turning up in cov_http_requests_total", func(m harness.Metrics) bool {
		return m.Sum("cov_http_requests_total", "route", "/login") == 1
	})
	if m.Has("e2e_http_requests_total") {
		t.Errorf("with Namespace changed to cov there should be no request metric under the e2e_ prefix any more")
	}
	for _, name := range []string{"cov_http_requests_total", "cov_http_request_duration_seconds_count", "cov_logins_total"} {
		ss := m.Find(name)
		if len(ss) == 0 {
			t.Errorf("with Namespace: cov there should be a %s", name)
			continue
		}
		for _, s := range ss {
			if s.Labels["env"] != "e2e-cov" || s.Labels["team"] != "infra" {
				t.Errorf("ConstLabels should be added to %s (env=e2e-cov team=infra), got %v", name, s.Labels)
				break
			}
		}
	}
	// The other metrics (the Go runtime, the process, the pools, log_errors_total and so on) are "every metric" too
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
		// The go_* and process_* families used to carry no ConstLabels: xmetric's New registered client_golang's
		// ready-made collectors on the Registry directly. A dashboard filtering by env would find nothing for go_goroutines{env="prod"}
		t.Errorf("ConstLabels should be added to every metric, and %d carry none: %s", len(missing), strings.Join(missing, ", "))
	}
	for _, name := range []string{"go_goroutines", "process_cpu_seconds_total"} {
		if !m.Has(name) {
			t.Errorf("%s should be collected by default, which is what shows it carries the ConstLabels as well", name)
		}
	}
}

// The table in xmetric/README.md, "Behavior (measured)"
func TestCoverage_NamespaceOrConstLabelsTypoFailsStartup(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	for _, c := range []struct {
		name, overlay string
		want          []string
	}{
		{"a Namespace with a hyphen", "XMetric:\n  Namespace: my-app\n", []string{"xmetric", `Namespace "my-app" is not a valid metric name prefix`}},
		{"a Namespace starting with a digit", "XMetric:\n  Namespace: 1app\n", []string{`Namespace "1app" is not a valid metric name prefix`}},
		{"a ConstLabels key colliding with route", "XMetric:\n  ConstLabels:\n    route: x\n", []string{`ConstLabels key "route" is reserved`}},
		{"a ConstLabels key colliding with level", "XMetric:\n  ConstLabels:\n    level: x\n", []string{`ConstLabels key "level" is reserved`}},
		{"a ConstLabels key of le", "XMetric:\n  ConstLabels:\n    le: x\n", []string{`ConstLabels key "le" is reserved`}},
		{"a ConstLabels key colliding with go_info's version", "XMetric:\n  ConstLabels:\n    version: x\n", []string{`ConstLabels key "version" is reserved`, "go_info"}},
		{"a ConstLabels key starting with __", "XMetric:\n  ConstLabels:\n    __x: y\n", []string{"__x"}},
		{"a ConstLabels key with a hyphen", "XMetric:\n  ConstLabels:\n    my-env: y\n", []string{"my-env"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			faultMustContain(t, "the start error for "+c.name, covStartupError(t, harness.Options{Overlay: c.overlay}), c.want...)
		})
	}
}

// xapp/README.md, App: "the metrics do not read this block, and /metrics carries no application
// name and no version: to tell applications apart in the metrics, use XMetric.ConstLabels".
// The documentation used to say "a metric's default title comes from App", which is not the case
// for the metrics at all (xmetric does not import xapp), and the documentation was what changed.
// The tracing half is TestCoverage_service_NameFromAppOverriddenByOTELEnv and the API
// documentation half is in swagger_ui_test.go; ConstLabels being added to every metric is
// TestCoverage_NamespaceAndConstLabelsOnRealMetrics
func TestCoverage_MetricsCarryNoAppName(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{})
	p.Get(t, "/ping")
	waitMetrics(t, p, "the request metric for /ping turning up in /metrics", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/ping") >= 1
	})
	if body := string(p.Get(t, "/metrics").Body); strings.Contains(body, "xone.e2e.service") {
		t.Errorf("the documentation says the metrics do not read App, and /metrics holds App.Name (xone.e2e.service): a change in behavior has to go into the App section of xapp/README.md")
	}
}
