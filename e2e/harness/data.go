package harness

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // the ClickHouse driver for database/sql, named "clickhouse"
	_ "github.com/go-sql-driver/mysql"         // the MySQL driver for database/sql, named "mysql"
	_ "github.com/jackc/pgx/v5/stdlib"         // the pgx driver for database/sql, named "pgx"
	"github.com/redis/go-redis/v9"
)

// DB connects to PG directly (not through the proxy), for a test to check the data with. It is
// closed when the test ends
func DB(t testing.TB) *sql.DB {
	t.Helper()
	Require(t)
	return openDB(t, "pgx", "postgres", PGDSN(PGAddr()))
}

// MySQL connects to MySQL directly (not through the proxy), for a test to check the data with. It
// is closed when the test ends
func MySQL(t testing.TB) *sql.DB {
	t.Helper()
	Require(t)
	return openDB(t, "mysql", "mysql", MySQLDSN(MySQLAddr()))
}

// CH connects to ClickHouse directly (the native protocol, not through the proxy), for a test to
// check the data with. It is closed when the test ends
func CH(t testing.TB) *sql.DB {
	t.Helper()
	RequireCH(t)
	return openDB(t, "clickhouse", "clickhouse", CHDSN(CHAddr()))
}

// openDB opens dsn with a database/sql driver and closes it when the test ends; name is used in
// the failure messages alone
func openDB(t testing.TB, driver, name, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Redis connects to Redis directly (not through the proxy), for a test to check the data with. It
// is closed when the test ends
func Redis(t testing.TB) *redis.Client {
	t.Helper()
	Require(t)
	c := redis.NewClient(&redis.Options{Addr: RedisAddr()})
	t.Cleanup(func() { c.Close() })
	return c
}

// dropData drops one process's two tables on PG, its table on MySQL (and, with ch, the one on
// ClickHouse) and every key under its prefix. A failed cleanup is only recorded: the names are
// random, so no other case is affected
func dropData(t testing.TB, table, prefix string, ch bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", PGDSN(PGAddr()))
	if err == nil {
		// The table name comes from NewID or from the test, and holds lower-case letters, digits and underscores alone
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
