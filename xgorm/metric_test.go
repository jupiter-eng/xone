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
	// A pool's state is instantaneous, which is why it is read when scraped rather than pushed into
	// a Gauge on a timer
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
			t.Errorf("%s should be in the export\ngot=\n%s", want, out)
		}
	}
}

func TestPoolCollector_LabelsPerInstance(t *testing.T) {
	// Several instances have to stay apart, or the connection count is several pools added together
	// and there is no telling which one filled up
	m, closer, _ := xmetric.New(xmetric.Config{})
	defer closer.Close()

	m.Registry.MustRegister(newPoolCollector("", nil, func() map[string]sql.DBStats {
		return map[string]sql.DBStats{"a": {OpenConnections: 1}, "b": {OpenConnections: 2}}
	}))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()
	if !strings.Contains(out, `db_pool_open{name="a"} 1`) || !strings.Contains(out, `db_pool_open{name="b"} 2`) {
		t.Errorf("each instance should be its own time series\ngot=\n%s", out)
	}
}

func TestPoolStats_EmptyWithoutInstances(t *testing.T) {
	withClients(t, nil)
	if got := poolStats(); len(got) != 0 {
		t.Errorf("with no instance it should be empty, got=%v", got)
	}
}

func TestPoolCollector_IncludesConstLabels(t *testing.T) {
	// Metrics of your own carry the same environment and cluster labels as the framework's built-in
	// ones, or they do not line up on a dashboard
	m, closer, _ := xmetric.New(xmetric.Config{})
	defer closer.Close()

	m.Registry.MustRegister(newPoolCollector("", prometheus.Labels{"env": "prod"},
		func() map[string]sql.DBStats { return map[string]sql.DBStats{"a": {OpenConnections: 1}} }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if out := w.Body.String(); !strings.Contains(out, `db_pool_open{env="prod",name="a"} 1`) {
		t.Errorf("the constant labels should be carried\ngot=\n%s", out)
	}
}

func TestPoolStats_ReadsLiveInstances(t *testing.T) {
	// The collector calls poolStats only when scraped: failing to read here leaves the connection
	// pool group of /metrics empty forever, with no error anywhere
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
		t.Fatalf("one instance is expected, got=%v", got)
	}
	if got["main"].MaxOpenConnections != 42 {
		t.Errorf("what was read is not this pool's state, got=%+v", got["main"])
	}
}

func TestPoolStats_SkipsInstancesWithoutPool(t *testing.T) {
	// One instance whose state cannot be read must not take the other instances' metrics down with it
	pool, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	withClients(t, map[string]*gorm.DB{
		"good": {Config: &gorm.Config{ConnPool: pool}},
		"bad":  {Config: &gorm.Config{}}, // no pool, so DB() errors
	})

	got := poolStats()
	if _, ok := got["good"]; !ok {
		t.Errorf("a healthy instance should still be read, got=%v", got)
	}
	if _, ok := got["bad"]; ok {
		t.Errorf("an instance whose state cannot be read must not appear in the result, got=%v", got)
	}
}

// okDriver is a database/sql driver that always connects and does nothing.
// The pool metrics have to be read from a real *sql.DB, and that is not worth starting a database for.
type okDriver struct{}

func (okDriver) Open(string) (driver.Conn, error) { return okConn{}, nil }

type okConn struct{}

func (okConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (okConn) Close() error                        { return nil }
func (okConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func init() { sql.Register("xgorm-ok", okDriver{}) }

// okDialector is the dialect that builds a pool with okDriver, so New runs the whole way through
type okDialector struct{ stubDialector }

func (okDialector) Initialize(db *gorm.DB) (err error) {
	db.ConnPool, err = sql.Open("xgorm-ok", "")
	return err
}

func TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall(t *testing.T) {
	// The collector is one per process and walks every instance when scraped: without honoring each
	// instance's own switch, Metric: false would be an empty phrase. The second round is a second
	// lifecycle in the same process — xmetric has a new Registry and the collector has to be attached
	// to it, or every pool metric is lost
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
			t.Errorf("round %d: an instance with Metric on should export\ngot=\n%s", round, out)
		}
		if strings.Contains(out, `name="off"`) {
			t.Errorf("round %d: an instance with Metric: false must not export\ngot=\n%s", round, out)
		}
		if err := reg.Close(); err != nil {
			t.Fatal(err)
		}
		closer.Close()
	}
}
