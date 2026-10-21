package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/jupiter-eng/xone/e2e/service/apidoc"
	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/e2e/service/covconf"
	"github.com/jupiter-eng/xone/xcache"
	"github.com/jupiter-eng/xone/xflow"
	"github.com/jupiter-eng/xone/xgin/trans"
	"github.com/jupiter-eng/xone/xginswagger"
	"github.com/jupiter-eng/xone/xhttp"
	"github.com/jupiter-eng/xone/xredis"
)

// probeRoutes are the endpoints for the TestCoverage_* cases (e2e/*_options_test.go and others),
// all of them under /probe (the Swagger UI aside):
//
//	GET  /swagger/*any (or under XGinSwagger.URLPrefix)  xginswagger's UI and doc.json
//	GET  /probe/config                      the Service block main read and the Cov block the start hook read
//	GET  /probe/log?level=warn&msg=xxx      writes one business log line at the given level
//	GET  /probe/cache?name=&op=set|get&key=&ttl=  a Set (waiting for it to take) / Get on a named local cache instance
//	GET  /probe/cache/fill?name=&n=N        writes N distinct keys (cost 1) into the instance and counts how many are still readable
//	GET  /probe/redis?name=&op=set|get&key=&val=  a SET / GET on a named Redis instance, answering how long it took
//	GET  /probe/fwd?url=http://host/path    calls any URL through xhttp, to test the per-host forwarding rules
//	POST /probe/flow                        xflow: the second step's Rollback hangs without watching ctx and the third step fails
//	POST /probe/zh                          answers the validation error of a missing required field after trans.ToZH
//	POST /probe/upload                      a multipart upload, answering whether the file stayed in memory or landed on disk
//	GET  /probe/skip/*any                   for the LogSkipPaths prefix matching
func probeRoutes(e *gin.Engine) {
	apidoc.Register()
	xginswagger.Register(e, apidoc.SwaggerInfo)

	g := e.Group("/probe")
	g.GET("/config", probeConfig)
	g.GET("/log", probeLog)
	g.GET("/cache", probeCache)
	g.GET("/cache/fill", probeCacheFill)
	g.GET("/redis", probeRedis)
	g.GET("/fwd", probeForward)
	g.POST("/flow", probeFlow)
	g.POST("/zh", probeZH)
	g.POST("/upload", probeUpload)
	g.GET("/skip/*any", func(c *gin.Context) { c.String(http.StatusOK, "skipped") })
}

// probeConfig is the Service block as main read it (before xone.Run) and the Cov block as the start
// hook read it
func probeConfig(c *gin.Context) {
	s := conf.C()
	c.JSON(http.StatusOK, gin.H{
		"service": gin.H{
			"downstream":   s.Downstream,
			"user_ttl":     s.UserTTL.String(),
			"stop_timeout": s.StopTimeout.String(),
		},
		"cov": covconf.C(),
	})
}

// probeLog writes one business log line at level, with msg as the message as it is
func probeLog(c *gin.Context) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(c.DefaultQuery("level", "info"))); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Log(c.Request.Context(), lv, c.DefaultQuery("msg", "cov log"), "level_asked", lv.String())
	c.JSON(http.StatusOK, gin.H{"level": lv.String()})
}

// cacheName: an empty string is the default instance
func cacheName(c *gin.Context) []string {
	if n := c.Query("name"); n != "" {
		return []string{n}
	}
	return nil
}

// probeCache, with op=set, writes one key with this instance's DefaultTTL (or the ttl given) and
// Waits; with op=get it reads it back.
// A wrong name has xcache.C panic, which the recovery middleware answers with a 500
func probeCache(c *gin.Context) {
	name := cacheName(c)
	cache := xcache.C(name...)
	key := c.Query("key")
	switch c.Query("op") {
	case "set":
		ttl := xcache.DefaultTTL(name...)
		if s := c.Query("ttl"); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			ttl = d
		}
		ok := cache.SetWithTTL(key, c.Query("val"), 1, ttl)
		cache.Wait()
		c.JSON(http.StatusOK, gin.H{"accepted": ok, "ttl": ttl.String()})
	case "get":
		v, ok := cache.Get(key)
		c.JSON(http.StatusOK, gin.H{"found": ok, "val": v})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "op must be set or get"})
	}
}

// probeCacheFill writes n distinct keys into the instance, reads them back one by one and answers
// how many are still there.
// The default instance goes through the package-level xcache.Set (where this package writes with a
// cost fixed at 1), and a named instance passes cost 1 itself
func probeCacheFill(c *gin.Context) {
	name := cacheName(c)
	cache := xcache.C(name...)
	n, err := strconv.Atoi(c.Query("n"))
	if err != nil || n <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "n must be a positive integer"})
		return
	}
	prefix := conf.C().KeyPrefix + "fill:" + randomHex(4) + ":"
	for i := range n {
		k := prefix + strconv.Itoa(i)
		if len(name) == 0 {
			xcache.Set(k, i)
		} else {
			cache.SetWithTTL(k, i, 1, xcache.DefaultTTL(name...))
		}
		cache.Wait() // one at a time: without the wait, writing too many at once has the ring buffer drop them and what is measured is no longer the capacity
	}
	stored := 0
	for i := range n {
		if _, ok := cache.Get(prefix + strconv.Itoa(i)); ok {
			stored++
		}
	}
	c.JSON(http.StatusOK, gin.H{"written": n, "stored": stored, "max_cost": cache.MaxCost()})
}

// probeRedis SETs / GETs one key on a named Redis instance (the key name gets KeyPrefix
// automatically) and answers how long it took and any error
func probeRedis(c *gin.Context) {
	ctx := c.Request.Context()
	var name []string
	if n := c.Query("name"); n != "" {
		name = []string{n}
	}
	client := xredis.C(name...)
	key := conf.C().KeyPrefix + c.Query("key")
	start := time.Now()
	var (
		val string
		err error
	)
	switch c.Query("op") {
	case "set":
		err = client.Set(ctx, key, c.Query("val"), time.Minute).Err()
	case "get":
		val, err = client.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			err = nil
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "op must be set or get"})
		return
	}
	body := gin.H{"key": key, "val": val, "db": client.Options().DB, "elapsed_ms": float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		body["error"] = err.Error()
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}
	c.JSON(http.StatusOK, body)
}

// probeForward GETs any URL through xhttp and passes the downstream's status code back
func probeForward(c *gin.Context) {
	resp, err := xhttp.R(c.Request.Context()).Get(c.Query("url"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": resp.StatusCode()})
}

// hangFlow has three steps: first succeeds (and rolls back quickly), hang succeeds (and its
// rollback sleeps HangMS without watching ctx), and fail fails.
// The rollback goes in reverse: hang first (where it hangs and is given up on by RollbackTimeout),
// then first (where the budget is already spent and it is recorded as "not run")
var hangFlow = xflow.New[*hangReq]("cov_hang_rollback", probeStep{"first", false}, probeStep{"hang", true}, probeStep{"fail", false})

type hangReq struct {
	HangMS int `json:"hang_ms"`
}

type probeStep struct {
	name string
	hang bool
}

func (s probeStep) Name() string { return s.name }

func (s probeStep) Process(context.Context, *hangReq) error {
	if s.name == "fail" {
		return errors.New("injected failure at step fail")
	}
	return nil
}

// Rollback: the hang step deliberately does not watch ctx, and the documentation says this budget
// holds for such a Rollback all the same
func (s probeStep) Rollback(_ context.Context, r *hangReq) error {
	if s.hang {
		time.Sleep(time.Duration(r.HangMS) * time.Millisecond)
	}
	return nil
}

// probeFlow runs hangFlow and answers how long it took, rolled, and each rollback error
func probeFlow(c *gin.Context) {
	var r hangReq
	if err := c.ShouldBindJSON(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	start := time.Now()
	res := hangFlow.Execute(c.Request.Context(), &r)
	elapsed := time.Since(start)
	errs := make([]string, len(res.RollbackErrors))
	for i, e := range res.RollbackErrors {
		errs[i] = e.Error()
	}
	body := gin.H{"success": res.Success(), "rolled": res.Rolled, "rollback_errors": errs, "elapsed_ms": elapsed.Milliseconds()}
	if res.Err != nil {
		body["error"] = res.Err.Error()
	}
	c.JSON(http.StatusOK, body)
}

type zhReq struct {
	Name string `json:"name" binding:"required"`
	Age  int    `json:"age" binding:"gte=1"`
}

// probeZH answers the validation error after trans.ToZH: without ZHTranslations on, ToZH returns it unchanged
func probeZH(c *gin.Context) {
	var r zhReq
	if err := c.ShouldBindJSON(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": trans.ToZH(err).Error()})
		return
	}
	c.JSON(http.StatusOK, r)
}

// probeUpload takes one multipart upload (the field file) and answers the size, the sha256 and
// whether it stayed in memory or landed on disk: mime/multipart writes whatever goes past
// maxMemory into a temporary file, and FileHeader.Open then returns an *os.File
func probeUpload(c *gin.Context) {
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
	_, onDisk := f.(*os.File)
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("read upload: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"size": n, "sha256": hex.EncodeToString(h.Sum(nil)), "on_disk": onDisk})
}
