// Package xginswagger 把 Swagger UI 挂到 Gin 上。
//
//	gx := xgin.New().WithRoutes(func(e *gin.Engine) {
//		xginswagger.Register(e, docs.SwaggerInfo)
//		registerBusinessRoutes(e)
//	})
//
// 单独一个 module，不并进 xgin：Swagger UI 会把整套前端资源编进二进制，
// 实测比只用 gin 多 27 个模块（GOWORK=off go list -m all：只 import gin 是 55 个第三方模块，只 import xginswagger 是 82 个）。文档是开发期的事，不该让每个线上服务都背上。
package xginswagger

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginswagger "github.com/swaggo/gin-swagger"
	"github.com/swaggo/swag"

	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XGinSwagger"

// route Swagger UI 的路由，*any 是 gin 的通配段，Swagger UI 要用它加载各种资源
const route = "/swagger/*any"

// Config Swagger 文档的元信息
//
// 这些值写在配置里而不是代码注释里，是因为它们随环境变：
// 联调环境和生产环境的 Host 不一样，而文档注解是编译进去的。
type Config struct {
	// Host 文档里显示的服务地址，如 api.example.com。默认取自请求。
	Host string `yaml:"Host"`

	// BasePath 所有接口的公共前缀，如 /api/v1。
	BasePath string `yaml:"BasePath"`

	// Title 文档标题。默认取 XApp.Name。
	Title string `yaml:"Title"`

	// Description 文档描述。
	Description string `yaml:"Description"`

	// Schemes 支持的协议，如 ["https"]。默认留空：沿用注解里的 @schemes，写了才覆盖。
	Schemes []string `yaml:"Schemes"`

	// URLPrefix Swagger UI 挂载路径的前缀，如 /internal。
	// 默认挂在 /swagger/*any。要么留空，要么以 / 开头、不以 / 结尾，否则启动失败。
	// gin 自己不挑（实测 v1.12.0：internal 和 /internal/ 都被规整成
	// /internal/swagger/*any，不 panic），但 URL() 是照原样拼的，
	// 拼出来的 internal/swagger/index.html 就和实际挂的地址对不上。
	URLPrefix string `yaml:"URLPrefix"`
}

// Validate 检查配置本身说不通的地方。
//
// xconfig.Unmarshal 解完配置文件里的 XGinSwagger 块会调它，配错的值在读配置时就失败。
func (c Config) Validate() error {
	if c.URLPrefix != "" && (!strings.HasPrefix(c.URLPrefix, "/") || strings.HasSuffix(c.URLPrefix, "/")) {
		return fmt.Errorf("URLPrefix must start with / and must not end with /, got=%q", c.URLPrefix)
	}
	return nil
}

// DefaultConfig 全部默认值集中在这里。
//
// 每一项都留空：没配的一律沿用注解，见 fill。默认值是预填进结构体的，
// 这里给了值就等于配置里写了——Schemes 原先默认 ["https", "http"]，
// 注解里的 @schemes 于是永远被盖掉
func DefaultConfig() Config {
	return Config{}
}

func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageServer))
}

// loadConfig 在启动阶段把这一块读一遍：认领它，并让配错的值在这里就失败。
// 读出来不存，Register 和 URL 用的时候自己读，读到的是同一份最终值
func loadConfig(context.Context) error {
	_, err := fileConfig()
	return err
}

// fileConfig 配置文件里的 XGinSwagger 块，没写的字段是默认值。
//
// 在 Start 之前任何时候调都行：配置文件第一次读的时候才加载，读到的永远是最终值——
// 在 main 顶上、xone.Run 之前就装配 engine 也一样。
// xconfig.Unmarshal 解完会调 Validate；解不出来或者不合法时返回默认值和那个错误
func fileConfig() (Config, error) {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return DefaultConfig(), err
	}
	return c, nil
}

// Register 把 Swagger UI 挂到 engine 上，并用配置填好文档元信息。
//
// info 通常是 swaggo 生成的 docs.SwaggerInfo。传 nil 表示只挂 UI 不填元信息。
//
// 配置错了就按默认值挂：那个错误由启动钩子报出来，服务起不来。
//
// 元信息在第一次访问文档时才填，不在这里填：标题和版本默认取自 xapp，
// 而 xapp 要等 xone.Run 的启动钩子才读好。Register 早于它的话，这里读到的
// 是空的——文档照样打得开，只是标题不对。第一个请求一定在服务起来之后。
func Register(e *gin.Engine, info *swag.Spec) {
	if e == nil {
		return
	}
	c, _ := fileConfig()

	serve := ginswagger.WrapHandler(swaggerfiles.Handler)
	if info != nil {
		// Once 而不是每次都填：info 是 swag 全局登记的那一份，
		// 并发的请求同时写它就是数据竞争
		var once sync.Once
		inner := serve
		serve = func(ctx *gin.Context) {
			once.Do(func() { fill(info, c) })
			inner(ctx)
		}
	}
	e.GET(c.URLPrefix+route, serve)
}

// URL 返回 Swagger UI 的实际访问路径，方便打日志或做跳转
func URL() string {
	c, _ := fileConfig()
	return c.URLPrefix + "/swagger/index.html"
}

// fill 把配置写进 swaggo 生成的元信息
//
// 只覆盖配了的字段：没配就保留注解里写的那份，
// 否则一个空的配置块会把文档标题清成空白。
func fill(info *swag.Spec, c Config) {
	if c.Host != "" {
		info.Host = c.Host
	}
	if c.BasePath != "" {
		info.BasePath = c.BasePath
	}
	if c.Description != "" {
		info.Description = c.Description
	}
	if len(c.Schemes) > 0 {
		info.Schemes = c.Schemes
	}

	// 标题和版本默认跟 App 走：同一个事实配两遍迟早会不一致
	switch {
	case c.Title != "":
		info.Title = c.Title
	case xapp.Name() != "":
		info.Title = xapp.Name()
	}
	if v := xapp.Version(); v != "" {
		info.Version = v
	}
}
