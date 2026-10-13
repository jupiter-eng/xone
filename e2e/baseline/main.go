// baseline：一个裸 gin 服务，给压测做对照。
//
// 和 e2e 服务有同样的 GET /ping 与 GET /users/:id（直接读 PG），但不经过 xone：
// 没有访问日志、链路、指标、恢复中间件，也没有缓存。两边压同一个接口，
// 差出来的就是框架的开销。连接池按 xgorm 的默认值配（50 / 50 / 5m / 5m），
// 免得差距里混进池子大小的影响。
//
// 设了 E2E_MYSQL_DSN 时另有 GET /mysql/users/:id，直接读 MySQL 上的同名表，
// 连接池同样按 xgorm 的默认值配，对照 e2e 服务里第二个 xgorm 实例的同名接口。
//
// 设了 E2E_CH_DSN 时另有 GET /ch/events/:id，直接读 ClickHouse 上的同名事件表（gorm.io/driver/clickhouse），
// 对照 e2e 服务里第三个 xgorm 实例的同名接口：连接池同样 50 / 50 / 5m / 5m，DSN 里没写 dial_timeout 时
// 补上 xgorm 默认注入的那个 500ms，读法同样是 Where("id = ?").Take。
//
// 配置只读环境变量，名字和 e2e 服务的一致：E2E_PORT、E2E_PG_DSN、E2E_MYSQL_DSN、E2E_CH_DSN、E2E_TABLE。
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/clickhouse"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type user struct {
	ID    int64  `json:"id" gorm:"column:id;primaryKey"`
	Name  string `json:"name" gorm:"column:name"`
	Email string `json:"email" gorm:"column:email"`
}

var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,55}$`)

func main() {
	port, dsn := os.Getenv("E2E_PORT"), os.Getenv("E2E_PG_DSN")
	table := os.Getenv("E2E_TABLE")
	if table == "" {
		table = "e2e_users"
	}
	if port == "" || dsn == "" || !ident.MatchString(table) {
		log.Fatal("E2E_PORT and E2E_PG_DSN are required, E2E_TABLE must be a lower-case identifier")
	}

	db, pool := open(postgres.Open(dsn))
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + table + ` (
		id         BIGSERIAL PRIMARY KEY,
		name       TEXT NOT NULL,
		email      TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`).Error; err != nil {
		log.Fatal(err)
	}

	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	e.GET("/users/:id", readUser(db, table))

	pools := []*sql.DB{pool}
	if mdsn := os.Getenv("E2E_MYSQL_DSN"); mdsn != "" {
		my, mpool := open(mysql.Open(mdsn))
		if err := my.Exec(`CREATE TABLE IF NOT EXISTS ` + table + ` (
			id         BIGINT AUTO_INCREMENT PRIMARY KEY,
			name       VARCHAR(255) NOT NULL,
			email      VARCHAR(255) NOT NULL DEFAULT '',
			created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
		)`).Error; err != nil {
			log.Fatal(err)
		}
		e.GET("/mysql/users/:id", readUser(my, table))
		pools = append(pools, mpool)
	}
	if cdsn := os.Getenv("E2E_CH_DSN"); cdsn != "" {
		ch, cpool := open(clickhouse.New(clickhouse.Config{DSN: withDialTimeout(cdsn)}))
		if err := ch.Exec(`CREATE TABLE IF NOT EXISTS ` + table + ` (
			id         UInt64,
			name       String,
			value      Int64,
			created_at DateTime64(3) DEFAULT now64(3)
		) ENGINE = MergeTree ORDER BY id`).Error; err != nil {
			log.Fatal(err)
		}
		e.GET("/ch/events/:id", readEvent(ch, table))
		pools = append(pools, cpool)
	}

	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: e, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdown // ListenAndServe 在 Shutdown 一开始就返回，等在途请求做完再关池子
	for _, p := range pools {
		_ = p.Close()
	}
}

// open 按 xgorm 的默认值配好连接池：50 / 50 / 5m / 5m
func open(d gorm.Dialector) (*gorm.DB, *sql.DB) {
	db, err := gorm.Open(d, &gorm.Config{Logger: logger.Discard})
	if err != nil {
		log.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	pool.SetMaxOpenConns(50)
	pool.SetMaxIdleConns(50)
	pool.SetConnMaxLifetime(5 * time.Minute)
	pool.SetConnMaxIdleTime(5 * time.Minute)
	return db, pool
}

// withDialTimeout DSN 里没写 dial_timeout 时补上 500ms：xgorm 按 DialTimeout 的默认值注入的就是它
func withDialTimeout(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		log.Fatal("E2E_CH_DSN is not a URL")
	}
	q := u.Query()
	if !q.Has("dial_timeout") {
		q.Set("dial_timeout", "500ms")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

type event struct {
	ID    uint64 `json:"id" gorm:"column:id"`
	Name  string `json:"name" gorm:"column:name"`
	Value int64  `json:"value" gorm:"column:value"`
}

// readEvent 按 id 读 ClickHouse 上的一行，和 e2e 服务里的读法一样
func readEvent(db *gorm.DB, table string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil || id == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id must be a positive integer"})
			return
		}
		var ev event
		err = db.WithContext(c.Request.Context()).Table(table).Where("id = ?", id).Take(&ev).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "event not found"})
		case err != nil:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusOK, ev)
		}
	}
}

// readUser 按 id 读一行，和 e2e 服务里的读法一样
func readUser(db *gorm.DB, table string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id must be a positive integer"})
			return
		}
		var u user
		err = db.WithContext(c.Request.Context()).Table(table).Where("id = ?", id).Take(&u).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		case err != nil:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusOK, u)
		}
	}
}
