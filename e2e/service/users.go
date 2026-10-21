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

// userResp carries which level this read came from: local / redis / db
type userResp struct {
	store.User
	Source string `json:"source"`
}

// userKey is shared by the local cache and Redis
func userKey(id int64) string { return conf.C().KeyPrefix + "user:" + strconv.FormatInt(id, 10) }

// createUser writes PG and then drops both cache levels. It answers 201 with the new user
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

// updateUser updates PG and then drops both cache levels. It answers 200 with the updated user, or
// 404 when there is no such user
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

// getUser reads through three levels, local cache -> Redis -> PG, and source in the response says
// which level answered.
//
// ?cache=off skips both cache levels and reads PG directly, to compare against baseline's
// /users/:id under load.
// A Redis error does not stop the read: it records a warning and falls back to PG
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

// cacheLocal writes the local cache and waits for it to take.
//
// ristretto writes asynchronously, and without the wait a second read right after mostly still
// misses, which has source jump between local and redis at random. The wait happens on the miss
// path alone and the hit path is untouched
func cacheLocal(key string, u store.User) {
	xcache.Set(key, u)
	xcache.C().Wait()
}

// invalidate drops both cache levels. The write has already succeeded, so a failed cache drop does
// not roll it back: it records an Error (and log_errors_total goes up with it), and the reading end
// sees a value at most UserTTL old
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
