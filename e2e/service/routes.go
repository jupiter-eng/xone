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

// ping 就绪探测：harness 等它返回 200 才算服务起来了
func ping(c *gin.Context) { c.String(http.StatusOK, "pong") }

type loginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// login 请求体里带 password、token，响应体里带 session_token：
// 打开 LogRequestBody / LogResponseBody 时这三个值都不该出现在日志里
func login(c *gin.Context) {
	ctx := c.Request.Context()
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 业务日志只记用户名：凭证不进日志是业务自己的事，脱敏只兜访问日志
	slog.InfoContext(ctx, "login attempt", "username", req.Username)
	xmetric.CounterInc("logins_total")
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": req.Username, "session_token": "sess-" + randomHex(16)})
}

// proxy 经 xhttp 调下游 GET {Downstream}/echo?token=xxx。
//
// token 取自入站请求的 ?token=，没给就用一个固定值。它在出站 URL 的查询串里，
// 所以出站 Span 的 url.full、xhttp 的日志里都不该看到它。
// ?method=POST 换成别的方法调下游（测非幂等方法不重试），默认 GET。
// 拿到下游响应就原样转回（状态码、Content-Type、body），传输层出错返回 502。
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

// slow 等 ms 毫秒（默认 1000），期间看 ctx：连接被断开时提前收尾。
//
// 优雅退出时它应该被等到做完；超过服务那一段停止预算时连接被断开，
// 它看到 ctx 取消后记一条 "slow request cancelled" 返回
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

// stuck 用 time.Sleep 睡 ms 毫秒（默认 1000），故意不看 ctx。
//
// 这是文档里「框架停不下来的 handler」：断开连接、取消 ctx 都叫不醒它。
// db=1 / mysql=1 / ch=1 / redis=1 时睡完再查一次 PG、MySQL、ClickHouse，Ping 一次 Redis，用的 ctx 同样剥掉了取消——
// 看它是不是在框架关掉数据库、Redis 之后还在跑。
// e2e_stuck_inflight 是此刻还睡在里面的个数：测试据此确认请求已经进了 handler 再发信号
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

// boom 直接 panic，交给 xgin 的恢复中间件
func boom(*gin.Context) {
	panic("deliberate panic from /boom")
}

// upload 收一个 multipart 上传（字段名 file），回文件名、大小和 sha256。
//
// 访问日志不读 multipart 的请求体、只记一句 omitted；handler 这边读到的
// 仍该是完整的文件——测试拿 sha256 核对「记日志没有动请求体」
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

// queryInt 取一个整数查询参数，没给或写错时用默认值
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
