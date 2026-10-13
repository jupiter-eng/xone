package e2e

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 写入和查询走的是第三个实例：xgorm/clickhouse/README.md「配置」——匿名 import xgorm/clickhouse、配置里 Driver: clickhouse，
// 「拿到的仍然是原生的 *gorm.DB，配置项和多实例写法都一样」。
// 数据直连 ClickHouse 核对：一条 INSERT 写进去的一批行都在，按主键点查、按 name 聚合都对
func TestClickHouse_WritesAndReadsViaThirdInstance_DataLandsInClickHouse(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{})
	name := "ch-" + harness.NewID()

	ids, _ := chInsert(t, p, name, 10, 20, 30)
	got := chRows(t, p, ids...)
	if len(got) != 3 {
		t.Fatalf("一条 INSERT 写了 3 行，直连 ClickHouse 应读到 3 行，实际 %+v", got)
	}
	for i, e := range got {
		if want := (chEvent{ID: ids[i], Name: name, Value: int64(10 * (i + 1))}); e != want {
			t.Errorf("第 %d 行应是 %+v，实际 %+v", i+1, want, e)
		}
	}

	r := p.Get(t, fmt.Sprintf("/ch/events/%d", ids[1]))
	var e chEvent
	r.JSON(t, &e)
	if r.Status != http.StatusOK || e != got[1] {
		t.Errorf("GET /ch/events/%d 应 200 回 %+v，实际 %v", ids[1], got[1], r)
	}
	if r := p.Get(t, "/ch/events/1"); r.Status != http.StatusNotFound {
		t.Errorf("没有的 id 应 404，实际 %v", r)
	}

	r = p.Get(t, "/ch/stats?name="+name)
	var s struct {
		Count uint64 `json:"count"`
		Sum   int64  `json:"sum"`
	}
	r.JSON(t, &s)
	if r.Status != http.StatusOK || s.Count != 3 || s.Sum != 60 {
		t.Errorf("按 name 聚合应是 count=3 sum=60，实际 %v", r)
	}

	// 三个实例都在：PG、MySQL 那两组照常
	if u := createUser(t, p, "pg-side", "pg@example.com"); u.ID <= 0 {
		t.Errorf("PG 那一组接口应照常")
	}
	mysqlCreate(t, p, "my-side", "my@example.com")
	if r := p.Get(t, "/dep?target=ch"); r.Status != http.StatusOK {
		t.Errorf("GET /dep?target=ch 应 200，实际 %v", r)
	}

	// 没激活 ch 那份 profile 的进程里没有这个实例：/ch/... 回 503，不 panic
	plain := harness.Start(t, harness.Options{})
	if r := plain.Get(t, "/dep?target=ch"); r.Status != http.StatusServiceUnavailable || !strings.Contains(string(r.Body), "not configured") {
		t.Errorf("没配 ch 实例时 /dep?target=ch 应 503 说没配，实际 %v", r)
	}
}

// xgorm/clickhouse 包文档与 xgorm/clickhouse/README.md「配置」：驱动在初始化时查的那次 SELECT version() 挪进了建连探测，
// 「版本号照样设进 Dialector，驱动靠它判断老版本不支持的改列名（< 20.4）和列精度（< 21.11）」。
// 24.8 上：Dialector.Version 就是服务端的 version()，两个开关都是 false（新版本全都支持）
func TestClickHouse_ProbedVersionSetOnDialector_BothLegacyFlagsOffOn24_8(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{})
	r := p.Get(t, "/ch/version")
	var v struct {
		Dialector string `json:"dialector_version"`
		Server    string `json:"server_version"`
		NoRename  bool   `json:"dont_support_rename_column"`
		NoPrec    bool   `json:"dont_support_column_precision"`
	}
	r.JSON(t, &v)
	if r.Status != http.StatusOK || v.Server == "" {
		t.Fatalf("GET /ch/version 应 200 回服务端版本，实际 %v", r)
	}
	if v.Dialector != v.Server {
		t.Errorf("文档说版本号照样设进 Dialector：Dialector.Version 应是服务端的 %q，实际 %q", v.Server, v.Dialector)
	}
	if !strings.HasPrefix(v.Server, "24.8.") {
		t.Logf("注意：本用例按 ClickHouse 24.8 写，实际连的是 %s", v.Server)
	}
	if v.NoRename || v.NoPrec {
		t.Errorf("%s 支持改列名（>= 20.4）和列精度（>= 21.11），两个开关都该是 false，实际 rename=%v precision=%v", v.Server, v.NoRename, v.NoPrec)
	}
	t.Logf("数字：Dialector.Version=%s DontSupportRenameColumn=%v DontSupportColumnPrecision=%v", v.Dialector, v.NoRename, v.NoPrec)
}

// xgorm/README.md XGorm.Log：「记的是带占位符的 SQL，不含参数值」「占位符就是发给数据库的那样」——ClickHouse 是 ?；
// 日志里的语句就是 Span 的 db.query.text。Log 按实例生效：只给 ch 开，PG、MySQL 的 SQL 一条都不该记。
//
// 聚合那一条走的是 GORM 的 Scan：见子测试「Scan」
func TestClickHouse_SQLLogHasPlaceholdersNotArgs_MatchesSpanDbQueryText(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{Spans: true, Overlay: sqlLog("ch")})
	name := "sql-" + harness.NewID()

	sqlOf := func(t *testing.T, tid string) []harness.Log {
		t.Helper()
		accessLog(t, p, tid) // 访问日志在 handler 返回之后才写，读到它时这条请求的 SQL 日志都到了
		serverSpan(t, p, tid)
		return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "SQL" && l.Str("trace_id") == tid })
	}
	check := func(t *testing.T, what, spanName, want string, r harness.Response, values ...string) {
		t.Helper()
		tid := traceIDOf(t, r)
		logs := sqlOf(t, tid)
		if len(logs) != 1 {
			t.Fatalf("%s 应记 1 条 SQL 日志，实际 %d 条", what, len(logs))
		}
		logged := logs[0].Str("sql")
		if logged != want {
			t.Errorf("%s 的 SQL 日志应是带 ? 占位符的 %q，实际 %q", what, want, logged)
		}
		mustNotContain(t, what+" 的 SQL 日志", logs[0].Line, values...)
		spans := spansNamed(traceSpans(t, p, tid), spanName)
		if len(spans) != 1 {
			t.Fatalf("%s 的链路上应有一个 %s，实际 %s", what, spanName, spanNames(traceSpans(t, p, tid)))
		}
		if sent := spans[0].Str("db.query.text"); sent != want {
			t.Errorf("%s：Span 的 db.query.text 应是同一条带占位符的语句 %q，实际 %q", what, want, sent)
		}
		t.Logf("数字：%s 记下的 SQL：%s", what, logged)
	}

	// 两行只有一组占位符：gorm.io/driver/clickhouse v0.7.0 的 Create 按一行预备、逐行 Append 成一个批次
	// （native 协议的批量写入），发出去的语句就是这一条
	// 值要够长：mustNotContain 查的是整行日志，4 位数会撞上时间戳（实测撞过 18.434395505 里的 4343）。
	// 11 位的数在纳秒（9 位）、十六进制的 trace_id / span_id 里都凑不出来
	ids, r := chInsert(t, p, name, 73914062851, 73914062852)
	check(t, "POST /ch/events", "gorm.create", "INSERT INTO `"+p.Table+"` (`name`,`value`,`id`) VALUES (?,?,?)", r,
		name, "73914062851", "73914062852", fmt.Sprint(ids[0]))
	check(t, "GET /ch/events/:id", "gorm.query", "SELECT * FROM `"+p.Table+"` WHERE id = ? LIMIT ?",
		p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0])), fmt.Sprint(ids[0]))

	t.Run("PG和MySQL没开Log", func(t *testing.T) {
		if logs := sqlOf(t, traceIDOf(t, p.Get(t, "/users/999999999?cache=off"))); len(logs) != 0 {
			t.Errorf("只给 ch 实例开了 Log，default（PG）的 SQL 不该记，实际 %d 条", len(logs))
		}
		if logs := sqlOf(t, traceIDOf(t, p.Get(t, "/mysql/users/999999999"))); len(logs) != 0 {
			t.Errorf("只给 ch 实例开了 Log，mysql 的 SQL 不该记，实际 %d 条", len(logs))
		}
	})

	// GORM 的 Scan（v1.31.2 finisher_api.go:537）执行期间把 Logger 换成 logger.Recorder，
	// 执行完再把 Recorder 记下的 SQL 交给我们的 Logger。Recorder 不走 xgorm 的 ParamsFilter，
	// 它的 ParamsFilter 只认包级的 logger.RecorderParamsFilter（默认原样返回参数），
	// 于是交过来的是 Explain 过、参数已经代进去的 SQL。这条原先是 KNOWN BUG，改之前实测：
	//
	//	SELECT count() AS n, sum(value) AS s FROM `e2e_…` WHERE name = 'sql-3fa9c1d2b7e4'
	//
	// 这不是 ClickHouse 独有的：xgorm 的每个实例上，Raw(...).Scan(...) 和 Table(...).Select(...).Scan(...)
	// 都这样（服务里 MySQL 的 SELECT SLEEP(?) 记下来就是 SELECT SLEEP(1.5)）。xgorm 的 init 把
	// logger.RecorderParamsFilter 换成了不交出参数的那个（xgorm/README.md「通用」）。
	// Span 不受影响：db.query.text 取自 Statement.SQL，照样是占位符
	t.Run("Scan", func(t *testing.T) {
		r := p.Get(t, "/ch/stats?name="+name)
		tid := traceIDOf(t, r)
		logs := sqlOf(t, tid)
		if len(logs) != 1 {
			t.Fatalf("GET /ch/stats 应记 1 条 SQL 日志，实际 %d 条", len(logs))
		}
		want := "SELECT count() AS n, sum(value) AS s FROM `" + p.Table + "` WHERE name = ?"
		spans := spansNamed(traceSpans(t, p, tid), "gorm.row")
		if len(spans) != 1 || spans[0].Str("db.query.text") != want {
			t.Errorf("Span 的 db.query.text 应是 %q，实际 %v", want, spans)
		}
		if logged := logs[0].Str("sql"); logged != want {
			t.Errorf("GORM 的 Scan 绕开了 xgorm 的 ParamsFilter？SQL 日志应是带 ? 占位符的 %q，实际 %q", want, logged)
		}
		mustNotContain(t, "GET /ch/stats 的 SQL 日志", logs[0].Line, name)
		t.Logf("数字：Scan 记下的 SQL：%s", logs[0].Str("sql"))
	})
}

// Span 的属性（xgorm/trace.go，OTel 数据库语义约定 v1.43.0；xgorm/README.md「链路」）：
// db.system.name=clickhouse、db.namespace 是库名、server.address / server.port 从 DSN 解出来分开记、
// db.query.text 带占位符、db.operation.name 是语句的第一个关键字。父是服务端 Span。
// 同一个请求里三个实例各报各的连接信息
func TestClickHouse_SQLSpanCarriesThisInstanceConnInfo(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{Spans: true})
	name := "span-" + harness.NewID()
	ids, ins := chInsert(t, p, name, 7)
	hit := p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0]))
	miss := p.Get(t, "/ch/events/1")
	agg := p.Get(t, "/ch/stats?name="+name)
	host, port, _ := net.SplitHostPort(harness.CHAddr())

	for _, c := range []struct {
		what, span, op, query string
		r                     harness.Response
	}{
		{"POST /ch/events", "gorm.create", "INSERT", "INSERT INTO `" + p.Table + "` (`name`,`value`,`id`) VALUES (?,?,?)", ins},
		{"GET /ch/events/:id", "gorm.query", "SELECT", "SELECT * FROM `" + p.Table + "` WHERE id = ? LIMIT ?", hit},
		{"GET 没有的 id", "gorm.query", "SELECT", "SELECT * FROM `" + p.Table + "` WHERE id = ? LIMIT ?", miss},
		{"GET /ch/stats", "gorm.row", "SELECT", "SELECT count() AS n, sum(value) AS s FROM `" + p.Table + "` WHERE name = ?", agg},
	} {
		t.Run(c.what, func(t *testing.T) {
			tid := traceIDOf(t, c.r)
			srv := serverSpan(t, p, tid)
			spans := spansNamed(traceSpans(t, p, tid), c.span)
			if len(spans) != 1 {
				t.Fatalf("链路上应有一个 %s，实际 %s", c.span, spanNames(traceSpans(t, p, tid)))
			}
			s := spans[0]
			if s.ParentSpanID != srv.SpanID || s.Kind != "client" {
				t.Errorf("%s 应是服务端 Span 的子 Span、kind=client，实际 parent=%s kind=%s", c.span, s.ParentSpanID, s.Kind)
			}
			for k, want := range map[string]string{
				"db.system.name": "clickhouse", "db.namespace": "xone_e2e", "server.address": host, "server.port": port,
				"db.operation.name": c.op, "db.query.text": c.query,
			} {
				if got := s.Str(k); got != want {
					t.Errorf("%s 的 %s 应是 %q，实际 %q", c.span, k, want, got)
				}
			}
			if s.StatusCode == "Error" {
				t.Errorf("%s 不该标成错误（没查到记录也不算），实际 status=%s %s", c.span, s.StatusCode, s.StatusDescription)
			}
			mustNotContain(t, "Span "+c.span+" 的属性", fmt.Sprint(s.Attributes), name, fmt.Sprint(ids[0]))
			// ClickHouse 的驱动对写入永远报 0 行（clickhouse-go v2.48.0 stdDriver.ExecContext 返回 driver.RowsAffected(0)），
			// db.rows_affected 在这个实例上没有意义，只记下来
			t.Logf("数字：%s 的 db.rows_affected=%s", c.what, s.Str("db.rows_affected"))
		})
	}

	t.Run("同一进程里PG和MySQL的Span报各自的连接信息", func(t *testing.T) {
		for _, c := range []struct{ path, system, addr string }{
			{"/users/999999999?cache=off", "postgresql", harness.PGAddr()},
			{"/mysql/users/999999999", "mysql", harness.MySQLAddr()},
		} {
			tid := traceIDOf(t, p.Get(t, c.path))
			serverSpan(t, p, tid)
			q := spansNamed(traceSpans(t, p, tid), "gorm.query")
			h, _, _ := net.SplitHostPort(c.addr)
			if len(q) != 1 || q[0].Str("db.system.name") != c.system || q[0].Str("server.address") != h {
				t.Errorf("%s 的 gorm.query 应报 %s / %s，实际 %v", c.path, c.system, h, q)
			}
		}
	})
}

// xgorm/README.md「通用」：ClickHouse 的服务端错误原文里同样有参数值——
// 实测 24.8 把 value 转 Int64 失败是 code: 6, message: Cannot parse string '<值>' as Int64 …。
// native 协议下驱动返回 *clickhouse.Exception，方言认得出错误码（xgorm/clickhouse errorCode）：
// SQL failed 的 error 字段、Span 的状态和属性里只有 6；返回给业务的错误原样不变
func TestClickHouse_ArgsInServerErrorKeptOutOfSQLLogAndSpan(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	p := chStart(t, harness.Options{Spans: true, Overlay: sqlLog("ch")})
	value := "leak-" + harness.NewID()
	r := p.Do(t, http.MethodPost, "/bad-sql", map[string]string{"db": "ch", "value": value})
	if r.Status != http.StatusInternalServerError {
		t.Fatalf("这条 SQL 应当失败，实际 %v", r)
	}
	if m := r.Map(t); m["error_has_value"] != true {
		t.Errorf("返回给业务的错误要原样不变（原文里带着值），实际 %v", m)
	}
	tid := traceIDOf(t, r)
	accessLog(t, p, tid)
	logs := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "SQL failed" && l.Str("trace_id") == tid })
	if len(logs) != 1 {
		t.Fatalf("应记 1 条 SQL failed，实际 %d 条", len(logs))
	}
	mustNotContain(t, "SQL failed 日志", logs[0].Line, value)
	if logs[0].Str("error_code") != "6" || logs[0].Str("error") != "clickhouse error 6 (message omitted, it may contain parameter values)" {
		t.Errorf("SQL failed 日志应只记错误码 6，实际 %s", logs[0].Line)
	}
	serverSpan(t, p, tid)
	spans := spansNamed(traceSpans(t, p, tid), "gorm.raw")
	if len(spans) != 1 {
		t.Fatalf("链路上应有一个 gorm.raw，实际 %s", spanNames(traceSpans(t, p, tid)))
	}
	s := spans[0]
	if s.StatusCode != "Error" || s.Str("db.response.status_code") != "6" || s.Str("error.type") != "6" {
		t.Errorf("Span 应标成 Error 并带上错误码 6，实际 status=%s attrs=%v", s.StatusCode, s.Attributes)
	}
	mustNotContain(t, "Span gorm.raw", fmt.Sprint(s.StatusDescription, s.Attributes, s.Events), value)
	t.Logf("数字：SQL failed error=%q error_code=%s", logs[0].Str("error"), logs[0].Str("error_code"))
}

// xgorm/README.md：「Metric: true # 连接池指标 db_pool_*，按实例生效：Metric: false 的实例不出现在 /metrics 里」。
// ch 实例的池子按 name="ch" 报：配 MaxOpenConns: 7 就是 7，别的实例仍是默认的 50
func TestClickHouse_PoolMetricsLabeledByInstance_MetricTogglePerInstance(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pool := []string{
		"e2e_db_pool_open", "e2e_db_pool_in_use", "e2e_db_pool_idle", "e2e_db_pool_max_open",
		"e2e_db_pool_wait_total", "e2e_db_pool_wait_duration_seconds_total",
		"e2e_db_pool_closed_max_idle_total", "e2e_db_pool_closed_max_lifetime_total",
	}
	t.Run("三个实例各报各的", func(t *testing.T) {
		t.Parallel()
		p := chStart(t, harness.Options{Overlay: chOverlay("MaxOpenConns: 7")})
		chInsert(t, p, "metric", 1)
		m := p.Metrics(t)
		for _, n := range pool {
			for _, inst := range []string{"default", "mysql", "ch"} {
				if len(m.Find(n, "name", inst)) != 1 {
					t.Errorf("/metrics 里应有 %s{name=%q}", n, inst)
				}
			}
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "ch"); got != 7 {
			t.Errorf("ch 实例配了 MaxOpenConns: 7，e2e_db_pool_max_open{name=\"ch\"} 应是 7，实际 %v", got)
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "mysql"); got != 50 {
			t.Errorf("MaxOpenConns 只改了 ch 实例，mysql 应仍是默认的 50，实际 %v", got)
		}
		if got := m.Sum("e2e_db_pool_open", "name", "ch"); got < 1 {
			t.Errorf("跑过 ClickHouse 的 SQL 之后 e2e_db_pool_open{name=\"ch\"} 应至少是 1，实际 %v", got)
		}
	})
	t.Run("ch配Metric: false就不出现", func(t *testing.T) {
		t.Parallel()
		p := chStart(t, harness.Options{Overlay: chOverlay("Metric: false")})
		chInsert(t, p, "metric", 1)
		m := p.Metrics(t)
		for _, n := range pool {
			if len(m.Find(n, "name", "ch")) != 0 {
				t.Errorf("ch 实例配了 Metric: false，/metrics 里不该有 %s{name=\"ch\"}", n)
			}
			if len(m.Find(n, "name", "mysql")) != 1 {
				t.Errorf("mysql 实例没关 Metric，/metrics 里应仍有 %s{name=\"mysql\"}", n)
			}
		}
	})
}

// ClickHouse 的密码不出现在任何输出里：docs/observability.md「框架自己的日志」、xgorm/clickhouse/README.md「配置」
// 「这些错误一律不回显 DSN」。SQL 日志、debug、请求体日志、Span 全开，走一圈写入、点查、聚合、报错，
// 中途断一次 ClickHouse（这时的错误最可能把连接串带出来），再核对 stdout / stderr、Span 文件、/metrics、响应体
func TestClickHouse_PasswordNeverInLogsSpansMetricsOrResponses(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pw := harness.CHPassword()
	chp := harness.NewProxy(t, harness.CHAddr())
	dsn := harness.CHDSN(chp.Addr())
	p := chStart(t, harness.Options{
		CHAddr: chp.Addr(), Spans: true,
		Overlay: sqlLog("ch") + debugLogs + payloadLogs,
	})
	var bodies []string
	keep := func(r harness.Response) harness.Response { bodies = append(bodies, string(r.Body)); return r }

	ids, r := chInsert(t, p, "pw-check", 1, 2)
	keep(r)
	keep(p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0])))
	keep(p.Get(t, "/ch/stats?name=pw-check"))
	keep(p.Get(t, "/ch/version"))
	keep(p.Do(t, http.MethodPost, "/bad-sql", map[string]string{"db": "ch", "value": "x"}))
	keep(p.Get(t, "/stuck?ms=1&ch=1"))

	chp.Cut()
	for _, path := range []string{fmt.Sprintf("/ch/events/%d", ids[0]), "/dep?target=ch", "/ch/stats?name=x"} {
		if r := keep(p.Get(t, path)); r.Status < 500 {
			t.Errorf("ClickHouse 断开时 %s 应报错，实际 %v", path, r)
		}
	}
	if r := keep(p.PostJSON(t, "/ch/events", map[string]any{"name": "while-cut", "values": []int{1}})); r.Status != http.StatusServiceUnavailable {
		t.Errorf("ClickHouse 断开时 POST /ch/events 应 503，实际 %v", r)
	}
	keep(p.Get(t, "/stuck?ms=1&ch=1"))
	chp.Restore()
	chWaitDep(t, p, 10*time.Second)

	if resp, err := http.Get(p.URL("/metrics")); err == nil {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		resp.Body.Close()
		mustNotContain(t, "/metrics", b.String(), pw)
	}
	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Errorf("SIGTERM 之后应以 0 退出，实际 %v", exit)
	}

	out := p.Output()
	spans, _ := os.ReadFile(p.SpanFile)
	mustNotContain(t, "进程的 stdout / stderr", out, pw, dsn)
	mustNotContain(t, "Span 文件", string(spans), pw)
	mustNotContain(t, "响应体", strings.Join(bodies, "\n"), pw)
	// 反过来确认这一圈真的走到了会记 SQL、会报错的路径，不是空跑
	for _, msg := range []string{"SQL", "SQL failed", "read ch event failed", "insert ch events failed", "xgorm connected"} {
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == msg })) == 0 {
			t.Errorf("这一圈应该产生过 %q 日志，一条都没有：核对范围不够", msg)
		}
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("这一圈应该在输出里留下过 connection refused，一条都没有：核对范围不够")
	}
	t.Logf("数字：核对了 %d 字节输出、%d 字节 Span、%d 个响应体", len(out), len(spans), len(bodies))
}

// xgorm/clickhouse/README.md「配置」的 DSN 规则（xgorm/clickhouse resolve）：
//
//   - 必须是 clickhouse:// tcp:// http:// https:// 四种 scheme 之一的 URL，否则启动失败——包括裸的 host:port、
//     scheme 拼错、前面多一个空格；
//   - 启动时还会用驱动自己的解析器把 DSN 过一遍（比如 https:// 必须配 secure=true）；
//   - 这些错误一律不回显 DSN：驱动和 url.Parse 的原始错误里带着整串 DSN，连同明文密码。
//
// 每种错法各起一个进程：非 0 退出、从没开始监听、错误说得清是哪一条，输出里没有密码和 DSN；
// 一次连接都不该发出去（代理数得到）
func TestClickHouse_DSNTypoFailsStartup_NamesTheItem_NoDSNOrPasswordEcho(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	secret := "dsn-secret-" + harness.NewID()
	chp := harness.NewProxy(t, harness.CHAddr())
	addr := chp.Addr()
	notURL := "DSN must be a URL starting with clickhouse://, tcp://, http:// or https://"
	malformed := "failed to parse DSN, check the format of XGorm"
	for _, c := range []struct {
		name, dsn, want string
	}{
		{"裸的host:port", "xone:" + secret + "@" + addr + "/xone_e2e", notURL},
		{"scheme拼错", "clickhous://xone:" + secret + "@" + addr + "/xone_e2e", notURL},
		{"前面多一个空格", " " + harness.CHDSNWith("clickhouse", addr, secret), notURL},
		{"https没配secure", harness.CHDSNWith("https", addr, secret), malformed},
		{"http配了secure", harness.CHDSNWith("http", addr, secret) + "?secure=true", malformed},
		{"查询串里有字面百分号", harness.CHDSNWith("clickhouse", addr, "x") + "?password=" + secret + "%zz", "a literal % in a password must be written as %25"},
		{"http_proxy写错", harness.CHDSNWith("http", addr, secret) + "?http_proxy=" + "http%3A%2F%2Fu%3A" + secret + "%40%5B%3A%3A1", malformed},
		{"dial_timeout写错", harness.CHDSNWith("clickhouse", addr, secret) + "?dial_timeout=" + secret, malformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": c.dsn}})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xgorm config failed", `instance "ch"`, c.want)
			mustNotContain(t, "进程的 stdout / stderr", p.Output(), secret, strings.TrimSpace(c.dsn))
			if exit.Uptime > faultStartSlack {
				t.Errorf("DSN 写错是配置错误，不该去连，应在 %v 内退出，实际 %v", faultStartSlack, exit.Uptime)
			}
			t.Logf("数字：%s：%v 后以 %d 退出；错误：%s", c.name, exit.Uptime.Round(time.Millisecond), exit.Code, lastLine(stderr))
		})
	}
	t.Cleanup(func() {
		if n := chp.Accepted(); n != 0 {
			t.Errorf("DSN 写错时不该发出任何连接，代理实际收到 %d 个", n)
		}
	})
}

// xgorm/clickhouse/README.md「配置」：「DialTimeout: 500ms # 注入 DSN 的 dial_timeout，DSN 里已写的不覆盖」。
// 主机宕机（Blackhole）时新建连接的 SYN 没有回音，拨号在 dial_timeout 失败——三种来源各起一个进程：
//
//	默认             dial_timeout=500ms（DialTimeout 的默认值注进去）
//	配置（只改 ch）   DialTimeout: 1s
//	DSN 里写了        dial_timeout=1500ms，配置里同时写着 DialTimeout: 1s，以 DSN 为准
//
// 池里的连接先用短截止时间耗掉：它们卡在读上，dial_timeout 管不到
func TestClickHouse_DialTimeoutFromDSN_ElseInstanceDialTimeout(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	for _, c := range []struct {
		name     string
		dsnQuery string
		overlay  string
		connect  time.Duration
	}{
		{"默认", "", "", chDialTimeout},
		{"配置", "", chOverlay("DialTimeout: 1s"), time.Second},
		{"DSN里写了", "?dial_timeout=1500ms", chOverlay("DialTimeout: 1s"), 1500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			chp := harness.NewProxy(t, harness.CHAddr())
			p := chStart(t, harness.Options{Overlay: c.overlay, Env: map[string]string{"E2E_CH_DSN": harness.CHDSN(chp.Addr()) + c.dsnQuery}})
			chp.Blackhole()
			for i := 0; ; i++ {
				if i == 10 {
					t.Fatal("10 次短截止时间的查询都没走到新建连接")
				}
				if r := chDep(p, "100ms", 10*time.Second); strings.Contains(r.Error, "dial tcp") {
					break
				}
			}
			r := chDep(p, "", 20*time.Second)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "dial tcp "+chp.Addr()) {
				t.Fatalf("主机宕机：新建连接应失败（503，dial tcp %s …），实际 %v", chp.Addr(), r)
			}
			if r.Server < c.connect-20*time.Millisecond || r.Server > c.connect+faultSlack {
				t.Errorf("dial_timeout 应是 %v（%s），新建连接应在那时失败，实际 %v：%s", c.connect, c.name, r.Server, r.Error)
			}
			t.Logf("数字：%s：主机宕机时新建连接在 %s 失败（dial_timeout=%v）：%s", c.name, faultMS(r.Server), c.connect, r.Error)
		})
	}
}

// 多主机 DSN（clickhouse://u:p@h1:9000,h2:9000/db）：驱动按逗号切成几个地址、依次去连（connection_open_strategy
// 默认 in_order），第一个连不上就换下一个。xgorm/README.md「链路」：server.address / server.port
// 「从 DSN 解出的主机、端口，分开记」；建连日志的 addr 是「主机:端口」。和 PostgreSQL 的多主机一样记第一个。
//
// 这条原先是 bug：resolve 把 URL 的整个 Host（"h1:9000,h2:9000"）当成地址，net.SplitHostPort 解不开，
// Span 里 server.address 是整串、没有 server.port，建连日志和错误里的 addr 也是整串
func TestClickHouse_MultiHostDSN_FirstThenNext_SpanAndLogRecordFirstHost(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	first, second := harness.NewProxy(t, harness.CHAddr()), harness.NewProxy(t, harness.CHAddr())
	dsn := strings.Replace(harness.CHDSN(first.Addr()), first.Addr(), first.Addr()+","+second.Addr(), 1)
	p := chStart(t, harness.Options{Spans: true, Env: map[string]string{"E2E_CH_DSN": dsn}})

	ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm connected" && l.Str("name") == "ch" })
	if len(ls) != 1 || ls[0].Str("addr") != first.Addr() {
		t.Errorf("建连日志的 addr 应是第一个主机 %s，实际 %v", first.Addr(), ls)
	}
	ids, r := chInsert(t, p, "multi", 1)
	tid := traceIDOf(t, r)
	serverSpan(t, p, tid)
	host, port, _ := net.SplitHostPort(first.Addr())
	if s := spansNamed(traceSpans(t, p, tid), "gorm.create"); len(s) != 1 || s[0].Str("server.address") != host || s[0].Str("server.port") != port {
		t.Errorf("Span 的 server.address / server.port 应是第一个主机的 %s / %s，实际 %v", host, port, s)
	}
	if first.Accepted() == 0 || second.Accepted() != 0 {
		t.Errorf("in_order：应只连第一个主机，实际第一个收到 %d 个连接、第二个 %d 个", first.Accepted(), second.Accepted())
	}

	first.Cut()
	if r := p.Get(t, fmt.Sprintf("/ch/events/%d", ids[0])); r.Status != http.StatusOK {
		t.Errorf("第一个主机挂了，驱动应换到第二个，查询照常 200，实际 %v", r)
	}
	if second.Accepted() == 0 {
		t.Errorf("第一个主机挂了之后应连到第二个，第二个一个连接都没收到")
	}
	t.Logf("数字：第一个主机挂了之后，第二个收到 %d 个连接", second.Accepted())
}
