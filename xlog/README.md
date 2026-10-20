# xlog

日志：装好之后 `slog.Default()` 就是按 `XLog` 配好的原生 `*slog.Logger`，业务代码直接用标准库 `log/slog`。

- 核心模块，零依赖，只跟着 xgin / xecho / xcron 一起来：用了它们就装好了；不写配置就是 info 级别的 JSON 打到标准输出
- 根包 `xone` 不带它。只用数据类集成（xgorm、xredis、xhttp、xcache）或 xtrace、xmetric 的程序不会被装上 xlog，
  `slog.Default()` 保持原样；想用就 `import _ "github.com/jupiter-eng/xone/xlog"`。没 import 却写了 `XLog` 块，启动失败并提示这一行
- 有链路时每条日志自动带 `trace_id` / `span_id`
- `xlog.AddKV` 给整个请求加字段（访问日志也带上），`xlog.CtxWithKV` 只给一段调用加
- 文件输出按时间轮转、按保留时长清理，只删自己命名的文件
- 时间戳可按指定时区渲染

## 快速上手

```yaml
# conf/application.yml（可选）
XLog:
  Level: info
  File: {Enable: true, Path: /var/log/app}
```

```go
import (
	"context"
	"log/slog"

	"github.com/jupiter-eng/xone/xlog"
)

func Create(ctx context.Context, userID string, orders []Order) {
	xlog.AddKV(ctx, "user_id", userID) // 同一请求之后的每条日志（包括访问日志）都带着它

	for _, o := range orders {
		ctx := xlog.CtxWithKV(ctx, map[string]any{"order_id": o.ID}) // 只有这一轮的日志带着它
		slog.InfoContext(ctx, "order created", "amount", o.Amount)
	}
}
```

一律用带 ctx 的 `slog.InfoContext`，才带得上 `trace_id` 和上面这些字段。
日志的 message 和字段名建议用英文：它们会变成日志平台里的检索词和 JSON key。

## 配置

```yaml
XLog:
  Level: info              # debug / info / warn / error，默认 info
  Format: json             # json / text，默认 json
  AddSource: false         # 记代码位置，有开销，默认关
  Timezone: ""             # 时间戳按哪个 IANA 时区渲染，如 Europe/Berlin；空 = 进程本地时区
  Console: true            # 打到标准输出，默认开
  File:
    Enable: false          # 默认关
    Path: ""               # 目录，默认空 = 进程的工作目录；不存在时自动创建
    Name: app.log          # 默认 app.log；实际文件带时间后缀，另有同名符号链接指向当前文件
    RotateTime: 24h        # 轮转周期，按本地时区对齐，默认一天，至少 1m
    MaxAge: 168h           # 历史保留时长，默认 7 天；0 = 不清理，负数启动失败
    Perm: "0644"           # 按八进制解析的字符串，默认 "0644"，644 / 0o644 也认
  Fields: {}               # 每条日志都带的静态字段，值可以用 ${VAR}；见下文「每条日志都带的字段」
```

- 文件名后缀随 `RotateTime` 的粒度：≥ 24h 是 `app.log.20260918`，≥ 1h 是 `app.log.2026091815`，更短是 `app.log.202609181504`。
  最细到分钟，所以短于 1m 启动失败（0s 实际每分钟一个文件，30s 两个周期落在同一个文件名上）。
- 清理启动时一次、之后每次轮转一次，只删 `app.log.<时间后缀>`；`app.log.bak`、`app.log.1.gz` 不碰。

## 每条日志都带的字段

框架默认给每条日志带上「这条是谁打的」，和 xtrace 的 Span 上的 `service.name`、`host.name`、`process.pid` 对得上：

| 字段 | 来源 | 说明 |
|---|---|---|
| `service` | `XApp.Name` | 没配就不写 |
| `version` | `XApp.Version` | 没配就不写 |
| `hostname` | `os.Hostname()` | 虚拟机上是机器名，K8s 里就是 Pod 名。不叫 `host`：xgin 访问日志的 `host` 是请求的 Host 头 |
| `pid` | `os.Getpid()` | 同一台机器上的多个实例、重启前后分得开 |

部署环境才知道的（Pod、节点、命名空间、机房、Pod IP）用 `XLog.Fields` 从环境变量注入。框架不去猜：
一台机器常有好几块网卡，自动挑的 IP 可能是错的，而且看起来像是对的。

```yaml
XLog:
  Fields:
    pod_ip: ${POD_IP:}          # ${VAR:} 没设时是空串，空串的字段不写
    node: ${NODE_NAME:}
    namespace: ${POD_NAMESPACE:}
    hostname: ""                # 同名的以这里为准，写成空串就是不要这个默认字段
```

```yaml
# K8s：用 Downward API 把这些值放进环境变量
env:
  - {name: POD_IP, valueFrom: {fieldRef: {fieldPath: status.podIP}}}
  - {name: NODE_NAME, valueFrom: {fieldRef: {fieldPath: spec.nodeName}}}
  - {name: POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
```

- 日志写 stdout、由 Fluent Bit / Filebeat / Vector 收的，采集组件多半已经附上了 Pod、命名空间、节点，不必再配。
- 这些值启动时算一次、序列化一次，开销见[「行为与实测」](#行为与实测)。
- `UseHandler` 换了后端照样带上；`New` 是纯构造器，只带 `cfg.Fields`，不带默认的 4 个。

## API

| 函数 | 说明 |
|---|---|
| `AddKV(ctx, k, v)` / `AddKVs(ctx, map[string]any)` | 往当前作用域原地写字段，同一请求之后的每条日志都带上 |
| `CtxWithKV(ctx, map[string]any) context.Context` | 派生一个带这些字段的新 ctx，只有用它写的日志带着；不影响父 ctx 和兄弟 |
| `CtxWithScope(ctx) context.Context` | 开一个字段作用域，非 Web 入口（消费一条消息、跑一次任务）用；已有作用域时原样返回 |
| `DroppedKVCount() int64` | 因为没有作用域被丢掉的 `AddKV` 字段数，不为零多半是漏了 `CtxWithScope` |
| `Location() *time.Location` | 生效中的时区：`t.In(xlog.Location()).Format(time.RFC3339)`；没配时是 `time.Local` |
| `TraceIDs(ctx) (traceID, spanID string)` | 当前 ctx 的链路标识，就是日志里写的那两个值；没有时是两个空串 |
| `UseHandler(h slog.Handler)` | 日志改由你自己的 handler 写（zap 的 slog 桥、公司的日志 SDK……），在 `xone.Run` 之前调；见[「用自己的日志后端」](#用自己的日志后端) |
| `New(cfg) (*slog.Logger, io.Closer, error)` | 纯构造器：不碰全局、不读配置文件，离开框架也能用 |

`SetTraceExtractor` / `AddObserver`（及类型 `TraceExtractor`、`Observer`）是给集成注入能力的扩展点，业务代码用不到；
xtrace、xmetric 走的是同一处注入点（`internal/logext`），不 import xlog。

## 用自己的日志后端

日志的入口永远是 `slog`；要换的只是最后由谁来写。在 `xone.Run` 之前把 handler 交给 xlog：

```go
func main() {
	xlog.UseHandler(zapslog.NewHandler(core)) // 或任何 slog.Handler
	xone.MustRun(xgin.New().WithRoutes(routes))
}
```

- xlog 照样把它包一层再装成 `slog.Default()`：`trace_id`、`AddKV` / `CtxWithKV` 的字段、错误日志计数、`Fields` 都还在，
  框架的访问日志、SQL 日志、启停日志也写进它。
- 级别、格式、输出去向都由你的 handler 决定，`XLog` 里除了 `Fields` 一项都不起作用：**写了就启动失败**
  （`XLog has no effect when xlog.UseHandler is set`），免得以为 `Level: debug` 生效了。只写默认值不算冲突。
- 你的 handler 归你管：退出时 xlog 不关它，也不把 `slog.Default()` 换掉。
- 日志装好之后再调不会生效，只打一条 WARN——那之前的日志已经写到别处了。

## 注意事项

- **用 `slog.InfoContext(ctx, …)`**：不带 ctx 的 `slog.Info` 拿不到 `trace_id`，也带不上 `AddKV` / `CtxWithKV` 的字段。
- **`AddKV` 要有作用域**：xgin / xecho 在每个请求开头开好（`Log: false` 时不开）；自己的非 Web 入口用 `xlog.CtxWithScope(ctx)` 开，否则字段被丢掉（计进 `DroppedKVCount`）。
  见 [observability.md「日志」](../docs/observability.md#logs)。
- **`AddKV` 和 `CtxWithKV` 的分工在影响范围**：`AddKV` 原地写，整个请求都带上；`CtxWithKV` 只影响它返回的 ctx，
  批量处理的每一条、起的每个 goroutine 各派生一个，互相不串，也不回流到访问日志。派生之后父 ctx 再 `AddKV` 的字段它看不到。
- **`Timezone` 配了却加载不到直接启动失败**，不会悄悄退回本地时区；scratch / distroless 镜像要 `import _ "time/tzdata"`，见[「行为与实测」](#行为与实测)。
- **`Name` 的位置上已经有一个不是符号链接的文件**时启动失败，不会把旧日志覆盖掉。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

**`Perm`**：yaml.v3 把数字 `0644` 解析成 420（对的），`644` 却是十进制 644 = 0o1204（`--w----r-T`），不报错。所以用字符串、一律按八进制解析。

**`RotateTime`** 的对齐和文件名后缀都按进程本地时区，不跟 `Timezone`（它只管日志里的时间戳）。

**`Timezone`**：scratch / distroless 镜像里没有 `/usr/share/zoneinfo`，要在自己的 `main` 包加一行 `import _ "time/tzdata"`（约 400KB）。
框架不替你编：没用到这项的人不该背这 400KB。

**默认字段的开销**：实测默认的 4 个（hostname 是 Pod 名那么长时）每行多 88 字节，时间和分配量不出差别
（`BenchmarkHandle_WithIdentityFields` 对照 `BenchmarkHandle_Bare`）。
