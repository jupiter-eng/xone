package main

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone/e2e/service/store"
	"github.com/jupiter-eng/xone/xmetric"
)

// The user endpoints on MySQL: the same table as the PG group has, through the second xgorm
// instance xgorm.C("mysql") and past the cache -- what they test is "several instances, each on
// its own" and not the three-level read.
//
//	POST   /mysql/users        201 with the new user
//	GET    /mysql/users/:id    200 / 404
//	PUT    /mysql/users/:id    200 / 404
//	DELETE /mysql/users/:id    204 / 404
//	GET    /mysql/sleep?ms=N   a server-side SELECT SLEEP(N/1000) on the request's ctx: whether a query in flight finishes during a graceful exit
func mysqlRoutes(e *gin.Engine) {
	e.POST("/mysql/users", createMySQLUser)
	e.GET("/mysql/users/:id", getMySQLUser)
	e.PUT("/mysql/users/:id", updateMySQLUser)
	e.DELETE("/mysql/users/:id", deleteMySQLUser)
	e.GET("/mysql/sleep", sleepMySQL)
	e.POST("/bad-sql", badSQL)
}

type badSQLReq struct {
	DB    string `json:"db"` // mysql, pg or ch
	Value string `json:"value"`
}

// badSQL has the server report an error that carries the parameter value in its text on purpose:
// MySQL puts value into a BIGINT column (1366), PG casts value to bigint (22P02) and ClickHouse
// casts value to Int64 (6).
//
// What it tests is that xgorm's SQL log and spans hold no such value; the error is returned
// unchanged, so the response tells the caller "whether the error text holds this value". This
// handler records no error text of its own -- that is the business's own affair and outside what
// xgorm can reach
func badSQL(c *gin.Context) {
	ctx := c.Request.Context()
	var req badSQLReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var err error
	switch req.DB {
	case "mysql":
		err = store.BadInsertMySQL(ctx, req.Value)
	case "pg":
		err = store.BadCastPG(ctx, req.Value)
	case "ch":
		if !store.HasCH() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ch instance is not configured"})
			return
		}
		err = store.BadCastCH(ctx, req.Value)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "db must be mysql, pg or ch"})
		return
	}
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"failed": false})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"failed": true, "error_has_value": strings.Contains(err.Error(), req.Value)})
}

func createMySQLUser(c *gin.Context) {
	ctx := c.Request.Context()
	var req userReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u := store.User{Name: req.Name, Email: req.Email}
	if err := store.CreateMySQLUser(ctx, &u); err != nil {
		slog.ErrorContext(ctx, "create mysql user failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	xmetric.CounterInc("mysql_users_created_total")
	c.JSON(http.StatusCreated, u)
}

func getMySQLUser(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := userID(c)
	if !ok {
		return
	}
	u, err := store.GetMySQLUser(ctx, id)
	if mysqlFailed(c, "read mysql user failed", id, err) {
		return
	}
	c.JSON(http.StatusOK, u)
}

func updateMySQLUser(c *gin.Context) {
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
	if mysqlFailed(c, "update mysql user failed", id, store.UpdateMySQLUser(ctx, u)) {
		return
	}
	c.JSON(http.StatusOK, u)
}

func deleteMySQLUser(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	if mysqlFailed(c, "delete mysql user failed", id, store.DeleteMySQLUser(c.Request.Context(), id)) {
		return
	}
	c.Status(http.StatusNoContent)
}

// mysqlFailed writes the 404 / 500 and returns true when err is non-nil
func mysqlFailed(c *gin.Context, msg string, id int64, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
	default:
		slog.ErrorContext(c.Request.Context(), msg, "user_id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
	return true
}

// sleepMySQL has the MySQL server sleep ms milliseconds (500 by default) on the request's ctx.
// Done it answers 200 and failed 503, both with elapsed_ms in the body
func sleepMySQL(c *gin.Context) {
	defer xmetric.TrackInFlight("mysql_sleep_inflight")()
	ctx := c.Request.Context()
	ms := queryInt(c, "ms", 500)
	start := time.Now()
	err := store.SleepMySQL(ctx, float64(ms)/1000)
	body := gin.H{"slept_ms": ms, "elapsed_ms": float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		slog.WarnContext(ctx, "mysql sleep failed", "ms", ms, "error", err)
		body["error"] = err.Error()
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}
	slog.InfoContext(ctx, "mysql sleep finished", "ms", ms)
	c.JSON(http.StatusOK, body)
}
