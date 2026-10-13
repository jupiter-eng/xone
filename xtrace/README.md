# xtrace

链路：装好 OpenTelemetry 的全局 TracerProvider 和 Propagator，业务代码用原生的 `otel.Tracer("...")`。

- xgin / xecho / xgorm / xredis / xhttp 自带它，用了其中任何一个就不用另外 import
- 有上游时听上游的采样决定，没有上游时按 `SampleRatio` 采样
- 按配置透传自定义 Header（如 `X-Request-Id`），可以只发给指定域名
- 透传的 Header 和 `baggage` 只收可信对端的
- 不内置 exporter：要上报就自己 `xtrace.AddSpanProcessor(...)`，退出时由框架 Shutdown

## 快速上手

```yaml
# conf/application.yml
XApp:
  Name: shop.order.api     # 就是 service.name
XTrace:
  SampleRatio: 0.1         # 只管根 Span；有上游时听上游的
  ForwardHeaders: [X-Request-Id]
```

```go
func main() {
	// 框架不内置上报：要上报就自己挂一个 exporter，在 xone.Run 之前
	exp, err := otlptracegrpc.New(context.Background(),
		otlptracegrpc.WithEndpoint(os.Getenv("OTLP_ENDPOINT")), otlptracegrpc.WithInsecure())
	if err != nil {
		log.Fatal(err)
	}
	xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(exp)) // 退出时由框架 Shutdown

	xone.MustRun(xgin.New().WithRoutes(routes))
}

func riskCheck(ctx context.Context, orderID string) {
	// 入站 Span 由 xgin 开好了；要再细分一段，就用原生的 OpenTelemetry API
	ctx, span := otel.Tracer("order").Start(ctx, "risk_check")
	defer span.End()
	span.SetAttributes(attribute.String("order.id", orderID))
	_, _ = xhttp.R(ctx).Get("http://risk.internal/api/v1/check") // traceparent 自动带给下游
}
```

## 配置

```yaml
XTrace:
  Enable: true             # 默认开。关掉后没有 Span，traceparent / baggage 也不再透传，ForwardHeaders 照常
  Console: false           # 把 Span 打到标准输出，本地调试用，默认关
  SampleRatio: 1           # 根 Span 的采样率 [0, 1]，默认 1；有上游时一律听上游的 sampled 位
  ShutdownTimeout: 5s      # 退出时等导出完成的上限，默认 5s，必须 > 0，同时不超过框架的停止预算
  ForwardHeaders:          # 向所有下游透传的 Header，默认无
    - X-Request-Id
  ForwardHeaderRules:      # 只发给匹配域名的 Header，默认无
    - Domains: ["api.internal.com", "*.trusted.com"]
      Headers: ["X-Internal-Token"]
```

- `Domains` 只认 `api.internal.com` 和 `*.trusted.com` 两种写法；`*.trusted.com` 匹配任意层级子域、**不匹配裸域**。
  其余带 `*` 的写法、同一个 header 同时出现在 `ForwardHeaders` 和 `ForwardHeaderRules` 里，都在读配置时失败。
- `XGin.Trace` / `XHttp.Trace` 只管开不开 Span，关掉之后透传照常。
- `AddSpanProcessor` 登记的处理器在 `Enable: false` 时收不到 Span，退出时照样被 Shutdown。

## API

| 函数 | 说明 |
|---|---|
| `AddSpanProcessor(sp sdktrace.SpanProcessor)` | 挂一个上报用的处理器，在 `xone.Run` 之前调；初始化之后调的立即挂上。传 nil 直接 panic |
| `ForwardHeaderFromContext(ctx, key) string` | 取一个透传的 Header 值，大小写不敏感 |
| `ForwardHeadersFromContext(ctx) map[string]string` | 取全部透传的 Header（拷贝） |
| `Transport{Next}` | `http.RoundTripper`：把目标 Host 写进 ctx，`ForwardHeaderRules` 才能按域名生效。它自己不注入 Header，`Next` 要是会调全局 Propagator 的那一层（如 `otelhttp.NewTransport(...)`）。xhttp 已经包好，自己的 `http.Client` 才要用 |
| `WithTargetHost(ctx, host)` / `TargetHostFromContext(ctx)` | 不经 HTTP 时自己写 / 读目标 Host，作用同 `Transport` |
| `New(ctx, cfg, procs...) (*Tracing, io.Closer, error)` | 纯构造器：不碰全局，`(*Tracing).Install()` 才装成进程级的 |

## 注意事项

- **框架不内置任何 exporter**：不 `AddSpanProcessor` 的话 Span 照样生成、`trace_id` 照样进日志，只是不上报。本地调试开 `Console: true`。
- **透传 Header 和 `baggage` 只收可信对端的**：直连对端在 `XGin.TrustedProxies` / `XEcho.TrustedProxies` 里才收，默认只信私有网段
  （负载均衡、K8s 的 Ingress 和 Pod）。`traceparent` / `b3` 谁发来的都接。见
  [observability.md「传播与信任边界」](../docs/observability.md#传播与信任边界)。
- **有上游时一律听上游的 sampled 位**，`SampleRatio: 1` 也不例外（`ParentBased`，见[「行为与实测」](#行为与实测)）。
  `SampleRatio: 0` 是不采样但照常生成、透传 TraceID；连 Span 都不要用 `Enable: false`。
- **`service.name`** 取 `XApp.Name`，环境变量 `OTEL_RESOURCE_ATTRIBUTES` / `OTEL_SERVICE_NAME` 压过它。见[「行为与实测」](#行为与实测)。

## 可观测

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `xtrace ignored forward headers from an untrusted peer, …` | WARN | `header`；整个进程只打一次 |
| `xtrace ignored baggage from an untrusted peer, …` | WARN | 整个进程只打一次 |
| `xtrace is disabled, the registered SpanProcessors will receive no spans` | WARN | `count`、`switch`；`Enable: false` 却登记了处理器时 |
| `xtrace some resource attributes could not be detected, continuing without them` | WARN | `error`；照常启动 |

日志的全局约定（`trace_id` 注入、`xlog.AddKV`、框架的启停日志）见 [`docs/observability.md`](../docs/observability.md#日志)。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

OTel SDK v1.46.0、otelhttp v0.71.0。

**采样**：SDK 的 `AlwaysSample` 无视上游的 `sampled=00`，还把 `-01` 往下游传。这里一律用
`ParentBased(TraceIDRatioBased(SampleRatio))`：只有根 Span 按比例抽，有上游时照上游的 sampled 位。

**`service.name` 的优先级**，后面的压过前面的：OTel 自己的兜底名 `unknown_service:<可执行文件名>` →
`XApp.Name` / `XApp.Version` → `OTEL_RESOURCE_ATTRIBUTES` → `OTEL_SERVICE_NAME`。没配的那一项不写，
不会写进一个空的 `service.name=""` 把兜底名盖掉。

**resource 采集出错时只打一条告警、用采到的那部分继续**：`OTEL_RESOURCE_ATTRIBUTES` 写错一项、
或容器里以随机 UID 运行查不到当前用户，都不让服务起不来。

**透传只收可信对端的值。** OTel 自带的 `propagation.Baggage` 谁发来的都收：公网客户端发一个 `X-Tenant-Id`，
或者改写成 `baggage: tenant=…`，就被当成自己人给的、带进内网的每一次调用。规则见
[`observability.md`](../docs/observability.md#传播与信任边界)。

**`*trusted.com` 这类通配**：照字面后缀匹配的话会匹配 `eviltrusted.com`，一个谁都能注册的域名就拿到了内部令牌。
所以 `Domains` 只认 `api.internal.com` 和 `*.trusted.com` 两种写法。
