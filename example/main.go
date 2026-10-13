// 一个最小的可运行示例：配置文件决定行为，main 里没有装配代码。
//
//	cd example && go run . --config=application.yml
package main

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xmetric"

	// 匿名 import 就是全部「装配」：各包在 init 里登记自己，
	// 框架按 Stage 决定谁先起、谁后关。import 的书写顺序不影响任何事。
	// 日志、链路、指标跟着 xgin 一起来，不用另外 import
	_ "github.com/jupiter-eng/xone/xcache"
	_ "github.com/jupiter-eng/xone/xhttp"
)

func main() {
	// xgin 内置了日志、链路、指标、panic 恢复四个中间件，
	// 顺序由框架管，开关在配置文件的 XGin 块里；/metrics 也是自动挂上的
	xone.MustRun(xgin.New().WithRoutes(routes))
}

func routes(e *gin.Engine) {
	e.GET("/hello", func(c *gin.Context) {
		defer xmetric.Timer("hello_handle")()

		// 业务代码只认识标准库 slog 和原生 gin，不认识本框架的包。
		// 日志会自动带上这次请求的 trace_id
		slog.InfoContext(c.Request.Context(), "request received", "path", c.Request.URL.Path)
		c.JSON(http.StatusOK, gin.H{"msg": "hello"})
	})

	e.GET("/boom", func(c *gin.Context) {
		panic("deliberate panic, to exercise the recover middleware")
	})
}
