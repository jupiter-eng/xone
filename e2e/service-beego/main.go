// service-beego: a beego application served by xbeego, for the cases in beego_test.go.
//
// Deliberately far smaller than service/, and not a second copy of it. What has to be proved in a
// real process is the part of xbeego no unit test reaches: that xone.MustRun(xbeego.New()) brings a
// beego application up at all, that the three beego globals it takes over (the router, the recovery
// slot in web.BConfig and the logs package) survive that in a real binary, that a real SIGTERM
// finishes the requests in flight, and that nothing the process writes falls outside JSON. The data
// integrations are service/'s business and are not repeated here.
//
// The routes are registered the way a beego application registers them -- package-level web.Get /
// web.Router on the global web.BeeApp -- because that being enough is the whole point of the
// module.
//
// Configured from the environment under the same names the e2e service uses:
//
//	E2E_PORT       XBeego.Port (required)
//	E2E_SPAN_FILE  where the spans go; empty writes none
//	E2E_LOG_FILE   XLog.File.Enable, with the directory in E2E_LOG_DIR
package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	beelogs "github.com/beego/beego/v2/core/logs"
	beeweb "github.com/beego/beego/v2/server/web"
	beectx "github.com/beego/beego/v2/server/web/context"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/e2e/internal/spanlog"
	"github.com/jupiter-eng/xone/xbeego"
	"github.com/jupiter-eng/xone/xtrace"
)

// userController is an ordinary beego controller: it logs with the request's ctx and answers JSON.
type userController struct{ beeweb.Controller }

func (c *userController) Get() {
	ctx := c.Ctx.Request.Context()
	id := c.Ctx.Input.Param(":id")
	slog.InfoContext(ctx, "get user", "user_id", id)
	c.Data["json"] = map[string]string{"id": id, "client_ip": xbeego.ClientIP(c.Ctx.Request)}
	_ = c.ServeJSON()
}

// teapotController answers through an error handler registered with web.ErrorHandler, which
// Controller.Abort reaches by panicking with the code as a string rather than with web.ErrAbort.
// It is how a beego application answers an error code, and taking beego's recovery over must not
// turn it into a 500.
type teapotController struct{ beeweb.Controller }

func (c *teapotController) Get() { c.Abort("418") }

func main() {
	if f := os.Getenv("E2E_SPAN_FILE"); f != "" {
		exp, err := spanlog.NewExporter(f)
		if err != nil {
			log.Fatal(err)
		}
		// Synchronous: a span lands on disk the moment it ends, so a test reading the file has
		// nothing to guess about how long to wait
		xtrace.AddSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))
	}

	beeweb.ErrorHandler("418", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("i am a teapot"))
	})

	beeweb.Get("/ping", func(ctx *beectx.Context) { _ = ctx.Output.Body([]byte("pong")) })
	beeweb.Router("/users/:id", &userController{})
	beeweb.Router("/teapot", &teapotController{})

	// Watches the request ctx, so a graceful shutdown lets it finish and a client that left ends it
	beeweb.Get("/slow", func(ctx *beectx.Context) {
		ms, _ := strconv.Atoi(ctx.Input.Query("ms"))
		if ms == 0 {
			ms = 1000
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			_ = ctx.Output.Body([]byte("slow done"))
		case <-ctx.Request.Context().Done():
			_ = ctx.Output.Body([]byte("cancelled"))
		}
	})

	// The panic value carries something that has to be masked on its way into the log
	beeweb.Get("/boom", func(*beectx.Context) { panic("boom dsn=postgres://u:hunter2@db/app") })

	// Context.Abort sets the status and then panics with the body, registered or not
	beeweb.Get("/forbid", func(ctx *beectx.Context) { ctx.Abort(http.StatusForbidden, "nope") })

	// Writes through beego's own logs package, which xbeego routes into slog. Left alone it goes to
	// the console as plain text, which is what checkOutput would catch
	beeweb.Get("/beego-log", func(ctx *beectx.Context) {
		beelogs.Warn("beego says %s", "hello")
		_ = ctx.Output.Body([]byte("logged"))
	})

	xone.MustRun(xbeego.New())
}
