package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xcache"
	"github.com/jupiter-eng/xone/xmetric"
	"github.com/jupiter-eng/xone/xredis"
)

type userReq struct {
	Name  string `json:"name" binding:"required"`
	Email string `json:"email"`
}

// userResp 响应里带上这次是从哪一级读到的：local / redis / db
type userResp struct {
	store.User
	Source string `json:"source"`
}

// userKey 本地缓存和 Redis 共用一个 key
func userKey(id int64) string { return conf.C().KeyPrefix + "user:" + strconv.FormatInt(id, 10) }

// createUser 写 PG，再删两级缓存。201 返回新用户
func createUser(c *gin.Context) {
	ctx := c.Request.Context()
	var req userReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u := store.User{Name: req.Name, Email: req.Email}
	if err := store.CreateUser(ctx, &u); err != nil {
		slog.ErrorContext(ctx, "create user failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	invalidate(ctx, u.ID)
	xmetric.CounterInc("users_created_total")
	slog.InfoContext(ctx, "user created", "user_id", u.ID)
	c.JSON(http.StatusCreated, u)
}

// updateUser 改 PG，再删两级缓存。200 返回改后的用户，没有这个用户时 404
func updateUser(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := userID(c)
	if !ok {
		return
	}
	var req userReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u := store.User{ID: id, Name: req.Name, Email: req.Email}
	switch err := store.UpdateUser(ctx, u); {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	case err != nil:
		slog.ErrorContext(ctx, "update user failed", "user_id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	invalidate(ctx, id)
	slog.InfoContext(ctx, "user updated", "user_id", id)
	c.JSON(http.StatusOK, u)
}

// getUser 本地缓存 → Redis → PG 三级读，响应里的 source 说明读到的是哪一级。
//
// ?cache=off 跳过两级缓存直接读 PG，压测时和 baseline 的 /users/:id 对照用。
// Redis 出错不挡读：记一条告警，降级到 PG
func getUser(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := userID(c)
	if !ok {
		return
	}

	var (
		u   store.User
		src string
		err error
	)
	if c.Query("cache") == "off" {
		src = "db"
		u, err = store.GetUser(ctx, id)
	} else {
		u, src, err = readThrough(ctx, id)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	case err != nil:
		slog.ErrorContext(ctx, "read user failed", "user_id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	xmetric.CounterInc("user_reads_total", xmetric.T("source", src))
	slog.DebugContext(ctx, "user read", "user_id", id, "source", src)
	c.JSON(http.StatusOK, userResp{User: u, Source: src})
}

func readThrough(ctx context.Context, id int64) (store.User, string, error) {
	key := userKey(id)
	if u, ok := xcache.Get[store.User](key); ok {
		return u, "local", nil
	}

	b, err := xredis.C().Get(ctx, key).Bytes()
	switch {
	case err == nil:
		var u store.User
		if err := json.Unmarshal(b, &u); err == nil {
			cacheLocal(key, u)
			return u, "redis", nil
		}
		slog.WarnContext(ctx, "bad user entry in redis, reading the database", "key", key)
	case !errors.Is(err, redis.Nil):
		slog.WarnContext(ctx, "redis read failed, falling back to the database", "key", key, "error", err)
	}

	u, err := store.GetUser(ctx, id)
	if err != nil {
		return u, "db", err
	}
	if b, err := json.Marshal(u); err == nil {
		if err := xredis.C().Set(ctx, key, b, conf.C().UserTTL).Err(); err != nil {
			slog.WarnContext(ctx, "redis write failed", "key", key, "error", err)
		}
	}
	cacheLocal(key, u)
	return u, "db", nil
}

// cacheLocal 写本地缓存并等它生效。
//
// ristretto 的写入是异步的，不等的话紧接着的第二次读多半还读不到，
// source 就在 local 和 redis 之间随机跳。只在未命中的路径上等，命中的路径不受影响
func cacheLocal(key string, u store.User) {
	xcache.Set(key, u)
	xcache.C().Wait()
}

// invalidate 删两级缓存。写库已经成功了，删缓存失败不回滚写入：
// 记一条 Error（log_errors_total 跟着加一），读的一端最多读到 UserTTL 那么旧的值
func invalidate(ctx context.Context, id int64) {
	key := userKey(id)
	xcache.Del(key)
	if err := xredis.C().Del(ctx, key).Err(); err != nil {
		slog.ErrorContext(ctx, "cache invalidation failed", "key", key, "error", err)
	}
}

func userID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id must be a positive integer"})
		return 0, false
	}
	return id, true
}
