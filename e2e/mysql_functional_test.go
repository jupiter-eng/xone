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

// 增删改查走的是第二个实例：xgorm/README.md XGorm「多实例」——C() 取 default，C("name") 取具名的那个。
// 数据直连 MySQL 核对，同时确认 PG 上的同名表里没有这些行：写进了 default 的话，
// 接口照样回 201 / 200，只看接口是看不出来的
func TestMySQL_CRUDViaSecondInstance_DataLandsInMySQL(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{})
	name, email := "my-"+harness.NewID(), "my-"+harness.NewID()+"@example.com"

	u, _ := mysqlCreate(t, p, name, email)
	if u.Name != name || u.Email != email {
		t.Errorf("POST /mysql/users 应回写进去的值，实际 %+v", u)
	}
	if row, ok := mysqlRow(t, p, u.ID); !ok || row != u {
		t.Fatalf("直连 MySQL 应读到刚写进去的 %+v，实际 %+v（有这一行：%v）", u, row, ok)
	}
	var pgRows int
	if err := harness.DB(t).QueryRow("SELECT COUNT(*) FROM " + p.Table).Scan(&pgRows); err != nil {
		t.Fatalf("直连 PG 数 %s：%v", p.Table, err)
	}
	if pgRows != 0 {
		t.Errorf("写的是 C(\"mysql\")，PG（default）上的 %s 应一行都没有，实际 %d 行", p.Table, pgRows)
	}

	r := p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID))
	var got mysqlUser
	r.JSON(t, &got)
	if r.Status != http.StatusOK || got != u {
		t.Errorf("GET /mysql/users/%d 应 200 回 %+v，实际 %v", u.ID, u, r)
	}
	if r := p.Get(t, "/mysql/users/999999999"); r.Status != http.StatusNotFound {
		t.Errorf("没有的用户应 404，实际 %v", r)
	}

	upd := mysqlUser{ID: u.ID, Name: name + "-v2", Email: "v2-" + email}
	if r := p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": upd.Name, "email": upd.Email}); r.Status != http.StatusOK {
		t.Errorf("PUT /mysql/users/%d 应 200，实际 %v", u.ID, r)
	}
	if row, _ := mysqlRow(t, p, u.ID); row != upd {
		t.Errorf("改完直连 MySQL 应读到 %+v，实际 %+v", upd, row)
	}
	// 改成和原来一样的值：MySQL 报的「改动行数」是 0，但这一行是在的，不该 404
	if r := p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": upd.Name, "email": upd.Email}); r.Status != http.StatusOK {
		t.Errorf("原样再改一次应照样 200，实际 %v", r)
	}
	if r := p.Do(t, http.MethodPut, "/mysql/users/999999999", map[string]string{"name": "x"}); r.Status != http.StatusNotFound {
		t.Errorf("改一个没有的用户应 404，实际 %v", r)
	}

	if r := p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil); r.Status != http.StatusNoContent {
		t.Errorf("DELETE /mysql/users/%d 应 204，实际 %v", u.ID, r)
	}
	if _, ok := mysqlRow(t, p, u.ID); ok {
		t.Errorf("删完直连 MySQL 应读不到 id=%d", u.ID)
	}
	if r := p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil); r.Status != http.StatusNotFound {
		t.Errorf("再删一次应 404，实际 %v", r)
	}

	// 两个实例都在：PG 那一组照常
	pu := createUser(t, p, "pg-side", "pg@example.com")
	if g, _, _ := getUser(t, p, pu.ID); g.Name != "pg-side" {
		t.Errorf("PG 那一组接口应照常，实际 %+v", g)
	}
}

// xgorm/README.md XGorm.Log：「记的是带占位符的 SQL，不含参数值」「占位符就是发给数据库的那样：MySQL 是 ?」；
// 日志里的语句就是 Span 的 db.query.text（xgorm/trace.go：「记的是带占位符的 SQL，不记 Statement.Vars」）。
//
// Log 按实例生效（它是 ClientConfig 的字段）：只给 mysql 开，default（PG）那边的 SQL 一条都不该记
func TestMySQL_SQLLogHasPlaceholdersNotArgs_MatchesSpan_LogPerInstance(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true, Overlay: sqlLog("mysql")})
	name, email := "sql-"+harness.NewID(), "sql-"+harness.NewID()+"@example.com"

	sqlOf := func(t *testing.T, tid string) []harness.Log {
		t.Helper()
		// 等这条请求的访问日志：它和 SQL 日志写的是同一个标准输出，在 handler 返回之后才写，
		// 读到它的时候这条请求的 SQL 日志都已经到了。不能拿服务端 Span 当信号：Span 是同步写文件的，
		// 标准输出经管道读进来要晚一点，压测把机器占满之后撞上过一次「Span 到了、SQL 日志还没到」
		accessLog(t, p, tid)
		serverSpan(t, p, tid)
		return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "SQL" && l.Str("trace_id") == tid })
	}
	check := func(t *testing.T, what, spanName, prefix string, r harness.Response, values ...string) {
		t.Helper()
		tid := traceIDOf(t, r)
		logs := sqlOf(t, tid)
		if len(logs) != 1 {
			t.Fatalf("%s 应记 1 条 SQL 日志，实际 %d 条", what, len(logs))
		}
		logged := logs[0].Str("sql")
		if !strings.HasPrefix(logged, prefix) || !strings.Contains(logged, "?") {
			t.Errorf("%s 的 SQL 日志应是带 ? 占位符的 %s…，实际 %q", what, prefix, logged)
		}
		mustNotContain(t, what+" 的 SQL 日志", logs[0].Line, values...)
		spans := spansNamed(traceSpans(t, p, tid), spanName)
		if len(spans) != 1 {
			t.Fatalf("%s 的链路上应有一个 %s", what, spanName)
		}
		if sent := spans[0].Str("db.query.text"); sent != logged {
			t.Errorf("%s：SQL 日志和 Span 的 db.query.text 应是同一条语句：日志 %q，Span %q", what, logged, sent)
		}
		t.Logf("数字：%s 记下的 SQL：%s", what, logged)
	}

	u, r := mysqlCreate(t, p, name, email)
	check(t, "POST /mysql/users", "gorm.create", "INSERT INTO `"+p.Table+"`", r, name, email)
	check(t, "GET /mysql/users/:id", "gorm.query", "SELECT * FROM `"+p.Table+"` WHERE id = ?",
		p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID)), fmt.Sprintf("id = %d", u.ID))
	check(t, "DELETE /mysql/users/:id", "gorm.delete", "DELETE FROM `"+p.Table+"` WHERE id = ?",
		p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil), fmt.Sprintf("id = %d", u.ID))

	// PG 那个实例没开 Log：它的 SQL 一条都不记
	pr := p.Get(t, "/users/999999999?cache=off")
	if logs := sqlOf(t, traceIDOf(t, pr)); len(logs) != 0 {
		t.Errorf("只给 mysql 实例开了 Log，default（PG）的 SQL 不该记，实际记了 %d 条：%s", len(logs), logs[0].Line)
	}
}

// Span 的属性（xgorm/trace.go，OTel 数据库语义约定 v1.43.0）：db.system.name 是数据库、db.namespace 是库名、
// server.address / server.port 是从 DSN 里解出来的地址、db.operation.name 是语句的第一个关键字，
// 结束时补 db.rows_affected；
// 父是服务端 Span。每个实例用自己的连接信息：同一个请求里 PG 和 MySQL 的 Span 各报各的
func TestMySQL_SQLSpanCarriesThisInstanceConnInfo(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true})
	name, email := "span-"+harness.NewID(), "span-"+harness.NewID()+"@example.com"

	u, r := mysqlCreate(t, p, name, email)
	upd := p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": name + "2"})
	del := p.Do(t, http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil)
	miss := p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID))

	for _, c := range []struct {
		what, span, op, rows string
		r                    harness.Response
	}{
		{"POST /mysql/users", "gorm.create", "INSERT", "1", r},
		{"PUT /mysql/users/:id", "gorm.update", "UPDATE", "1", upd},
		{"DELETE /mysql/users/:id", "gorm.delete", "DELETE", "1", del},
		{"GET 删掉之后", "gorm.query", "SELECT", "0", miss},
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
			host, port, _ := net.SplitHostPort(harness.MySQLAddr())
			for k, want := range map[string]string{
				"db.system.name": "mysql", "db.namespace": "xone_e2e", "server.address": host, "server.port": port,
				"db.operation.name": c.op, "db.rows_affected": c.rows,
			} {
				if got := s.Str(k); got != want {
					t.Errorf("%s 的 %s 应是 %q，实际 %q", c.span, k, want, got)
				}
			}
			// 「没查到记录」是正常的业务分支，不标成错误（trace.go endSpan）
			if s.StatusCode == "Error" {
				t.Errorf("%s 不该标成错误，实际 status=%s", c.span, s.StatusCode)
			}
			mustNotContain(t, "Span "+c.span+" 的属性", fmt.Sprint(s.Attributes), name, email)
		})
	}

	t.Run("同一进程里 PG 的 Span 报 PG 的连接信息", func(t *testing.T) {
		pr := p.Get(t, "/users/999999999?cache=off")
		tid := traceIDOf(t, pr)
		serverSpan(t, p, tid)
		q := spansNamed(traceSpans(t, p, tid), "gorm.query")
		host, _, _ := net.SplitHostPort(harness.PGAddr())
		if len(q) != 1 || q[0].Str("db.system.name") != "postgresql" || q[0].Str("server.address") != host {
			t.Errorf("default 实例的 gorm.query 应报 postgresql / %s，实际 %v", host, spanNames(q))
		}
	})
}

// xgorm/README.md XGorm：「Metric: true # 连接池指标，按实例生效：Metric: false 的实例不出现在 /metrics 里」。
// 指标按实例名打 name 标签，每个实例报自己的池子：给 mysql 配 MaxOpenConns: 7，
// e2e_db_pool_max_open{name="mysql"} 就是 7，default 仍是默认的 50
func TestMySQL_PoolMetricsLabeledByInstance_MetricTogglePerInstance(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pool := []string{
		"e2e_db_pool_open", "e2e_db_pool_in_use", "e2e_db_pool_idle", "e2e_db_pool_max_open",
		"e2e_db_pool_wait_total", "e2e_db_pool_wait_duration_seconds_total",
		"e2e_db_pool_closed_max_idle_total", "e2e_db_pool_closed_max_lifetime_total",
	}

	t.Run("两个实例各报各的", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: mysqlOverlay("MaxOpenConns: 7")})
		mysqlCreate(t, p, "metric", "metric@example.com")
		m := p.Metrics(t)
		for _, n := range pool {
			for _, inst := range []string{"default", "mysql"} {
				if len(m.Find(n, "name", inst)) != 1 {
					t.Errorf("/metrics 里应有 %s{name=%q}", n, inst)
				}
			}
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "mysql"); got != 7 {
			t.Errorf("mysql 实例配了 MaxOpenConns: 7，e2e_db_pool_max_open{name=\"mysql\"} 应是 7，实际 %v", got)
		}
		if got := m.Sum("e2e_db_pool_max_open", "name", "default"); got != 50 {
			t.Errorf("MaxOpenConns 只改了 mysql 实例，default 应仍是默认的 50，实际 %v", got)
		}
		if got := m.Sum("e2e_db_pool_open", "name", "mysql"); got < 1 {
			t.Errorf("跑过 MySQL 的 SQL 之后 e2e_db_pool_open{name=\"mysql\"} 应至少是 1，实际 %v", got)
		}
		t.Logf("数字：open{default}=%v open{mysql}=%v", m.Sum("e2e_db_pool_open", "name", "default"), m.Sum("e2e_db_pool_open", "name", "mysql"))
	})

	t.Run("mysql 配 Metric: false 就不出现", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: mysqlOverlay("Metric: false")})
		mysqlCreate(t, p, "metric", "metric@example.com")
		m := p.Metrics(t)
		for _, n := range pool {
			if len(m.Find(n, "name", "mysql")) != 0 {
				t.Errorf("mysql 实例配了 Metric: false，/metrics 里不该有 %s{name=\"mysql\"}", n)
			}
			if len(m.Find(n, "name", "default")) != 1 {
				t.Errorf("default 实例没关 Metric，/metrics 里应仍有 %s{name=\"default\"}", n)
			}
		}
	})
}

// MySQL 的密码不出现在任何输出里：docs/observability.md「框架自己的日志」、
// xgorm/dsn.go「这里的做法是根本不打印 DSN」。SQL 日志、debug、请求体日志、Span 全开，
// 走一圈增删改查，中途断一次 MySQL（这时的错误最可能把连接串带出来），再核对
// stdout / stderr、Span 文件、/metrics、响应体
func TestMySQL_PasswordNeverInLogsSpansMetricsOrResponses(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := harness.MySQLPassword()
	my := harness.NewProxy(t, harness.MySQLAddr())
	dsn := harness.MySQLDSN(my.Addr())
	p := harness.Start(t, harness.Options{
		MySQLAddr: my.Addr(), Spans: true,
		Overlay: sqlLog("mysql") + debugLogs + payloadLogs,
	})
	var bodies []string
	keep := func(r harness.Response) harness.Response { bodies = append(bodies, string(r.Body)); return r }

	u, r := mysqlCreate(t, p, "pw-check", "pw@example.com")
	keep(r)
	keep(p.Get(t, fmt.Sprintf("/mysql/users/%d", u.ID)))
	keep(p.Do(t, http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": "pw-check-2"}))
	keep(p.Get(t, "/mysql/users/999999999"))
	keep(p.Get(t, "/dep?target=mysql"))
	keep(p.Get(t, "/stuck?ms=1&mysql=1"))

	my.Cut()
	for _, path := range []string{fmt.Sprintf("/mysql/users/%d", u.ID), "/dep?target=mysql"} {
		if r := keep(p.Get(t, path)); r.Status < 500 {
			t.Errorf("MySQL 断开时 %s 应报错，实际 %v", path, r)
		}
	}
	if r := keep(p.PostJSON(t, "/mysql/users", map[string]string{"name": "while-cut"})); r.Status != http.StatusInternalServerError {
		t.Errorf("MySQL 断开时 POST /mysql/users 应 500，实际 %v", r)
	}
	keep(p.Get(t, "/stuck?ms=1&mysql=1"))
	my.Restore()
	mysqlWaitDep(t, p, 10*time.Second)

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
	for _, msg := range []string{"SQL", "read mysql user failed", "create mysql user failed", "xgorm connected"} {
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == msg })) == 0 {
			t.Errorf("这一圈应该产生过 %q 日志，一条都没有：核对范围不够", msg)
		}
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("这一圈应该在输出里留下过 connection refused，一条都没有：核对范围不够")
	}
	t.Logf("数字：核对了 %d 字节输出、%d 字节 Span、%d 个响应体", len(out), len(spans), len(bodies))
}

// xgorm/README.md XGorm：「DSN 里已经写了的 timeout 之类的参数不会被配置覆盖——配置里的值只是默认值」；
// 「MySQL 注入 DSN 的 timeout」「ReadTimeout 读超时，对应 DSN 的 readTimeout。默认 3s」。
//
// 读超时：对端不回话（SetDelay(time.Hour)），池里的连接上一条不给截止时间的查询该在 readTimeout 失败。
// 建连超时：主机宕机（Blackhole），先用短截止时间把池里卡住的连接耗掉，新建连接的 SYN 没有回音，
// 该在 timeout 失败。三种来源各起一个进程：
//
//	默认                 readTimeout=3s（MySQL.ReadTimeout）、timeout=500ms（DialTimeout）
//	配置（只改 mysql）    MySQL.ReadTimeout: 2s、DialTimeout: 800ms
//	DSN 里写了            readTimeout=1s&timeout=1500ms，配置里同时写着上面那组，以 DSN 为准
//
// 三组数两两之间至少差 300ms（faultSlack）：差得比余量小的话，DSN 里的被配置盖掉也照样落在余量里
func TestMySQL_ReadAndDialTimeoutFromDSN_ElseInstanceConfig(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	cfgOverlay := mysqlOverlay("DialTimeout: 800ms\nMySQL:\n  ReadTimeout: 2s")
	for _, c := range []struct {
		name          string
		dsnQuery      string
		overlay       string
		read, connect time.Duration
	}{
		{"默认", "", "", mysqlReadTimeout, mysqlDialTimeout},
		{"配置", "", cfgOverlay, 2 * time.Second, 800 * time.Millisecond},
		{"DSN里写了", "?readTimeout=1s&timeout=1500ms", cfgOverlay, time.Second, 1500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			t.Run("读超时", func(t *testing.T) {
				t.Parallel()
				my := harness.NewProxy(t, harness.MySQLAddr())
				p := harness.Start(t, harness.Options{Overlay: c.overlay,
					Env: map[string]string{"E2E_MYSQL_DSN": harness.MySQLDSN(my.Addr()) + c.dsnQuery}})
				my.SetDelay(time.Hour)
				r := mysqlDep(p, "", c.read+10*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("对端不回话、不给截止时间：查询应在 readTimeout（%v）失败返回 503，实际 %v", c.read, r)
				}
				if r.Server < c.read-20*time.Millisecond || r.Server > c.read+faultSlack {
					t.Errorf("readTimeout 应是 %v（%s），查询应在那时失败，实际 %v：%s", c.read, c.name, r.Server, r.Error)
				}
				t.Logf("数字：%s：对端不回话时一条不给截止时间的查询在 %s 失败（readTimeout=%v）：%s", c.name, faultMS(r.Server), c.read, r.Error)
			})
			t.Run("建连超时", func(t *testing.T) {
				t.Parallel()
				my := harness.NewProxy(t, harness.MySQLAddr())
				p := harness.Start(t, harness.Options{Overlay: c.overlay,
					Env: map[string]string{"E2E_MYSQL_DSN": harness.MySQLDSN(my.Addr()) + c.dsnQuery}})
				my.Blackhole()
				// 先用短截止时间把池里卡住的连接耗掉（它们卡在读上，建连超时管不到）：
				// 驱动在 ctx 结束时把那条连接关掉、不还回池里
				for i := 0; ; i++ {
					if i == 10 {
						t.Fatal("10 次短截止时间的查询都没走到新建连接")
					}
					if r := mysqlDep(p, "100ms", 10*time.Second); strings.Contains(r.Error, "dial tcp") {
						break
					}
				}
				r := mysqlDep(p, "10s", 20*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "dial tcp") {
					t.Fatalf("主机宕机：新建连接应失败（503，dial tcp …），实际 %v", r)
				}
				if !strings.Contains(r.Error, my.Addr()) {
					t.Errorf("错误里应有连不上的地址 %s，实际 %q", my.Addr(), r.Error)
				}
				if r.Server < c.connect-20*time.Millisecond || r.Server > c.connect+faultSlack {
					t.Errorf("timeout 应是 %v（%s），新建连接应在那时失败，实际 %v：%s", c.connect, c.name, r.Server, r.Error)
				}
				t.Logf("数字：%s：主机宕机时新建连接在 %s 失败（timeout=%v）：%s", c.name, faultMS(r.Server), c.connect, r.Error)
			})
		})
	}
}

// 调用方给了截止时间，MySQL 上的查询就在那一刻返回（xgorm.CWithCtx：「取实例并绑定 ctx，
// 链路和超时才能传到下游」）。池里的连接（卡在读上）和新建的连接（主机宕机时卡在 SYN 上）都要听
func TestMySQL_HungOrDown_QueryReturnsAtCallerDeadline(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const deadline = 200 * time.Millisecond
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			my := harness.NewProxy(t, harness.MySQLAddr())
			p := harness.Start(t, harness.Options{MySQLAddr: my.Addr()})
			m.inject(my)
			var took []time.Duration
			for i := range 6 {
				r := mysqlDep(p, deadline.String(), 10*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("第 %d 次：调用方给了 %v，应在那一刻以 503 返回，实际 %v", i+1, deadline, r)
				}
				if r.Server > deadline+faultSlack || r.Server < deadline-10*time.Millisecond {
					t.Errorf("第 %d 次：调用方给了 %v，查询应在那一刻返回，实际 %v：%s", i+1, deadline, r.Server, r.Error)
				}
				took = append(took, r.Server)
			}
			t.Logf("数字：MySQL %s、调用方给 %v：6 次依次用了 %v；%s", m.name, deadline, took, faultSummary(took))
		})
	}
}

// xgorm/README.md「通用」：服务端报错的原文里就是参数值
// （实测 MySQL 8.0.46 的 1366 是 Incorrect integer value: '<值>' for column 'id'，
// PG 16 的 22P02 是 invalid input syntax for type bigint: "<值>"）。
// SQL failed 日志的 error 字段和 Span 的状态、属性、事件里只有错误码；返回给业务的错误原样不变
func TestXGorm_ArgsInServerErrorKeptOutOfSQLLogAndSpan(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	p := harness.Start(t, harness.Options{Spans: true, Overlay: sqlLog("default", "mysql")})

	// 顺带：建连日志写着是哪个实例（两个实例连的库名一样，只看 addr / db 分不清）
	for _, name := range []string{"default", "mysql"} {
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm connected" && l.Str("name") == name })); n != 1 {
			t.Errorf("应有一条 name=%s 的 xgorm connected，实际 %d 条", name, n)
		}
	}

	for _, c := range []struct {
		db, span, code string
	}{
		{"mysql", "gorm.raw", "1366"},
		{"pg", "gorm.raw", "22P02"},
	} {
		t.Run(c.db, func(t *testing.T) {
			value := "leak-" + harness.NewID()
			r := p.Do(t, http.MethodPost, "/bad-sql", map[string]string{"db": c.db, "value": value})
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
			if logs[0].Str("error_code") != c.code || !strings.Contains(logs[0].Str("error"), c.code) {
				t.Errorf("SQL failed 日志应记下错误码 %s，实际 %s", c.code, logs[0].Line)
			}

			serverSpan(t, p, tid)
			spans := spansNamed(traceSpans(t, p, tid), c.span)
			if len(spans) != 1 {
				t.Fatalf("链路上应有一个 %s，实际 %s", c.span, spanNames(traceSpans(t, p, tid)))
			}
			s := spans[0]
			if s.StatusCode != "Error" || s.Str("db.response.status_code") != c.code || s.Str("error.type") != c.code {
				t.Errorf("Span 应标成 Error 并带上错误码 %s，实际 status=%s attrs=%v", c.code, s.StatusCode, s.Attributes)
			}
			mustNotContain(t, "Span "+c.span, fmt.Sprint(s.StatusDescription, s.Attributes, s.Events), value)
			t.Logf("数字：%s 的 SQL failed error=%q", c.db, logs[0].Str("error"))
		})
	}
}
