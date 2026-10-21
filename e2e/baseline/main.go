// baseline: a bare gin service to compare against under load.
//
// It serves the same GET /ping and GET /users/:id (reading PG directly) as the e2e service, but
// without going through xone: no access log, no tracing, no metrics, no recovery middleware and no
// cache. Running the same endpoint on both sides, the difference is the framework's overhead. The
// connection pool is set to xgorm's defaults (50 / 50 / 5m / 5m) so that the pool size does not mix
// into that difference.
//
// With E2E_MYSQL_DSN set there is a GET /mysql/users/:id as well, reading the table of the same
// name on MySQL directly, its pool set to xgorm's defaults too, to compare against the endpoint of
// the same name on the e2e service's second xgorm instance.
//
// With E2E_CH_DSN set there is a GET /ch/events/:id as well, reading the event table of the same
// name on ClickHouse (gorm.io/driver/clickhouse) to compare against the endpoint of the same name
// on the e2e service's third xgorm instance: the same 50 / 50 / 5m / 5m pool, the 500ms xgorm
// injects by default added to the DSN when it carries no dial_timeout, and the same
// Where("id = ?").Take to read with.
//
// It is configured from the environment alone, under the same names as the e2e service:
// E2E_PORT, E2E_PG_DSN, E2E_MYSQL_DSN, E2E_CH_DSN, E2E_TABLE.
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
	<-shutdown // ListenAndServe returns as soon as Shutdown starts; wait for the requests in flight before closing the pools
	for _, p := range pools {
		_ = p.Close()
	}
}

// open sets the connection pool to xgorm's defaults: 50 / 50 / 5m / 5m
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

// withDialTimeout adds a 500ms dial_timeout when the DSN carries none: that is what xgorm injects
// as DialTimeout's default
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

// readEvent reads one ClickHouse row by id, the same way the e2e service reads it
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

// readUser reads one row by id, the same way the e2e service reads it
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
