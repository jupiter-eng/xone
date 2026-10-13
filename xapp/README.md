# xapp

`XApp` 块写应用本身的事：叫什么、什么版本、默认跑在哪个环境、配置从哪些文件来。

- 链路的 `service.name` / `service.version`、接口文档的默认标题和版本都取自这里，只配一次
- 根包 `xone` 带着它，不用为了这块配置 import xapp
- `Profiles`：默认激活哪个环境的配置文件
- `Import`：再读哪些配置文件

## 快速上手

```yaml
# conf/application.yml
XApp:
  Name: shop.order.api
  Version: v1.2.0
  Profiles: ${APP_ENV:dev}    # 默认激活的 profile
  Import: [common/log.yml]    # 再读哪些文件
```

```go
slog.Info("build info", "app", xapp.Name(), "version", xapp.Version())
```

## 配置

```yaml
XApp:
  Name: shop.order.api     # 建议 team.system.app，默认空
  Version: v1.2.0          # 默认空
  Profiles: dev            # 默认激活的 profile：一个名字、"prod,eu" 或 [prod, eu]；只能写在主配置文件里
  Import:                  # 再读哪些文件：一个写字符串，多个写列表；optional: 前缀表示可以没有
    - common/log.yml
```

- `Name`、`Version`：环境文件里可以覆盖，比如 `application-prod.yml` 换一个 `Name`。
- `Profiles`：`--profile` / `XONE_PROFILE` 给了就不看这一项（替换，不是追加）。规则见
  [docs/config.md「Profiles」](../docs/config.md#profiles--按环境分文件)。
- `Import`：相对路径按写着它的那个文件所在的目录解析，引进来的压过引它的；被引进来的文件里可以再写 `Import`，
  但不能写 `Profiles`。规则见 [docs/config.md「Import」](../docs/config.md#import--引入别的配置文件)。

## API

| 函数 | 说明 |
|---|---|
| `Name() string` | 应用名，未配置时为空字符串 |
| `Version() string` | 应用版本，未配置时为空字符串 |

`XApp` 在日志那一档（`StageLog`）就读好，链路、客户端、业务、服务各档的钩子里读到的都是最终值。

## 注意事项

- **`Profiles`、`Import` 由配置加载本身读**，读完就从 `XApp` 里拿走，xapp 看到的只有 `Name`、`Version`。
- **指标不读这一块**：要在指标上区分应用，用 `XMetric.ConstLabels`（如 `app: shop.order.api`）。
- **环境变量压过这里**：`OTEL_RESOURCE_ATTRIBUTES` / `OTEL_SERVICE_NAME` 压过 `Name` / `Version`，优先级见
  [xtrace「行为与实测」](../xtrace/README.md#行为与实测)。
