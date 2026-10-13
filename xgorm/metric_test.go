package xgorm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xmetric"
)

func TestPoolCollector(t *testing.T) {
	// 连接池状态是瞬时量，所以是抓取时才读，而不是定时推进 Gauge
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	stats := map[string]sql.DBStats{
		"main":   {OpenConnections: 7, InUse: 3, Idle: 4, MaxOpenConnections: 50, WaitCount: 2, WaitDuration: 1500 * time.Millisecond},
		"report": {OpenConnections: 1, MaxLifetimeClosed: 9},
	}
	m.Registry.MustRegister(newPoolCollector("demo", nil, func() map[string]sql.DBStats { return stats }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()

	for _, want := range []string{
		`demo_db_pool_open{name="main"} 7`,
		`demo_db_pool_in_use{name="main"} 3`,
		`demo_db_pool_idle{name="main"} 4`,
		`demo_db_pool_max_open{name="main"} 50`,
		`demo_db_pool_wait_total{name="main"} 2`,
		`demo_db_pool_wait_duration_seconds_total{name="main"} 1.5`,
		`demo_db_pool_closed_max_lifetime_total{name="report"} 9`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("导出里应有 %s\n实际=\n%s", want, out)
		}
	}
}

func TestPoolCollector_LabelsPerInstance(t *testing.T) {
	// 多实例时必须分得开，否则连接数是几个池子加起来的，看不出是谁满了
	m, closer, _ := xmetric.New(xmetric.Config{})
	defer closer.Close()

	m.Registry.MustRegister(newPoolCollector("", nil, func() map[string]sql.DBStats {
		return map[string]sql.DBStats{"a": {OpenConnections: 1}, "b": {OpenConnections: 2}}
	}))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()
	if !strings.Contains(out, `db_pool_open{name="a"} 1`) || !strings.Contains(out, `db_pool_open{name="b"} 2`) {
		t.Errorf("每个实例该是独立的时间序列\n实际=\n%s", out)
	}
}

func TestPoolStats_EmptyWithoutInstances(t *testing.T) {
	withClients(t, nil)
	if got := poolStats(); len(got) != 0 {
		t.Errorf("没有实例时应为空，got=%v", got)
	}
}

func TestPoolCollector_IncludesConstLabels(t *testing.T) {
	// 自建指标要和框架内置指标带上同样的环境/集群标签，否则看板上对不起来
	m, closer, _ := xmetric.New(xmetric.Config{})
	defer closer.Close()

	m.Registry.MustRegister(newPoolCollector("", prometheus.Labels{"env": "prod"},
		func() map[string]sql.DBStats { return map[string]sql.DBStats{"a": {OpenConnections: 1}} }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if out := w.Body.String(); !strings.Contains(out, `db_pool_open{env="prod",name="a"} 1`) {
		t.Errorf("常量标签应带上\n实际=\n%s", out)
	}
}

func TestPoolStats_ReadsLiveInstances(t *testing.T) {
	// collector 是抓取时才调 poolStats 的：这里要是读不出来，
	// /metrics 上连接池那一组指标就是永远空的，而没有任何报错
	pool, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(42)

	withClients(t, map[string]*gorm.DB{
		"main": {Config: &gorm.Config{ConnPool: pool}},
	})

	got := poolStats()
	if len(got) != 1 {
		t.Fatalf("应当读到一个实例，got=%v", got)
	}
	if got["main"].MaxOpenConnections != 42 {
		t.Errorf("读到的不是这个池子的状态，got=%+v", got["main"])
	}
}

func TestPoolStats_SkipsInstancesWithoutPool(t *testing.T) {
	// 一个实例读不出状态，不该让其余实例的指标也一起消失
	pool, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	withClients(t, map[string]*gorm.DB{
		"good": {Config: &gorm.Config{ConnPool: pool}},
		"bad":  {Config: &gorm.Config{}}, // 没有连接池，DB() 会报错
	})

	got := poolStats()
	if _, ok := got["good"]; !ok {
		t.Errorf("正常的实例应当照常读出来，got=%v", got)
	}
	if _, ok := got["bad"]; ok {
		t.Errorf("读不出状态的实例不该出现在结果里，got=%v", got)
	}
}

// okDriver 一个永远连得上、什么都不做的 database/sql 驱动。
// 连接池指标要从真实的 *sql.DB 上读，又不值得为此起一个数据库
type okDriver struct{}

func (okDriver) Open(string) (driver.Conn, error) { return okConn{}, nil }

type okConn struct{}

func (okConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (okConn) Close() error                        { return nil }
func (okConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func init() { sql.Register("xgorm-ok", okDriver{}) }

// okDialector 用 okDriver 建连接池的方言，New 走得完全程
type okDialector struct{ stubDialector }

func (okDialector) Initialize(db *gorm.DB) (err error) {
	db.ConnPool, err = sql.Open("xgorm-ok", "")
	return err
}

func TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall(t *testing.T) {
	// collector 是进程级的一个，抓取时遍历全部实例：不看实例自己的开关，
	// Metric: false 就是一句空话。第二轮是同一进程里再走一遍生命周期——
	// xmetric 换了新的 Registry，collector 得跟着挂上去，否则连接池指标全丢
	withDialect(t, Dialect{Name: "okdb", Open: func(string) gorm.Dialector { return okDialector{} }})
	on := DefaultClientConfig()
	on.Driver, on.DSN = "okdb", "okdb://h/d"
	off := on
	off.Metric = false

	for round := 1; round <= 2; round++ {
		m, closer, err := xmetric.New(xmetric.Config{})
		if err != nil {
			t.Fatal(err)
		}
		m.Install()
		if err := install(context.Background(), Config{Clients: map[string]ClientConfig{"on": on, "off": off}}); err != nil {
			t.Fatal(err)
		}
		out := testkit.Scrape(m.Handler)
		if !strings.Contains(out, `db_pool_open{name="on"}`) {
			t.Errorf("第 %d 轮：开了 Metric 的实例该导出\n实际=\n%s", round, out)
		}
		if strings.Contains(out, `name="off"`) {
			t.Errorf("第 %d 轮：Metric: false 的实例不该导出\n实际=\n%s", round, out)
		}
		if err := reg.Close(); err != nil {
			t.Fatal(err)
		}
		closer.Close()
	}
}
