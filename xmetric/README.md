# xmetric

Prometheus 指标：原生的 `*prometheus.Registry` 用 `xmetric.Registry()` 取，日常打点有一组免样板的快捷方法。

- 指标端点由 xgin / xecho 自动挂在 `/metrics`，不用它们的自己挂 `xmetric.Handler()`
- 快捷方法按名字建指标、按标签复用，不用先声明
- 耗时指标收 `time.Duration`、自动补 `_seconds` 后缀
- 自带 `log_errors_total`（Error 级别日志计数）和 `go_*` / `process_*`
- 名字、标签、桶写错在读配置时就失败，不等到导出时悄悄转义

## 快速上手

```yaml
# conf/application.yml（可选）
XMetric:
  Namespace: shop          # 所有指标名前加 shop_
  ConstLabels:
    env: "${ENV:dev}"      # 附加到所有指标上
```

```go
import "github.com/jupiter-eng/xone/xmetric"

func Place(ctx context.Context, o *Order) (err error) {
	start := time.Now()
	defer func() {
		status := "ok"
		if err != nil {
			status = "error"
		}
		xmetric.CounterInc("orders_total", xmetric.T("status", status))
		xmetric.ObserveDuration("order_place", time.Since(start), xmetric.T("status", status)) // 导出为 order_place_seconds
	}()
	return save(ctx, o)
}

// 标签不随结果变时更短：defer xmetric.Timer("order_query")()
```

## 配置

```yaml
XMetric:
  Namespace: myapp         # 指标名前缀，默认无
  ConstLabels:             # 附加到所有指标上（含 go_* / process_*），默认无
    env: "${ENV:dev}"
  HTTPDurationBuckets: [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]  # 出入站 HTTP 耗时（秒），此为默认
  HistogramBuckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]           # 快捷方法建的直方图，即 prometheus.DefBuckets
  GoMetrics: true          # Go 运行时指标，默认开
  ProcessMetrics: true     # 进程指标，默认开
  LogErrorMetric: true     # Error 及以上级别的日志计入 log_errors_total，默认开；只数走 xlog 的日志
```

- 两组桶不写就是默认值，写了就整体替换；桶必须严格递增。
- 指标端点的路径是 `XGin.MetricPath` / `XEcho.MetricPath`。

## API

| 函数 | 说明 |
|---|---|
| `CounterInc(name, tags...)` / `CounterAdd(name, v, tags...)` | 计数器 +1 / +v（v 必须 >= 0） |
| `GaugeSet(name, v, tags...)` / `GaugeInc` / `GaugeDec` | 仪表盘 |
| `HistogramObserve(name, v, tags...)` | 直方图观测，单位秒，桶是 `XMetric.HistogramBuckets` |
| `ObserveDuration(name, d, tags...)` | 记一次耗时，`time.Duration` 换算成秒，名字补 `_seconds` |
| `Timer(name, tags...) func()` | `defer xmetric.Timer("x")()`；标签在开始时就固定，重复调用只有首次生效 |
| `TrackInFlight(name, tags...) func()` | 进行中的数量，`defer xmetric.TrackInFlight("x")()`，不会漏减 |
| `T(name, value) Tag` | 构造一个标签 |
| `Registry() *prometheus.Registry` | 原生 Registry，要完整控制时自己 `NewCounterVec` 再注册 |
| `MustRegister(cs...)` | 注册自定义 collector，重复注册会 panic |
| `Register(c)` / `RegisterAs[T](c) (T, error)` | 注册，已注册过同名同标签的复用已有实例；**务必用返回值** |
| `ConstLabels()` / `Namespace()` / `HTTPDurationBuckets()` | 读生效中的配置，自己建指标时填进 `prometheus.Opts`，和框架指标带同样的标签 |
| `Handler() http.Handler` | `/metrics` 的 handler，不用 xgin / xecho 的服务自己挂 |
| `New(cfg) (*Metrics, io.Closer, error)` | 纯构造器：不碰全局、不读配置文件，离开框架也能用 |

## 注意事项

- **名字写错读配置时就失败**：`Namespace` 和 `ConstLabels` 的 key 只收字母、数字、下划线，不以数字开头，
  `ConstLabels` 的 key 也不以 `__` 开头（client_golang 不报错，会悄悄把 `my-app` 导出成 `my_app_…`）。见[「行为与实测」](#行为与实测)。
- **`ConstLabels` 的 key 有保留字**：`le`、`quantile`、`version` 和框架指标自己的变量标签
  （`level`、`caller`、`method`、`status`、`route`、`host`、`name`）不能用，读配置时失败。
- **桶写成空列表 `[]` 不是「用默认」**，直接失败；要默认值就别写这个字段。
- **`Register` 同名但类型或标签不同时返回错误**：传进去的那个不在 registry 里，通过它记的值导不出去。
- 框架自带的指标见 [observability.md「指标」](../docs/observability.md#metrics)。

## 可观测

### 指标

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `log_errors_total` | counter | `level`、`caller` | xmetric，`XMetric.LogErrorMetric`，需要 xlog |
| `go_*` / `process_*` | —— | —— | `XMetric.GoMetrics` / `ProcessMetrics` |

指标名的前缀、常量标签和几条通用规则见 [`docs/observability.md`「指标」](../docs/observability.md#metrics)。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

client_golang v1.24.1。

| 写法 | 不拦的话（实测） |
|---|---|
| 桶不是严格递增（`[1, 0.5, 2]`、`[1, 1]`），或含 NaN / Inf | 通过启动，第一次 `HistogramObserve` 时在业务请求里 panic |
| 桶写成空列表 `[]` | 不是「用默认」：被 Prometheus 悄悄换成它自己的 `DefBuckets`（HTTP 那组少了 1ms 一档） |
| `Namespace` 或 `ConstLabels` 的 key 不合规 | 不报错，导出时转义：`my-app` 导出成 `my_app_…`，`1app` 成 `_app_…`，中文成一串下划线 |
| `ConstLabels` 的 key 是 `le` / `quantile` | `le` 通过启动，第一次 `HistogramObserve` 时 panic；`quantile` 让建 Summary 当场 panic |
| `ConstLabels` 的 key 撞上框架自带指标的变量标签 | 撞名的那个注册失败，只打一条错误日志，那组指标一个都导不出去 |
| `ConstLabels` 的 key 是 `version` | `go_info` 自带常量标签 `version`，client_golang 拒绝注册整组 Go 运行时指标（`attempted wrapping with already existing label name "version"`） |

所以这些在读配置时就失败。
