# xconfig

把配置文件里你自己的那一块解进一个普通的 Go 结构体（核心模块），规则和框架自己的配置块一样。

- 默认值预填在结构体里，文件里没写的字段保持不变
- 写错就启动失败：不认识的字段、`${VAR}` 没设、时长写成裸数字
- `${VAR}` 照样展开，`${VAR:default}` 可选
- 结构体实现了 `Validate() error` 的，解完自动调一次
- 什么时候读都行：第一次读时才加载配置文件，读到的永远是最终值

## 快速上手

```yaml
# conf/application.yml
Order:
  PayTimeout: 30m                        # 时长写单位：30s / 1500ms / 1h30m
  CallbackURL: "${ORDER_CALLBACK_URL}"   # 必填的环境变量，没设就启动失败
```

```go
type OrderConfig struct {
	PayTimeout  time.Duration `yaml:"PayTimeout"` // 每个字段都写 yaml tag
	MaxItems    int           `yaml:"MaxItems"`
	CallbackURL string        `yaml:"CallbackURL"`
}

// Validate 解完自动调一次：配错的值在启动时就失败，一个请求都还没接
func (c OrderConfig) Validate() error {
	if c.MaxItems <= 0 {
		return errors.New("MaxItems must be > 0")
	}
	return nil
}

// main 里、xone.Run 之前
cfg := OrderConfig{PayTimeout: 15 * time.Minute, MaxItems: 50} // 默认值预填
if err := xconfig.Unmarshal("Order", &cfg); err != nil {
	log.Fatal(err)
}
```

`MaxItems` 没写，保持默认的 50；`PayTimeout` 被文件改成 30 分钟。

## API

| 函数 | 说明 |
|---|---|
| `Unmarshal(key string, into any) error` | 把 `key` 那一块解进 `into`。整块没写时 `into` 原样不动、返回 nil |
| `Has(key string) bool` | 文件里有没有写这一块，用于「配了才做」。问过就算认领 |
| `UnmarshalClients[C any](key string, defaults func() C) (map[string]C, error)` | 解「单实例 / 多实例」两种写法的块，返回 `map[名字]配置`，每个实例先铺默认值。主要给写集成的人用 |
| `DecodeStrict(node *yaml.Node, v any) error` | 在集合元素自己的 `UnmarshalYAML` 里用，保留「字段拼错就失败」的严格检查 |
| `DefaultClientName` | 单实例写法规整成的实例名，即 `"default"` |

测试里换一份配置用 `xonetest.UseConfigYAML(t, yml)`，见 [guide.md「测试」](../docs/guide.md#测试)。

## 注意事项

- **每个字段都写 `yaml` tag。** 没写 tag 的字段 yaml.v3 只认全小写的 key，`PayTimeout:` 会被当成不认识的字段、启动失败，
  报错里提示加 tag，见 [troubleshooting.md](../docs/troubleshooting.md#field-bogus-not-found-in-type-xginconfig)。
- **在 `Start` 之前读**：`main` 里、`xone.Run` 之前，或者一个 `BeforeStart` 钩子里（见[下文](#在钩子里读)）。
  只在 `Start` 里才读的不算：启动钩子跑完时没人读过的顶层 key 让启动失败（`config keys [Order] are not read by anyone`）。
- **时长写单位**：裸数字 `30` 直接报错（``cannot unmarshal !!int `30` into time.Duration``），不会被悄悄当成 30 纳秒。
- **区分「没配」和「配了」用 `xconfig.Has("Order")`**，不必用指针字段。
- 在 `main` 里读的好处：你的类型不必认识 `xconfig`，测试里直接传值。

## 在钩子里读

配置要在好几个包里用、或者读完还要做点初始化的，放进一个启动钩子。不写档位就是 `StageBusiness`：
所有客户端都已就绪、服务还没接流量。

```go
var conf = Config{PayTimeout: 15 * time.Minute}

func init() {
	xhook.BeforeStart(func(ctx context.Context) error { // init 里只登记，由 xone.Run 按档位执行
		return xconfig.Unmarshal("Order", &conf)
	})
}

// C 在 Start、请求处理里用：那时启动钩子已经跑完
func C() Config { return conf }
```

可运行的样例：[`example/component/conf/`](../example/component/conf/)（在 `main` 里读）、[`example/consumer/`](../example/consumer/)。

## 配置文件的几条规则

一行一条，细节都在 [`docs/config.md`](../docs/config.md)：

| 想要 | 写法 | 详见 |
|---|---|---|
| 从环境变量取值 | `${VAR}` 必填，未设置启动失败；`${VAR:default}` 可选 | [占位符](../docs/config.md#占位符) |
| 按环境分文件 | `application-prod.yml` 放差异，`--profile=prod` 或 `XONE_PROFILE=prod` 选 | [Profiles](../docs/config.md#profiles--按环境分文件)、[完整例子](../docs/config.md#多环境配置一个完整的例子) |
| 把配置拆成几个文件 | `Import: [shared.yml, optional:local.yml]` | [Import](../docs/config.md#import--引入别的配置文件) |
| 指定配置文件 | `--config=<path>` 或 `XONE_CONFIG`；不指定就找 `conf/application.yml` 等约定路径 | [文件位置](../docs/config.md#文件位置与优先级) |
| 知道几个文件怎么叠 | map 递归合并，列表整体替换，标量覆盖 | [合并规则](../docs/config.md#合并规则) |

## 进阶

- **map / 切片的元素也要默认值**：给元素类型写 `UnmarshalYAML`，先铺默认值、再用 `xconfig.DecodeStrict` 解
  （不能用 `node.Decode`，它会丢掉严格检查）：

  ```go
  func (r *Rule) UnmarshalYAML(n *yaml.Node) error {
  	*r = DefaultRule()
  	type raw Rule // 换个类型，否则这里会递归调用自己
  	return xconfig.DecodeStrict(n, (*raw)(r))
  }
  ```

- **单实例 / 多实例两种写法**（像 XGorm 那样）：`UnmarshalClients` 看有没有 `Clients` 决定按哪种解，两种混着写是错误。见
  [guide.md「写一个自己的集成」](../docs/guide.md#写一个自己的集成)。
