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

// dep 对一个依赖做一次最小的操作，给故障测试量「这一下要多久才返回」：
//
//	GET /dep?target=redis[&timeout=200ms]   Redis 上 GET 一个不存在的 key（redis.Nil 算成功）
//	GET /dep?target=db[&timeout=200ms]      PG 上 SELECT 1
//	GET /dep?target=mysql[&timeout=200ms]   MySQL 上 SELECT 1
//	GET /dep?target=ch[&timeout=200ms]      ClickHouse 上 SELECT 1（没配 ch 实例时 503）
//
// 给了 timeout 就用 context.WithTimeout 包住请求的 ctx——「调用方给了截止时间」；
// 不给就是请求的 ctx 本身，没有截止时间，管得住它的只剩各客户端自己配的超时。
//
// 业务接口一次请求要碰好几下依赖（三级读是 Redis GET、PG、Redis SET），
// 从它们的耗时反推单次操作的代价不准，所以单独开这个口子。
// 成功 200、失败 503，body 里都有 elapsed_ms，失败时另有 error
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
