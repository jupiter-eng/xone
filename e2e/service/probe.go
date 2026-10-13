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

// probeRoutes 给 TestCoverage_* 用例（e2e/*_options_test.go 等）用的接口，都挂在 /probe 下面（Swagger UI 除外）：
//
//	GET  /swagger/*any（或 XGinSwagger.URLPrefix 下）  xginswagger 的 UI 和 doc.json
//	GET  /probe/config                      main 里读到的 Service 块、启动钩子里读到的 Cov 块
//	GET  /probe/log?level=warn&msg=xxx      按给定级别打一条业务日志
//	GET  /probe/cache?name=&op=set|get&key=&ttl=  具名的本地缓存实例上 Set（等它生效）/ Get
//	GET  /probe/cache/fill?name=&n=N        往实例里写 N 个不同的键（cost 1），再数还读得到几个
//	GET  /probe/redis?name=&op=set|get&key=&val=  具名的 Redis 实例上 SET / GET，回耗时
//	GET  /probe/fwd?url=http://host/path    经 xhttp 调任意 URL，测按域名透传的规则
//	POST /probe/flow                        xflow：第二步的 Rollback 不看 ctx 地挂住，第三步失败
//	POST /probe/zh                          必填字段校验失败时回 trans.ToZH 之后的报错
//	POST /probe/upload                      multipart 上传，回这个文件是在内存里还是落了盘
//	GET  /probe/skip/*any                   LogSkipPaths 前缀匹配用
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

// probeConfig main 里（xone.Run 之前）读到的 Service 块，和启动钩子里读到的 Cov 块
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

// probeLog 按 level 打一条业务日志，msg 原样当消息
func probeLog(c *gin.Context) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(c.DefaultQuery("level", "info"))); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Log(c.Request.Context(), lv, c.DefaultQuery("msg", "cov log"), "level_asked", lv.String())
	c.JSON(http.StatusOK, gin.H{"level": lv.String()})
}

// cacheName 空串是默认实例
func cacheName(c *gin.Context) []string {
	if n := c.Query("name"); n != "" {
		return []string{n}
	}
	return nil
}

// probeCache op=set：用这个实例的 DefaultTTL（给了 ttl 就用它）写一个键并 Wait；op=get：读回来。
// 名字写错时 xcache.C 会 panic，交给恢复中间件回 500
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

// probeCacheFill 往实例里写 n 个不同的键再逐个读回，回读得到几个。
// 默认实例走包级的 xcache.Set（本包写入时 cost 固定为 1），具名实例自己给 cost 1
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
		cache.Wait() // 一个一个等：不等的话一次写太多会被环形缓冲丢掉，量的就不是容量了
	}
	stored := 0
	for i := range n {
		if _, ok := cache.Get(prefix + strconv.Itoa(i)); ok {
			stored++
		}
	}
	c.JSON(http.StatusOK, gin.H{"written": n, "stored": stored, "max_cost": cache.MaxCost()})
}

// probeRedis 具名的 Redis 实例上 SET / GET 一个键（键名自动加 KeyPrefix），回耗时和错误
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

// probeForward 经 xhttp GET 任意 URL，把下游的状态码转回来
func probeForward(c *gin.Context) {
	resp, err := xhttp.R(c.Request.Context()).Get(c.Query("url"))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": resp.StatusCode()})
}

// hangFlow 三步：first 成功（回滚很快）、hang 成功（回滚不看 ctx 地睡 HangMS）、fail 失败。
// 回滚逆序：先 hang（挂住，被 RollbackTimeout 放弃），再 first（预算已经用完，记成「没执行」）
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

// Rollback hang 那一步故意不看 ctx：文档说这份预算对这样的 Rollback 同样有效
func (s probeStep) Rollback(_ context.Context, r *hangReq) error {
	if s.hang {
		time.Sleep(time.Duration(r.HangMS) * time.Millisecond)
	}
	return nil
}

// probeFlow 跑 hangFlow，回耗时、rolled、每条回滚错误
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

// probeZH 校验失败时回 trans.ToZH 之后的报错：没打开 ZHTranslations 时 ToZH 原样返回
func probeZH(c *gin.Context) {
	var r zhReq
	if err := c.ShouldBindJSON(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": trans.ToZH(err).Error()})
		return
	}
	c.JSON(http.StatusOK, r)
}

// probeUpload 收一个 multipart 上传（字段 file），回大小、sha256，以及它在内存里还是落了盘：
// mime/multipart 超过 maxMemory 的部分写进临时文件，这时 FileHeader.Open 返回的是 *os.File
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
