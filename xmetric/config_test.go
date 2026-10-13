package xmetric

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xonetest"
)

func TestNew_InconsistentConfigFailsAtStartup(t *testing.T) {
	// 不在这里拦的话：乱序的桶在第一次 Observe 时 panic（在业务请求里）；
	// 不合规的 Namespace 不报错，而是导出时被悄悄转义（my-app → my_app_），
	// 看板和告警按配置里写的名字查不到
	for name, mutate := range map[string]func(*Config){
		"桶乱序":            func(c *Config) { c.HistogramBuckets = []float64{1, 0.5, 2} },
		"桶有重复":           func(c *Config) { c.HistogramBuckets = []float64{1, 1} },
		"桶有 NaN":         func(c *Config) { c.HistogramBuckets = []float64{0.1, math.NaN()} },
		"桶有 Inf":         func(c *Config) { c.HTTPDurationBuckets = []float64{0.1, math.Inf(1)} },
		"写了空列表":          func(c *Config) { c.HTTPDurationBuckets = []float64{} },
		"HTTP 桶乱序":       func(c *Config) { c.HTTPDurationBuckets = []float64{2, 1} },
		"Namespace 带横线":  func(c *Config) { c.Namespace = "my-app" },
		"Namespace 数字开头": func(c *Config) { c.Namespace = "1app" },
		"Namespace 带冒号":  func(c *Config) { c.Namespace = "app:x" },
		"Namespace 是中文":  func(c *Config) { c.Namespace = "应用" },
		"常量标签名带横线":       func(c *Config) { c.ConstLabels = map[string]string{"my-env": "x"} },
		"常量标签名用了保留前缀":    func(c *Config) { c.ConstLabels = map[string]string{"__name": "x"} },
		// 实测 le 照常通过启动，第一次 HistogramObserve 在业务请求里 panic
		"常量标签名是直方图保留的 le":             func(c *Config) { c.ConstLabels = map[string]string{"le": "x"} },
		"常量标签名是 summary 保留的 quantile": func(c *Config) { c.ConstLabels = map[string]string{"quantile": "x"} },
		// 撞上框架自带指标的变量标签：那个指标注册失败，只打一条错误日志
		"常量标签名撞上日志计数的标签":  func(c *Config) { c.ConstLabels = map[string]string{"level": "x"} },
		"常量标签名撞上请求指标的标签":  func(c *Config) { c.ConstLabels = map[string]string{"method": "x"} },
		"常量标签名撞上连接池指标的标签": func(c *Config) { c.ConstLabels = map[string]string{"name": "x"} },
		// 关掉 Go 运行时指标也拦：开关一拨就启动失败的配置，不如一开始就不让写
		"常量标签名撞上 go_info 的标签": func(c *Config) { c.ConstLabels = map[string]string{"version": "x"} },
	} {
		c := DefaultConfig()
		c.GoMetrics, c.ProcessMetrics = false, false
		mutate(&c)
		_, closer, err := New(c)
		if err == nil {
			t.Errorf("%s 应当报错", name)
			continue
		}
		var xe *xerror.Error
		if !errors.As(err, &xe) || xe.Module != "xmetric" || xe.Op != "config" {
			t.Errorf("%s：该是 xmetric 的 config 错误，got %v", name, err)
		}
		if closer != nil {
			t.Errorf("%s：出错时不该返回 Closer", name)
		}
	}
}

func TestNew_ValidConfigPasses(t *testing.T) {
	c := DefaultConfig()
	c.GoMetrics, c.ProcessMetrics = false, false
	c.Namespace = "my_app_2"
	c.ConstLabels = map[string]string{"env": "prod", "_zone": "a"}
	c.HistogramBuckets = []float64{-1, 0, 0.5, 10}
	if _, _, err := New(c); err != nil {
		t.Fatalf("合法配置不该报错：%v", err)
	}
}

func TestNew_DefaultBucketsWhenNoneGiven(t *testing.T) {
	// Config{} 的两个桶都是 nil，原样交给 Prometheus 就是它自己的 DefBuckets，
	// 而不是这里的默认值——同一个「没配」有两种结果
	m, closer, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if closer == nil {
		t.Error("成功时 Closer 不该为 nil")
	}
	if !slices.Equal(m.cfg.HTTPDurationBuckets, defaultHTTPDurationBuckets) {
		t.Errorf("HTTP 桶该用默认值，got %v", m.cfg.HTTPDurationBuckets)
	}
	if len(m.cfg.HistogramBuckets) == 0 {
		t.Error("业务直方图的桶该用默认值")
	}
}

func TestInitXMetric_EmptyListInConfigFailsStartup(t *testing.T) {
	// 列表字段整体替换：写 [] 的人多半以为是「用默认」，实际被 Prometheus
	// 悄悄换成了它自己的 DefBuckets
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XMetric:\n  HTTPDurationBuckets: []\n")

	err := initXMetric(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTPDurationBuckets") {
		t.Fatalf("空列表应当让启动失败并点名字段，got %v", err)
	}
	// 在读配置时就拦下（xconfig.Unmarshal 调 Validate），不是等到 New
	if !xerror.Is(err, "xconfig") {
		t.Errorf("错误该出自读配置那一步，got %v", err)
	}
}
