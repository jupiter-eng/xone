// A minimal runnable example: the configuration file decides the behavior and main holds no wiring
// code.
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

	// An anonymous import is the whole of the "wiring": each package registers itself in its init,
	// and the framework decides by Stage which comes up first and which closes last. The order the
	// imports are written in affects nothing.
	// Logging, tracing and metrics come along with xgin and need no import of their own
	_ "github.com/jupiter-eng/xone/xcache"
	_ "github.com/jupiter-eng/xone/xhttp"
)

func main() {
	// xgin has four middlewares built in -- logging, tracing, metrics and panic recovery -- whose
	// order the framework takes care of and whose switches are in the XGin block of the configuration
	// file; /metrics is mounted by itself too
	xone.MustRun(xgin.New().WithRoutes(routes))
}

func routes(e *gin.Engine) {
	e.GET("/hello", func(c *gin.Context) {
		defer xmetric.Timer("hello_handle")()

		// The business code knows the standard library's slog and the native gin, and none of this
		// framework's packages. The log carries this request's trace_id by itself
		slog.InfoContext(c.Request.Context(), "request received", "path", c.Request.URL.Path)
		c.JSON(http.StatusOK, gin.H{"msg": "hello"})
	})

	e.GET("/boom", func(c *gin.Context) {
		panic("deliberate panic, to exercise the recover middleware")
	})
}
