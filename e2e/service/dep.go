package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xredis"
)

// dep runs one minimal operation against a dependency, so that a fault test can measure "how long
// does this one take to return":
//
//	GET /dep?target=redis[&timeout=200ms]   a GET of a key that does not exist on Redis (redis.Nil counts as success)
//	GET /dep?target=db[&timeout=200ms]      SELECT 1 on PG
//	GET /dep?target=mysql[&timeout=200ms]   SELECT 1 on MySQL
//	GET /dep?target=ch[&timeout=200ms]      SELECT 1 on ClickHouse (503 with no ch instance configured)
//
// Given a timeout it wraps the request's ctx in context.WithTimeout -- "the caller gave a
// deadline"; without one it is the request's ctx itself, which has no deadline, and the only thing
// left holding it is each client's own timeout.
//
// One request to a business endpoint touches a dependency several times (the three-level read is a
// Redis GET, PG and a Redis SET), so working back from those timings to the cost of one operation
// is inaccurate, which is why this endpoint exists on its own.
// Success answers 200 and failure 503, both with elapsed_ms in the body and failure with error
// besides
func dep(c *gin.Context) {
	ctx := c.Request.Context()
	if s := c.Query("timeout"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "timeout must be a positive duration such as 200ms"})
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	target := c.Query("target")
	start := time.Now()
	var err error
	switch target {
	case "redis":
		err = xredis.C().Get(ctx, conf.C().KeyPrefix+"dep-probe").Err()
		if errors.Is(err, redis.Nil) {
			err = nil
		}
	case "db":
		err = store.Ping(ctx)
	case "mysql":
		err = store.PingMySQL(ctx)
	case "ch":
		if !store.HasCH() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ch instance is not configured"})
			return
		}
		err = store.PingCH(ctx)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "target must be redis, db, mysql or ch"})
		return
	}
	elapsed := time.Since(start)

	body := gin.H{"target": target, "elapsed_ms": float64(elapsed.Microseconds()) / 1000}
	if err != nil {
		body["error"] = err.Error()
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}
	c.JSON(http.StatusOK, body)
}
