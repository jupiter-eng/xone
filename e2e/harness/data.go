package harness

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // database/sql 的 ClickHouse 驱动，名字是 "clickhouse"
	_ "github.com/go-sql-driver/mysql"         // database/sql 的 MySQL 驱动，名字是 "mysql"
	_ "github.com/jackc/pgx/v5/stdlib"         // database/sql 的 pgx 驱动，名字是 "pgx"
	"github.com/redis/go-redis/v9"
)

// DB 直连 PG（不经代理），测试里核对数据用。测试结束时关掉
func DB(t testing.TB) *sql.DB {
	t.Helper()
	Require(t)
	return openDB(t, "pgx", "postgres", PGDSN(PGAddr()))
}

// MySQL 直连 MySQL（不经代理），测试里核对数据用。测试结束时关掉
func MySQL(t testing.TB) *sql.DB {
	t.Helper()
	Require(t)
	return openDB(t, "mysql", "mysql", MySQLDSN(MySQLAddr()))
}

// CH 直连 ClickHouse（native 协议，不经代理），测试里核对数据用。测试结束时关掉
func CH(t testing.TB) *sql.DB {
	t.Helper()
	RequireCH(t)
	return openDB(t, "clickhouse", "clickhouse", CHDSN(CHAddr()))
}

// openDB 用 database/sql 的 driver 打开 dsn，测试结束时关掉；name 只用在失败消息里
func openDB(t testing.TB, driver, name, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Redis 直连 Redis（不经代理），测试里核对数据用。测试结束时关掉
func Redis(t testing.TB) *redis.Client {
	t.Helper()
	Require(t)
	c := redis.NewClient(&redis.Options{Addr: RedisAddr()})
	t.Cleanup(func() { c.Close() })
	return c
}

// dropData 删掉一个进程在 PG 上的两张表、MySQL 上的那张表（ch 时还有 ClickHouse 上的那张）
// 和它前缀下的全部 key。清理失败只记一笔：数据是随机名字，不会影响别的用例
func dropData(t testing.TB, table, prefix string, ch bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", PGDSN(PGAddr()))
	if err == nil {
		// 表名是 NewID 拼的或测试给的，只含小写字母、数字、下划线
		_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS `+table+`, `+table+`_orders`)
		db.Close()
	}
	if err != nil {
		t.Logf("cleanup: drop tables %s: %v", table, err)
	}
	if my, err := sql.Open("mysql", MySQLDSN(MySQLAddr())); err == nil {
		if _, err := my.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			t.Logf("cleanup: drop mysql table %s: %v", table, err)
		}
		my.Close()
	}

	if ch {
		if c, err := sql.Open("clickhouse", CHDSN(CHAddr())); err == nil {
			if _, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
				t.Logf("cleanup: drop clickhouse table %s: %v", table, err)
			}
			c.Close()
		}
	}

	rc := redis.NewClient(&redis.Options{Addr: RedisAddr()})
	defer rc.Close()
	iter := rc.Scan(ctx, 0, prefix+"*", 1000).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Logf("cleanup: scan redis keys %s*: %v", prefix, err)
		return
	}
	if len(keys) > 0 {
		if err := rc.Del(ctx, keys...).Err(); err != nil {
			t.Logf("cleanup: delete redis keys %s*: %v", prefix, err)
		}
	}
}
