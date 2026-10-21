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

// silentServer accepts the connection and answers not one byte: a MySQL handshake has the server
// speak first, so the client is stuck reading the handshake packet.
// It returns the address and "how many connections arrived".
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
	// Regression. gorm.Open used to have the driver query SELECT VERSION() itself
	// (context.Background()), and that was the first connection: a failure made gorm.Open return
	// outright and none of xgorm's retries was ever reached, with one connection measured.
	c, accepted := silentMySQL(t, 100*time.Millisecond)
	start := time.Now()
	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("an unresponsive peer should be reported as an error")
	}
	assertOneFrame(t, err, "xgorm", "connect")
	if !strings.Contains(err.Error(), "cannot reach ") {
		t.Errorf("it should be reported as unreachable, got=%v", err)
	}
	if got := accepted.Load(); got != pingAttempts {
		t.Errorf("the documentation says the connection check tries %d times, and the peer received %d connections", pingAttempts, got)
	}
	t.Logf("numbers: unresponsive peer, ReadTimeout 100ms: failed after %v, the peer received %d connections", time.Since(start).Round(time.Millisecond), accepted.Load())
}

func TestNew_MySQLCancelReturnsImmediatelyWithoutReadTimeout(t *testing.T) {
	// The handshake read is governed by ReadTimeout (not DialTimeout). Once connecting goes through
	// the ctx-aware Ping, cancellation makes go-sql-driver's watcher close the connection on the
	// spot, without waiting out that read
	c, accepted := silentMySQL(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for accepted.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // let it get stuck reading the handshake packet
		cancel()
	}()

	start := time.Now()
	_, _, err := New(ctx, c)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("after cancellation it should report context canceled, got=%v", err)
	}
	if elapsed > time.Second {
		t.Errorf("after cancellation it should return on the spot, but New took %v (ReadTimeout 5s)", elapsed)
	}
	t.Logf("numbers: cancelled while stuck on the handshake read, New returned in %v (about 50ms of it waiting before the cancel)", elapsed.Round(time.Millisecond))
}

func TestNew_MySQLDriverLogsGoToSlog(t *testing.T) {
	// go-sql-driver writes [mysql] 2026/09/24 … packets.go:58 … to stderr by default, bypassing
	// slog. An unresponsive peer and a read timeout is exactly when it writes that line
	logs := capture(t)
	c, _ := silentMySQL(t, 50*time.Millisecond)
	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("an unresponsive peer should be reported as an error")
	}
	var got []map[string]any
	for _, l := range logs() {
		if l["msg"] == "xgorm go-sql-driver log" {
			got = append(got, l)
		}
	}
	if len(got) == 0 {
		t.Fatal("the driver logs a line on a read timeout, and it should go through slog")
	}
	if got[0]["level"] != "WARN" {
		t.Errorf("the driver's log should be recorded as WARN, got %v", got[0]["level"])
	}
	if d, _ := got[0]["detail"].(string); !strings.Contains(d, "packets.go") || !strings.Contains(d, "timeout") {
		t.Errorf("the original text belongs in detail, got %q", d)
	}
	t.Logf("numbers: 3 read timeouts in one round produced %d driver lines; for example: %v", len(got), got[0]["detail"])
}

func TestMySQLDialect_OpenSkipsNetwork_VersionProbeInReady(t *testing.T) {
	d, _ := lookupDialect(DriverMySQL)
	md, ok := d.Open("u:p@tcp(127.0.0.1:1)/app").(*mysql.Dialector)
	if !ok {
		t.Fatalf("MySQL's Open should build a *mysql.Dialector")
	}
	if !md.SkipInitializeWithVersion {
		t.Error("the version query inside Initialize has to be off and moved into Ready")
	}
	if d.Ready == nil {
		t.Fatal("the MySQL dialect has to carry a Ready, or nobody ever queries the version")
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
		t.Errorf("ServerVersion should be the value queried, got %q", c.ServerVersion)
	}
	if !c.DontSupportRenameColumn || !c.DontSupportForShareClause || !c.DontSupportDropConstraint {
		t.Errorf("none of the three is supported on 5.7, got %+v", c)
	}
}

func TestProbeMySQLVersion_QueryHonorsCtx(t *testing.T) {
	// Moving it out of gorm.Open was for exactly this: the driver's own used context.Background()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeMySQLVersion(ctx, fakeVersionDB(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("with the ctx already cancelled the version query should give up on the spot, got=%v", err)
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
	// The rules are copied and drift most easily on a driver upgrade: compare the driver's own
	// Initialize (without skipping the version query, so it finds the same version on the fake
	// driver) against our result field by field, switches and clauses alike
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
				t.Errorf("the switches disagree with the driver:\ndriver %+v\nours %+v", tc, oc)
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
					t.Errorf("%s's clauses disagree with the driver: driver %v, ours %v", p.name, p.tcl, p.ocl)
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
	// The driver closes "whether RETURNING is supported" into its callbacks inside Initialize, so
	// once the version query moves later those three callbacks have to be replaced, or default-valued
	// fields stop being filled back in on MariaDB 10.5+
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
					t.Errorf("RETURNING should be %v, the SQL was: %s", c.want, s)
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

// ---- A database/sql driver that answers nothing but a version ----

// versionReply is the row the fakeVersion driver answers to any query
var versionReply = "8.0.46"

func init() { sql.Register("xgorm-fake-version", fakeVersionDriver{}) }

// fakeVersionDB is a *gorm.DB on the MySQL dialect running on the fake driver, touching no network
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
