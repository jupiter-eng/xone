// service-echo: an echo application served by xecho, for the cases in echo_test.go.
//
// Deliberately far smaller than service/, and not a second copy of it. What has to be proved in a
// real process is the part of xecho no unit test reaches: that xone.MustRun(xecho.New()) brings an
// echo application up at all, that a real SIGTERM finishes the requests in flight, that echo's two
// loggers really end up inside slog in a real binary -- echo writes through its own Logger and a
// std *log.Logger, and a line of either going to the console as plain text is what checkOutput
// catches -- and that the layers echo has and the other two do not (Pre, HTTPErrorHandler) still
// leave a complete access log and span behind. The data integrations are service/'s business and
// are not repeated here.
//
// Configured from the environment under the same names the other e2e services use:
//
//	E2E_PORT       XEcho.Port (required)
//	E2E_SPAN_FILE  where the spans go; empty writes none
//	E2E_LOG_FILE   XLog.File.Enable, with the directory in E2E_LOG_DIR
package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/e2e/internal/spanlog"
	"github.com/jupiter-eng/xone/xecho"
	"github.com/jupiter-eng/xone/xtrace"
)

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

	xone.MustRun(xecho.New().WithRoutes(routes))
}

func routes(e *echo.Echo) {
	// The readiness probe the harness waits for
	e.GET("/ping", func(c echo.Context) error { return c.String(http.StatusOK, "pong") })

	// A version prefix, which is how an echo application lays an API out: the route label has to
	// come out whole
	v1 := e.Group("/api/v1")
	v1.GET("/users/:id", func(c echo.Context) error {
		ctx := c.Request().Context()
		id := c.Param("id")
		slog.InfoContext(ctx, "get user", "user_id", id)
		// RealIP goes through the IPExtractor xecho installs from XEcho.TrustedProxies, not
		// through echo's default one, which believes every X-Forwarded-For it is given
		return c.JSON(http.StatusOK, map[string]string{"id": id, "client_ip": c.RealIP()})
	})

	// Watches the request ctx, so a graceful shutdown lets it finish and a client that left ends it
	e.GET("/slow", func(c echo.Context) error {
		ms, _ := strconv.Atoi(c.QueryParam("ms"))
		if ms == 0 {
			ms = 1000
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			return c.String(http.StatusOK, "slow done")
		case <-c.Request().Context().Done():
			return c.String(http.StatusOK, "cancelled")
		}
	})

	// The panic value carries something that has to be masked on its way into the log
	e.GET("/boom", func(echo.Context) error { panic("boom dsn=postgres://u:hunter2@db/app") })

	// An *echo.HTTPError goes to echo's HTTPErrorHandler, which renders the response after the
	// handler has returned. The access log has to record what that handler really sent, not the
	// 200 the response had before it ran
	e.GET("/teapot", func(echo.Context) error {
		return echo.NewHTTPError(http.StatusTeapot, "i am a teapot")
	})

	// Writes through echo's own Logger and through the std *log.Logger echo hands out. Left alone
	// both go to the console as plain text, which is what checkOutput would catch.
	//
	// Error rather than Warn: gommon's level is ERROR unless XEcho.Debug raises it (echo raises it
	// from e.Debug inside e.Start alone, which xecho does not use, so xecho sets it itself), and a
	// Warn here would be dropped before it ever reached slog. That gommon level is xecho's own
	// unit test's business (TestDebug_RaisesEchoLoggerLevel); what this endpoint is for is the
	// routing holding inside a real binary.
	e.GET("/echo-log", func(c echo.Context) error {
		c.Echo().Logger.Error("echo says hello")
		c.Echo().StdLogger.Print("echo std says hello")
		return c.String(http.StatusOK, "logged")
	})

	// Pre runs before the router, which no other integration has. A path rewritten there still has
	// to route, and the request still has to reach the access log, the metrics and a span
	e.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if r := c.Request(); strings.HasPrefix(r.URL.Path, "/old/") {
				r.URL.Path = "/api/v1/users/" + strings.TrimPrefix(r.URL.Path, "/old/")
			}
			return next(c)
		}
	})
}
