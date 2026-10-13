# xginswagger

接口文档：把 [swag](https://github.com/swaggo/swag) 生成的文档以 Swagger UI 挂到 xgin 的原生 `*gin.Engine` 上。

- 单独一个 module：UI 资源会编进二进制，不该让每个线上服务都背上
- 没写的字段沿用注解里的值，写了才覆盖；标题、版本默认取自 `XApp`
- `Host` 这类随环境变的值写在配置里：联调和生产的地址不一样，而注解是编译进去的
- 挂载前缀可配，写错启动失败

## 快速上手

先 `swag init` 从注解生成 `docs` 包，然后在路由里注册：

```go
import (
	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xginswagger"

	"example.com/shop/docs" // swag init 生成的
)

func main() {
	xone.MustRun(xgin.New().WithRoutes(func(e *gin.Engine) {
		xginswagger.Register(e, docs.SwaggerInfo) // 默认挂在 /swagger/*any
		// ……业务路由
	}))
}
```

打开 `http://localhost:8080/swagger/index.html`（实际路径是 `xginswagger.URL()`）。`XGinSwagger` 配置块可以不写。

## 配置

```yaml
XGinSwagger:
  Host: api.example.com    # 文档里显示的地址；默认沿用注解的 @host，注解也没写时 Swagger UI 用当前请求的地址
  BasePath: /api/v1        # 所有接口的公共前缀；默认沿用注解的 @BasePath
  Title: ""                # 默认取 XApp.Name
  Description: ""
  Schemes: []              # 默认留空：沿用注解里的 @schemes，写了才覆盖
  URLPrefix: ""            # UI 挂载路径前缀，默认挂在 /swagger/*any；以 / 开头、不以 / 结尾
```

- 没写的字段沿用注解里的值，写了才覆盖。标题和版本例外，按这个顺序取第一个有值的：
  - 标题：`XGinSwagger.Title` → `XApp.Name` → 注解里的 `@title`
  - 版本：`XApp.Version` → 注解里的 `@version`（`XGinSwagger` 没有版本字段）

## API

| 函数 | 说明 |
|---|---|
| `Register(e *gin.Engine, info *swag.Spec)` | 把 Swagger UI 挂到 engine 上，`info` 通常是 `docs.SwaggerInfo`；传 nil 只挂 UI、不填元信息 |
| `URL() string` | Swagger UI 的实际访问路径（带上 `URLPrefix`），方便打日志或做跳转 |
| `DefaultConfig() Config` | 默认值：每一项都留空，没配的一律沿用注解 |

## 注意事项

- `Register` 在 `Start` 之前任何时候调都行，读到的是最终配置。
- `URLPrefix` 要么留空，要么以 `/` 开头、不以 `/` 结尾，写错启动失败（`Register` 先按默认值挂，错误由启动钩子报出来）。
- 元信息在第一次访问文档时才填，因为 `XApp` 要等 `xone.Run` 的启动钩子才读好。
