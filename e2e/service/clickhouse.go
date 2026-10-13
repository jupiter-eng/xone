package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xmetric"
)

// ClickHouse 上的接口：第三个 xgorm 实例 xgorm.C("ch")，经 xgorm/clickhouse 方言、native 协议。
// 实例只在 harness 激活 ch 那份 profile 时才有（service/application-ch.yml），没有时一律 503。
//
//	POST /ch/events              {"name": "...", "values": [1, 2]} 一条 INSERT 写一批，201 回 ids
//	GET  /ch/events/:id          按主键点查，200 / 404
//	GET  /ch/stats?name=x        按 name 聚合 count() / sum(value)，走 GORM 的 Scan
//	GET  /ch/sleep?ms=N          服务端 SELECT sleep(N/1000)（服务端上限 3s），用请求的 ctx
//	GET  /ch/echo?tag=x&ms=N     服务端在标量子查询里睡 N 毫秒再回 tag：表头之前一个字节都不回
//	GET  /ch/stream?rows=N&ms=M  一行一个数据块、每行之间睡 M 毫秒：表头之后才慢下来
//	GET  /ch/version             Dialector 上记下的版本号和两个按版本设的开关
//
// sleep / echo / stream 另收 &timeout=200ms：用 context.WithTimeout 包住请求的 ctx（「调用方给了截止时间」），
// 不给就是请求的 ctx 本身。成功时 body 里都有 elapsed_ms，失败 503 另有 error：故障测试据此量「这一下要多久」
func clickhouseRoutes(e *gin.Engine) {
	g := e.Group("/ch", requireCH)
	g.POST("/events", createCHEvents)
	g.GET("/events/:id", getCHEvent)
	g.GET("/stats", chStats)
	g.GET("/sleep", sleepCH)
	g.GET("/echo", echoCH)
	g.GET("/stream", streamCH)
	g.GET("/version", chVersion)
}

// requireCH 没配 ClickHouse 实例时直接 503，免得 xgorm.C("ch") 在 handler 里 panic
func requireCH(c *gin.Context) {
	if !store.HasCH() {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "ch instance is not configured"})
	}
}

type chEventsReq struct {
	Name   string  `json:"name" binding:"required"`
	Values []int64 `json:"values" binding:"required,min=1"`
}

func createCHEvents(c *gin.Context) {
	ctx := c.Request.Context()
	var req chEventsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ids, err := store.InsertEvents(ctx, req.Name, req.Values)
	if err != nil {
		slog.ErrorContext(ctx, "insert ch events failed", "rows", len(req.Values), "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	xmetric.CounterAdd("ch_events_inserted_total", float64(len(ids)))
	c.JSON(http.StatusCreated, gin.H{"ids": ids})
}

func getCHEvent(c *gin.Context) {
	ctx := c.Request.Context()
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id must be a positive integer"})
		return
	}
	e, err := store.GetEvent(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "event not found"})
	case err != nil:
		slog.ErrorContext(ctx, "read ch event failed", "event_id", id, "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusOK, e)
	}
}

func chStats(c *gin.Context) {
	ctx := c.Request.Context()
	s, err := store.EventStats(ctx, c.Query("name"))
	if err != nil {
		slog.ErrorContext(ctx, "ch stats failed", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

// sleepCH 让 ClickHouse 服务端睡 ms 毫秒（默认 500），传的是请求的 ctx
func sleepCH(c *gin.Context) {
	defer xmetric.TrackInFlight("ch_sleep_inflight")()
	ctx, cancel, ok := chCtx(c)
	if !ok {
		return
	}
	defer cancel()
	ms := queryInt(c, "ms", 500)
	start := time.Now()
	err := store.SleepCH(ctx, float64(ms)/1000)
	chDone(c, "ch sleep", start, err, gin.H{"slept_ms": ms})
}

// echoCH 服务端睡 ms 毫秒（默认 0）之后把 tag 回来，body 的 got 是服务端回的那一串
func echoCH(c *gin.Context) {
	ctx, cancel, ok := chCtx(c)
	if !ok {
		return
	}
	defer cancel()
	ms := queryInt(c, "ms", 0)
	start := time.Now()
	got, err := store.EchoCH(ctx, c.Query("tag"), float64(ms)/1000)
	chDone(c, "ch echo", start, err, gin.H{"got": got})
}

// streamCH 服务端吐 rows 行（默认 20），每行之间睡 ms 毫秒（默认 100）
func streamCH(c *gin.Context) {
	defer xmetric.TrackInFlight("ch_stream_inflight")()
	ctx, cancel, ok := chCtx(c)
	if !ok {
		return
	}
	defer cancel()
	rows, ms := queryInt(c, "rows", 20), queryInt(c, "ms", 100)
	start := time.Now()
	n, err := store.StreamCH(ctx, rows, float64(ms)/1000)
	chDone(c, "ch stream", start, err, gin.H{"rows": n})
}

func chVersion(c *gin.Context) {
	v, err := store.VersionCH(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, v)
}

// chCtx 请求的 ctx，给了 &timeout= 就再包一层截止时间。timeout 写错时写好 400 并返回 false
func chCtx(c *gin.Context) (context.Context, context.CancelFunc, bool) {
	ctx := c.Request.Context()
	s := c.Query("timeout")
	if s == "" {
		return ctx, func() {}, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "timeout must be a positive duration such as 200ms"})
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	return ctx, cancel, true
}

// chDone 写好 200 / 503，body 里带上 elapsed_ms；成功记 "<what> finished"，失败记 "<what> failed"
func chDone(c *gin.Context, what string, start time.Time, err error, body gin.H) {
	ctx := c.Request.Context() // 日志用请求的 ctx：截止时间到了也照样带着 trace_id
	body["elapsed_ms"] = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		slog.WarnContext(ctx, what+" failed", "error", err)
		body["error"] = err.Error()
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}
	slog.InfoContext(ctx, what+" finished")
	c.JSON(http.StatusOK, body)
}
