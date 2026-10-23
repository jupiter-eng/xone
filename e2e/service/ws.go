package main

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// The long-lived connection a service with a WebSocket has. The handler hijacks, which takes the
// connection out of net/http's hands: Shutdown does not wait for a hijacked connection and Close
// cannot break it either, so the one thing that ends this handler is the request ctx the framework
// cancels on its way out. xgin and xecho each have a unit test for that; what no unit test reaches
// is a real process answering a real signal with such a connection open.
//
// The upgrade is written by hand rather than with a WebSocket library: what is being tested is the
// hijack and the shutdown, and a real handshake would only add a dependency to e2e.
//
//	GET /ws   hijacks, answers 101, then writes a line every 100ms until the ctx is cancelled
func wsRoutes(e *gin.Engine) {
	e.GET("/ws", func(c *gin.Context) {
		conn, _, err := c.Writer.Hijack()
		if err != nil {
			c.String(http.StatusInternalServerError, "hijack: %v", err)
			return
		}
		defer conn.Close()
		const upgraded = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
		if _, err := io.WriteString(conn, upgraded); err != nil {
			return
		}

		ctx := c.Request.Context()
		for {
			select {
			case <-ctx.Done():
				// The line the shutdown case waits for: it says the handler left because the
				// framework cancelled the ctx, rather than because the client went away.
				slog.InfoContext(ctx, "ws handler left", "reason", ctx.Err().Error())
				return
			case <-time.After(100 * time.Millisecond):
				if _, err := io.WriteString(conn, "tick\n"); err != nil {
					return // the client is gone
				}
			}
		}
	})
}
