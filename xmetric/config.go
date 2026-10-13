// Package xmetric 按配置装好 Prometheus 的 Registry 与 /metrics handler，
// 并提供一组免去样板的打点快捷方法。
//
// 需要完整控制时，Registry() 返回的就是原生的 *prometheus.Registry，
// 自己 NewCounterVec 再 MustRegister 即可，本包不挡路。
package xmetric

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XMetric"

// defaultHTTPDurationBuckets HTTP 请求耗时默认桶边界（秒）
//
// 与 prometheus.DefBuckets 同构，头部补一档 1ms 以便观察极快的接口。
// 用秒而非毫秒：Prometheus 约定以基准单位记录，各类 exporter、
// 社区看板与告警模板都按秒来，混用单位会让同一个服务导出两套刻度。
var defaultHTTPDurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Config 指标配置
type Config struct {
	// Namespace 指标名前缀。默认无。
	//
	// 只能是字母、数字、下划线，且不以数字开头，否则启动失败。client_golang
	// v1.24 按 UTF-8 规则校验名字，不合规的前缀不会报错，而是在导出时被悄悄
	// 转义：实测 my-app 导出成 my_app_，1app 成了 _app_，中文成了一串下划线，
	// 看板和告警按配置里写的名字查，什么都查不到。
	Namespace string `yaml:"Namespace"`

	// ConstLabels 附加到所有指标上的常量标签，包括 Go 运行时与进程指标。默认无。
	//
	// 典型用途是区分环境和集群：env: "${ENV:dev}"、cluster: "${CLUSTER:local}"。
	// 标签名的规则同 Namespace，另外不能以 __ 开头（Prometheus 保留），
	// 也不能是 le、quantile、version（go_info 自带）或框架自带指标的变量标签名
	// （level、caller、method、status、route、host、name），否则启动失败：
	// 常量标签附加在所有指标上，撞了名的那个指标就建不起来。
	ConstLabels map[string]string `yaml:"ConstLabels"`

	// HTTPDurationBuckets HTTP 出入站耗时 Histogram 的桶边界（秒）。
	// 默认 [0.001 0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10]。
	//
	// 必须严格递增、都是有限数。写了空列表 [] 启动失败：列表字段是整体替换的，
	// [] 不是「用默认」，而是被 Prometheus 悄悄换成它自己的 DefBuckets
	// （少了 1ms 那一档）。要默认值就别写这个字段。
	HTTPDurationBuckets []float64 `yaml:"HTTPDurationBuckets"`

	// HistogramBuckets 快捷方法建出来的业务 Histogram 的桶边界（秒）。
	// 默认 prometheus.DefBuckets。规则同 HTTPDurationBuckets。
	//
	// 不在这里拦的话，[1, 0.5, 2] 能通过启动，然后在第一次 HistogramObserve
	// 时 panic（histogram buckets must be in increasing order）——那是在业务
	// 请求里，不是在启动时。
	HistogramBuckets []float64 `yaml:"HistogramBuckets"`

	// GoMetrics 是否采集 Go 运行时指标（goroutine 数、GC 等）。默认开启。
	GoMetrics bool `yaml:"GoMetrics"`

	// ProcessMetrics 是否采集进程指标（CPU、内存、文件描述符等）。默认开启。
	ProcessMetrics bool `yaml:"ProcessMetrics"`

	// LogErrorMetric 是否把 Error 及以上级别的日志计入 log_errors_total。默认开启。
	//
	// 它让「错误率」这条最常用的告警不必等业务先埋点。
	// 统计的是走 xlog 的日志：不用 xlog 的应用这项不会报错，只是数不到。
	LogErrorMetric bool `yaml:"LogErrorMetric"`
}

// nameRE 指标名前缀和标签名的合法形态：经典的 Prometheus 规则，不含冒号
// （冒号留给录制规则），这样在任何导出格式下名字都原样出现
var nameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// reservedLabels 不能当常量标签名的那些，值是它被谁占着。
//
// 常量标签附加在所有指标上，和哪个指标自己的变量标签撞名，那个指标就建不起来。
// 实测（client_golang v1.24.1）不拦的后果：
//   - le：New 照常通过，第一次 HistogramObserve 在业务请求里 panic
//   - quantile：NewSummary 当场 panic
//   - 其余是框架自带指标的变量标签：注册失败，只打一条错误日志，
//     那组指标从此一个都导不出去，服务照常跑
//
// 这些指标分散在各自的模块里，而它们都依赖本模块，没法反过来引用，
// 所以集中列在这里：给框架加指标、改变量标签时要同步。
var reservedLabels = map[string]string{
	"le":       "histogram buckets",
	"quantile": "summary quantiles",
	"level":    "log_errors_total",
	"caller":   "log_errors_total",
	"method":   "the xgin / xecho and xhttp request metrics",
	"status":   "the xgin / xecho and xhttp request metrics",
	"route":    "the xgin / xecho request metrics",
	"host":     "the xhttp request metrics",
	"name":     "the xgorm, xredis and xcache instance metrics",
	// go_info 自带常量标签 version，常量标签也附加在 Go 运行时指标上，
	// 实测撞名时 New 报 attempted wrapping with already existing label name "version"
	"version": "go_info in the Go runtime metrics",
}

// Validate 检查配置本身说不通的地方，在建任何指标之前就失败。
//
// xconfig.Unmarshal 解完配置文件里的 XMetric 块会调它；直接调 New 的，New 也会调一次。
// 返回普通 error，由调它的那一层包一次 xerror。
func (c Config) Validate() error {
	if c.Namespace != "" && !nameRE.MatchString(c.Namespace) {
		return fmt.Errorf("Namespace %q is not a valid metric name prefix: use letters, digits and underscores, not starting with a digit", c.Namespace)
	}
	for k := range c.ConstLabels {
		if !nameRE.MatchString(k) || strings.HasPrefix(k, "__") {
			return fmt.Errorf("ConstLabels key %q is not a valid label name: use letters, digits and underscores, not starting with a digit or __", k)
		}
		if owner, ok := reservedLabels[k]; ok {
			return fmt.Errorf("ConstLabels key %q is reserved as a label of %s, choose another name", k, owner)
		}
	}
	if err := validBuckets(c.HTTPDurationBuckets); err != nil {
		return fmt.Errorf("HTTPDurationBuckets: %w", err)
	}
	if err := validBuckets(c.HistogramBuckets); err != nil {
		return fmt.Errorf("HistogramBuckets: %w", err)
	}
	return nil
}

// validBuckets nil 表示用默认值；写了就必须非空、有限、严格递增
func validBuckets(b []float64) error {
	if b == nil {
		return nil
	}
	if len(b) == 0 {
		return errors.New("must not be an empty list, leave the field out to use the defaults")
	}
	for i, v := range b {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("bucket %v is not a finite number", v)
		}
		if i > 0 && v <= b[i-1] {
			return fmt.Errorf("buckets must be strictly increasing, got %v after %v", v, b[i-1])
		}
	}
	return nil
}

// withDefaults 没给桶（nil）的补上默认值。
//
// 直接用 Config{} 调 New 的话两个桶都是 nil，交给 Prometheus 就是它自己的
// DefBuckets，而不是这里的默认值——同一个「没配」有两种结果
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	// 拷一份：调用方手里那个 map 之后再改，不该改到已经装好的设施
	c.ConstLabels = maps.Clone(c.ConstLabels)
	if c.HTTPDurationBuckets == nil {
		c.HTTPDurationBuckets = d.HTTPDurationBuckets
	}
	if c.HistogramBuckets == nil {
		c.HistogramBuckets = d.HistogramBuckets
	}
	return c
}

// DefaultConfig 全部默认值集中在这里。
func DefaultConfig() Config {
	return Config{
		HTTPDurationBuckets: slices.Clone(defaultHTTPDurationBuckets),
		HistogramBuckets:    slices.Clone(prometheus.DefBuckets),
		GoMetrics:           true,
		ProcessMetrics:      true,
		LogErrorMetric:      true,
	}
}
