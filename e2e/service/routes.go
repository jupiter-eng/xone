package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xhttp"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xredis"
)

func routes(e *gin.Engine) {
	e.GET("/ping", ping)
	e.POST("/users", createUser)
	e.GET("/users/:id", getUser)
	e.PUT("/users/:id", updateUser)
	e.POST("/login", login)
	e.GET("/proxy", proxy)
	e.POST("/orders", createOrder)
	e.GET("/slow", slow)
	e.GET("/stuck", stuck)
	e.GET("/boom", boom)
	e.POST("/upload", upload)
	e.GET("/dep", dep)
	mysqlRoutes(e)
	clickhouseRoutes(e)
	kafkaRoutes(e)
}

// ping is the readiness probe: the harness waits for a 200 from it before the service counts as up
func ping(c *gin.Context) { c.String(http.StatusOK, "pong") }

type loginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// login carries password and token in the request body and session_token in the response body:
// with LogRequestBody / LogResponseBody on, none of the three should appear in the log
func login(c *gin.Context) {
	ctx := c.Request.Context()
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// The business log records the username alone: keeping credentials out of the log is the business's own job, and the redaction only backs up the access log
	slog.InfoContext(ctx, "login attempt", "username", req.Username)
	xmetric.CounterInc("logins_total")
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": req.Username, "session_token": "sess-" + randomHex(16)})
}

// proxy calls the downstream GET {Downstream}/echo?token=xxx through xhttp.
//
// token comes from the inbound request's ?token= and falls back to a fixed value. It sits in the
// outbound URL's query string, so neither the outbound span's url.full nor xhttp's log should hold
// it.
// ?method=POST calls the downstream with another method (to test that a non-idempotent method is
// not retried), GET by default.
// The downstream's response is passed back as it is (status code, Content-Type, body), and a
// transport error answers 502.
func proxy(c *gin.Context) {
	ctx := c.Request.Context()
	base := conf.C().Downstream
	if base == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "downstream is not configured"})
		return
	}
	token := c.DefaultQuery("token", "e2e-downstream-token")
	method := c.DefaultQuery("method", http.MethodGet)
	resp, err := xhttp.R(ctx).SetQueryParam("token", token).Execute(method, base+"/echo")
	if err != nil {
		slog.WarnContext(ctx, "downstream call failed", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	xmetric.CounterInc("downstream_calls_total", xmetric.T("status", strconv.Itoa(resp.StatusCode())))
	c.Data(resp.StatusCode(), resp.Header().Get("Content-Type"), resp.Body())
}

// slow waits ms milliseconds (1000 by default) while watching ctx: it winds down early when the
// connection breaks.
//
// A graceful exit should wait for it to finish; past the service's share of the stop budget the
// connection is broken, it sees the cancelled ctx, records a "slow request cancelled" and returns
func slow(c *gin.Context) {
	defer xmetric.TrackInFlight("slow_inflight")()
	ctx := c.Request.Context()
	ms := queryInt(c, "ms", 1000)

	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		slog.InfoContext(ctx, "slow request finished", "ms", ms)
		c.JSON(http.StatusOK, gin.H{"slept_ms": ms})
	case <-ctx.Done():
		slog.InfoContext(ctx, "slow request cancelled", "ms", ms, "error", ctx.Err().Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": ctx.Err().Error()})
	}
}

// stuck sleeps ms milliseconds (1000 by default) with time.Sleep and deliberately does not watch
// ctx.
//
// This is the documentation's "handler the framework cannot stop": neither breaking the connection
// nor cancelling ctx wakes it.
// With db=1 / mysql=1 / ch=1 / redis=1 it queries PG, MySQL and ClickHouse once more and Pings
// Redis after the sleep, with a ctx whose cancellation is stripped as well -- to see whether it is
// still running after the framework has closed the databases and Redis.
// e2e_stuck_inflight is how many are sleeping inside it right now: a test uses it to make sure the
// request has reached the handler before it sends a signal
func stuck(c *gin.Context) {
	defer xmetric.TrackInFlight("stuck_inflight")()
	ctx := c.Request.Context()
	ms := queryInt(c, "ms", 1000)
	time.Sleep(time.Duration(ms) * time.Millisecond)

	attrs := []any{"ms", ms}
	if c.Query("db") == "1" {
		dbErr := "none"
		if err := store.Ping(context.WithoutCancel(ctx)); err != nil {
			dbErr = err.Error()
		}
		attrs = append(attrs, "db_error", dbErr)
	}
	if c.Query("mysql") == "1" {
		myErr := "none"
		if err := store.PingMySQL(context.WithoutCancel(ctx)); err != nil {
			myErr = err.Error()
		}
		attrs = append(attrs, "mysql_error", myErr)
	}
	if c.Query("ch") == "1" && store.HasCH() {
		chErr := "none"
		if err := store.PingCH(context.WithoutCancel(ctx)); err != nil {
			chErr = err.Error()
		}
		attrs = append(attrs, "ch_error", chErr)
	}
	if c.Query("redis") == "1" {
		redisErr := "none"
		if err := xredis.C().Ping(context.WithoutCancel(ctx)).Err(); err != nil {
			redisErr = err.Error()
		}
		attrs = append(attrs, "redis_error", redisErr)
	}
	slog.InfoContext(ctx, "stuck request finished", attrs...)
	c.JSON(http.StatusOK, gin.H{"slept_ms": ms})
}

// boom panics outright, which is left to xgin's recovery middleware
func boom(*gin.Context) {
	panic("deliberate panic from /boom")
}

// upload takes one multipart upload (the field file) and answers the file name, the size and the
// sha256.
//
// The access log does not read a multipart request body and records one omitted instead; what
// reaches the handler should still be the whole file -- a test checks the sha256 to see that
// logging did not touch the request body
func upload(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"filename": fh.Filename, "size": n, "sha256": hex.EncodeToString(h.Sum(nil))})
}

// queryInt takes an integer query parameter, and falls back to the default when it is missing or malformed
func queryInt(c *gin.Context, key string, def int) int {
	if v, err := strconv.Atoi(c.Query(key)); err == nil && v >= 0 {
		return v
	}
	return def
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
