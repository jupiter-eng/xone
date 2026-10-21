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

// The endpoints on ClickHouse: the third xgorm instance xgorm.C("ch"), through the
// xgorm/clickhouse dialect and the native protocol.
// The instance exists only when the harness activates the ch profile
// (service/application-ch.yml), and without it everything answers 503.
//
//	POST /ch/events              {"name": "...", "values": [1, 2]}, one INSERT writing a batch, 201 with the ids
//	GET  /ch/events/:id          a point lookup by primary key, 200 / 404
//	GET  /ch/stats?name=x        count() / sum(value) grouped by name, through GORM's Scan
//	GET  /ch/sleep?ms=N          a server-side SELECT sleep(N/1000) (the server stops at 3s) on the request's ctx
//	GET  /ch/echo?tag=x&ms=N     the server sleeps N milliseconds in a scalar subquery before answering tag: not a byte comes back before the header
//	GET  /ch/stream?rows=N&ms=M  one data block per row, sleeping M milliseconds between rows: it only slows down after the header
//	GET  /ch/version             the version recorded on the Dialector and the two switches set from it
//
// sleep / echo / stream also take &timeout=200ms: the request's ctx wrapped in
// context.WithTimeout ("the caller gave a deadline"), and without it the request's ctx itself.
// Success carries elapsed_ms in the body and a 503 carries error besides: a fault test measures
// "how long does this one take" with them
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

// requireCH answers 503 outright with no ClickHouse instance configured, so that xgorm.C("ch")
// does not panic inside a handler
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

// sleepCH has the ClickHouse server sleep ms milliseconds (500 by default) on the request's ctx
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

// echoCH has the server answer tag after sleeping ms milliseconds (0 by default), and got in the
// body is the string the server sent back
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

// streamCH has the server emit rows rows (20 by default), sleeping ms milliseconds (100 by
// default) between them
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

// chCtx is the request's ctx, wrapped in a deadline when &timeout= was given. A malformed timeout
// writes the 400 and returns false
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

// chDone writes the 200 / 503 with elapsed_ms in the body; a success records "<what> finished" and
// a failure "<what> failed"
func chDone(c *gin.Context, what string, start time.Time, err error, body gin.H) {
	ctx := c.Request.Context() // the log goes on the request's ctx: it carries trace_id even once the deadline has passed
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
