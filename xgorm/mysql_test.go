package xgorm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// silentServer 收下连接、一个字节都不回：MySQL 握手要服务端先说话，客户端就卡在读握手包上。
// 返回地址和「收到过几个连接」
func silentServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		n     atomic.Int32
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return l.Addr().String(), &n
}

func silentMySQL(t *testing.T, read time.Duration) (ClientConfig, *atomic.Int32) {
	addr, n := silentServer(t)
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DSN = "u:p@tcp(" + addr + ")/app"
	c.DialTimeout = 50 * time.Millisecond
	c.MySQL.ReadTimeout = read
	return c, n
}

func TestNew_MySQLRetriesThreeTimesWhenPeerSilent(t *testing.T) {
	// 回归用例。曾经 gorm.Open 里驱动自己查一次 SELECT VERSION()（context.Background()），
	// 那一下就是第一次建连：失败了 gorm.Open 直接返回，xgorm 的重试一次都没轮上，
	// 实测只建了 1 个连接
	c, accepted := silentMySQL(t, 100*time.Millisecond)
	start := time.Now()
	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("对端不回话时应当报错")
	}
	assertOneFrame(t, err, "xgorm", "connect")
	if !strings.Contains(err.Error(), "cannot reach ") {
		t.Errorf("该报成连不上，got=%v", err)
	}
	if got := accepted.Load(); got != pingAttempts {
		t.Errorf("文档说建连验证试 %d 次，实际对端收到 %d 个连接", pingAttempts, got)
	}
	t.Logf("数字：对端不回话、ReadTimeout 100ms：%v 后失败，对端收到 %d 个连接", time.Since(start).Round(time.Millisecond), accepted.Load())
}

func TestNew_MySQLCancelReturnsImmediatelyWithoutReadTimeout(t *testing.T) {
	// 握手那一读受 ReadTimeout 管（不是 DialTimeout）。建连走 ctx-aware 的 Ping 之后，
	// 取消由 go-sql-driver 的 watcher 当场关掉连接，不必等这一读超时
	c, accepted := silentMySQL(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for accepted.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // 让它卡进读握手包里
		cancel()
	}()

	start := time.Now()
	_, _, err := New(ctx, c)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消之后该报 context canceled，got=%v", err)
	}
	if elapsed > time.Second {
		t.Errorf("取消之后该当场返回，实际 New 用了 %v（ReadTimeout 5s）", elapsed)
	}
	t.Logf("数字：卡在握手读上时取消，New 在 %v 返回（其中约 50ms 是取消前的等待）", elapsed.Round(time.Millisecond))
}

func TestNew_MySQLDriverLogsGoToSlog(t *testing.T) {
	// go-sql-driver 默认往 stderr 写 [mysql] 2026/09/24 … packets.go:58 …，绕开 slog。
	// 对端不回话、读超时时它就会写这一行
	logs := capture(t)
	c, _ := silentMySQL(t, 50*time.Millisecond)
	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("对端不回话时应当报错")
	}
	var got []map[string]any
	for _, l := range logs() {
		if l["msg"] == "xgorm go-sql-driver log" {
			got = append(got, l)
		}
	}
	if len(got) == 0 {
		t.Fatal("读超时时驱动会记一条日志，应当走 slog")
	}
	if got[0]["level"] != "WARN" {
		t.Errorf("驱动的日志应记成 WARN，实际 %v", got[0]["level"])
	}
	if d, _ := got[0]["detail"].(string); !strings.Contains(d, "packets.go") || !strings.Contains(d, "timeout") {
		t.Errorf("原文要放在 detail 里，实际 %q", d)
	}
	t.Logf("数字：一轮 3 次读超时，驱动记了 %d 条；例：%v", len(got), got[0]["detail"])
}

func TestMySQLDialect_OpenSkipsNetwork_VersionProbeInReady(t *testing.T) {
	d, _ := lookupDialect(DriverMySQL)
	md, ok := d.Open("u:p@tcp(127.0.0.1:1)/app").(*mysql.Dialector)
	if !ok {
		t.Fatalf("MySQL 的 Open 应造出 *mysql.Dialector")
	}
	if !md.SkipInitializeWithVersion {
		t.Error("Initialize 里那次查版本必须关掉，挪到 Ready 里做")
	}
	if d.Ready == nil {
		t.Fatal("MySQL 方言该带着 Ready，不然版本就永远没人查了")
	}
}

func TestProbeMySQLVersion_SetsVersionOnDialector(t *testing.T) {
	versionReply = "5.7.44-log"
	t.Cleanup(func() { versionReply = "8.0.46" })
	db := fakeVersionDB(t)
	d, _ := lookupDialect(DriverMySQL)
	if err := d.Ready(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	c := db.Dialector.(*mysql.Dialector).Config
	if c.ServerVersion != "5.7.44-log" {
		t.Errorf("ServerVersion 应是查到的值，实际 %q", c.ServerVersion)
	}
	if !c.DontSupportRenameColumn || !c.DontSupportForShareClause || !c.DontSupportDropConstraint {
		t.Errorf("5.7 上这三项都不支持，实际 %+v", c)
	}
}

func TestProbeMySQLVersion_QueryHonorsCtx(t *testing.T) {
	// 挪出 gorm.Open 就是为了这一条：驱动原来那次用的是 context.Background()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeMySQLVersion(ctx, fakeVersionDB(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 已取消时查版本该当场放弃，got=%v", err)
	}
}

func TestApplyMySQLVersion_MatchesDriverRules(t *testing.T) {
	type flags struct{ renameIndex, renameColumn, forShare, nullDefault, dropConstraint, noPrecision, renameUnique bool }
	for _, c := range []struct {
		v    string
		want flags
	}{
		{"8.0.46", flags{}},
		{"5.7.44", flags{renameColumn: true, forShare: true, dropConstraint: true}},
		{"5.6.51", flags{renameIndex: true, renameColumn: true, forShare: true, dropConstraint: true}},
		{"5.5.62", flags{renameIndex: true, renameColumn: true, forShare: true, dropConstraint: true, noPrecision: true}},
		{"10.4.34-MariaDB", flags{renameIndex: true, renameColumn: true, forShare: true, nullDefault: true}},
		{"8.0.11-TiDB-v7.5.1", flags{renameUnique: true}},
	} {
		t.Run(c.v, func(t *testing.T) {
			db := fakeVersionDB(t)
			cfg := db.Dialector.(*mysql.Dialector).Config
			if err := applyMySQLVersion(db, cfg, c.v); err != nil {
				t.Fatal(err)
			}
			got := flags{cfg.DontSupportRenameIndex, cfg.DontSupportRenameColumn, cfg.DontSupportForShareClause,
				cfg.DontSupportNullAsDefaultValue, cfg.DontSupportDropConstraint, cfg.DisableDatetimePrecision, cfg.DontSupportRenameColumnUnique}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestApplyMySQLVersion_IdenticalToDriverOwnProbe(t *testing.T) {
	// 规则是抄的，升级驱动时最容易漂：拿驱动自己的 Initialize（不跳过查版本，
	// 让它在假驱动上查到同一个版本号）和我们的结果逐项对比，开关和增删改的子句都要一样
	t.Cleanup(func() { versionReply = "8.0.46" })
	for _, v := range []string{"8.0.46", "5.7.44-log", "5.6.51", "5.5.62", "10.4.34-MariaDB", "10.11.8-MariaDB-1:10.11.8+maria~ubu2204", "8.0.11-TiDB-v7.5.1"} {
		t.Run(v, func(t *testing.T) {
			versionReply = v
			pool, err := sql.Open("xgorm-fake-version", "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { pool.Close() })
			theirs, err := gorm.Open(mysql.New(mysql.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
			if err != nil {
				t.Fatal(err)
			}
			ours := fakeVersionDB(t)
			if err := probeMySQLVersion(context.Background(), ours); err != nil {
				t.Fatal(err)
			}
			tc, oc := *theirs.Dialector.(*mysql.Dialector).Config, *ours.Dialector.(*mysql.Dialector).Config
			tc.Conn, oc.Conn = nil, nil
			tc.SkipInitializeWithVersion = false
			oc.SkipInitializeWithVersion = false
			if !reflect.DeepEqual(tc, oc) {
				t.Errorf("开关与驱动不一致：\n驱动 %+v\n我们 %+v", tc, oc)
			}
			for _, p := range []struct {
				name     string
				tcl, ocl []string
			}{
				{"create", theirs.Callback().Create().Clauses, ours.Callback().Create().Clauses},
				{"update", theirs.Callback().Update().Clauses, ours.Callback().Update().Clauses},
				{"delete", theirs.Callback().Delete().Clauses, ours.Callback().Delete().Clauses},
			} {
				if !slices.Equal(p.tcl, p.ocl) {
					t.Errorf("%s 的子句与驱动不一致：驱动 %v，我们 %v", p.name, p.tcl, p.ocl)
				}
			}
		})
	}
}

type returningUser struct {
	ID   uint
	Name string
	Flag int `gorm:"default:7"`
}

func TestApplyMySQLVersion_MariaDB105PlusWritesUseRETURNING(t *testing.T) {
	// 驱动在 Initialize 里把「支不支持 RETURNING」闭包进了回调，版本挪到后面查之后
	// 得把那三个回调换掉，否则 MariaDB 10.5+ 上默认值字段就不再回填
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"10.11.8-MariaDB", true},
		{"10.5.0-MariaDB", true},
		{"10.4.34-MariaDB", false},
		{"8.0.46", false},
	} {
		t.Run(c.v, func(t *testing.T) {
			db := fakeVersionDB(t)
			if err := applyMySQLVersion(db, db.Dialector.(*mysql.Dialector).Config, c.v); err != nil {
				t.Fatal(err)
			}
			tx := db.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true})
			sqls := []string{
				tx.Create(&returningUser{Name: "a"}).Statement.SQL.String(),
				tx.Model(&returningUser{ID: 1}).Clauses(clause.Returning{}).Update("name", "b").Statement.SQL.String(),
				tx.Clauses(clause.Returning{}).Delete(&returningUser{ID: 1}).Statement.SQL.String(),
			}
			for _, s := range sqls {
				if got := strings.Contains(s, "RETURNING"); got != c.want {
					t.Errorf("RETURNING 应为 %v，实际 SQL：%s", c.want, s)
				}
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		v, floor string
		want     bool
	}{
		{"10.5", "10.5", true},
		{"10.5.1-MariaDB", "10.5", true},
		{"10.11.8-MariaDB", "10.5", true},
		{"10.4.34-MariaDB", "10.5", false},
		{"11.0.2-MariaDB", "10.5", true},
		{"9.9", "10.5", false},
	} {
		if got := versionAtLeast(c.v, c.floor); got != c.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", c.v, c.floor, got, c.want)
		}
	}
}

// ---- 一个只会回版本号的 database/sql 驱动 ----

// versionReply fakeVersion 驱动对任何查询回的那一行
var versionReply = "8.0.46"

func init() { sql.Register("xgorm-fake-version", fakeVersionDriver{}) }

// fakeVersionDB 用 MySQL 方言、跑在假驱动上的 *gorm.DB，不碰网络
func fakeVersionDB(t *testing.T) *gorm.DB {
	t.Helper()
	pool, err := sql.Open("xgorm-fake-version", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}),
		&gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

type fakeVersionDriver struct{}

func (fakeVersionDriver) Open(string) (driver.Conn, error) { return fakeVersionConn{}, nil }

type fakeVersionConn struct{}

func (fakeVersionConn) Prepare(string) (driver.Stmt, error) { return fakeVersionStmt{}, nil }
func (fakeVersionConn) Close() error                        { return nil }
func (fakeVersionConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

type fakeVersionStmt struct{}

func (fakeVersionStmt) Close() error  { return nil }
func (fakeVersionStmt) NumInput() int { return -1 }
func (fakeVersionStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("not supported")
}
func (fakeVersionStmt) Query([]driver.Value) (driver.Rows, error) {
	return &fakeVersionRows{}, nil
}

type fakeVersionRows struct{ done bool }

func (*fakeVersionRows) Columns() []string { return []string{"VERSION()"} }
func (*fakeVersionRows) Close() error      { return nil }
func (r *fakeVersionRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = versionReply
	return nil
}
