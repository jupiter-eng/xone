package e2e

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// mysqlDriverLogLine go-sql-driver 自带的 logger（标准库 log，前缀 [mysql]）写出来的一行
var mysqlDriverLogLine = regexp.MustCompile(`(?m)^\[mysql\] \d{4}/\d{2}/\d{2} .*$`)

// 运行中 MySQL 进程挂了（端口拒绝连接）：
//
//   - 用到 MySQL 的操作当场报错（新连接被拒不需要等任何超时），错误里有地址
//   - 另一个实例（PG）和不碰 MySQL 的接口不受影响：多实例各是各的连接池
//   - MySQL 回来之后不用重启就恢复
func TestMySQL_RefusedAtRuntime_FailsFast_PGUnaffected_Recovers(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	my := harness.NewProxy(t, harness.MySQLAddr())
	p := harness.Start(t, harness.Options{MySQLAddr: my.Addr()})
	u, _ := mysqlCreate(t, p, "before-cut", "cut@example.com")

	my.Cut()

	t.Run("用到MySQL的操作当场报错", func(t *testing.T) {
		var took []time.Duration
		for i := range 5 {
			r := mysqlDep(p, "", 30*time.Second)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("第 %d 次：MySQL 拒绝连接时 /dep?target=mysql 应回 503，实际 %v", i+1, r)
			}
			if !strings.Contains(r.Error, "connection refused") || !strings.Contains(r.Error, my.Addr()) {
				t.Errorf("第 %d 次：错误应说清连不上哪（%s，connection refused），实际 %q", i+1, my.Addr(), r.Error)
			}
			// 连接被拒是立即的，上限给 DialTimeout：真等了哪怕一次超时也会超过它
			if r.Server > mysqlDialTimeout {
				t.Errorf("第 %d 次：新连接被拒不该等任何超时，实际 %v", i+1, r.Server)
			}
			took = append(took, r.Server)
		}
		t.Logf("数字：MySQL 拒绝连接时 SELECT 1 依次用了 %v；%s", took, faultSummary(took))

		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, fmt.Sprintf("/mysql/users/%d", u.ID), nil},
			{http.MethodPost, "/mysql/users", map[string]string{"name": "while-cut"}},
			{http.MethodPut, fmt.Sprintf("/mysql/users/%d", u.ID), map[string]string{"name": "while-cut"}},
			{http.MethodDelete, fmt.Sprintf("/mysql/users/%d", u.ID), nil},
		} {
			r, took := faultTimed(t, p, c.method, c.path, c.body)
			if r.Status != http.StatusInternalServerError {
				t.Errorf("MySQL 不可用时 %s %s 应 500，实际 %v", c.method, c.path, r)
			}
			if took > mysqlDialTimeout {
				t.Errorf("MySQL 拒绝连接时 %s %s 应当场报错，实际用了 %v", c.method, c.path, took)
			}
		}
	})

	t.Run("PG和不碰MySQL的接口不受影响", func(t *testing.T) {
		faultQuick(t, p, "GET /ping", "/ping", http.StatusOK)
		faultQuick(t, p, "GET /dep?target=db", "/dep?target=db", http.StatusOK)
		pu := createUser(t, p, "pg-while-mysql-cut", "pg@example.com")
		faultQuick(t, p, "GET /users/:id?cache=off（PG）", fmt.Sprintf("/users/%d?cache=off", pu.ID), http.StatusOK)
	})

	t.Run("MySQL回来之后自动恢复", func(t *testing.T) {
		my.Restore()
		took, n := mysqlWaitDep(t, p, 10*time.Second)
		t.Logf("数字：MySQL 恢复监听之后 %s（第 %d 次探测）SELECT 1 回到 200", faultMS(took), n)
		fresh, _ := mysqlCreate(t, p, "after", "after@example.com")
		if r := p.Get(t, fmt.Sprintf("/mysql/users/%d", fresh.ID)); r.Status != http.StatusOK {
			t.Errorf("恢复后读写应照常，实际 %v", r)
		}
	})
}

// 运行中 MySQL 不回话（SetDelay(time.Hour)）、调用方不给截止时间：
// xgorm/README.md XGorm.MySQL.ReadTimeout「读超时，对应 DSN 的 readTimeout。默认 3s」——
// 池里的每一条连接都卡在读上，一条查询在 ReadTimeout 失败，不会一直挂住；并发地打也一样。
// 同一时刻 PG 那个实例照常、而且快：两个实例的连接池互不牵连。
//
// 另有一条：go-sql-driver 读超时时会用它自带的 logger 往 stderr 写一行纯文本
// （[mysql] 2026/09/24 packets.go:58 read tcp …: i/o timeout）。docs/architecture.md
// 「我们故意跟底层库不一样的地方」里，go-redis、resty、GORM 自带的 logger 都接到了 slog
// ——「绕开 slog 的日志进不了日志平台」；go-sql-driver 的没接，按同一条原则是 bug
func TestMySQL_SilentAtRuntime_QueryFailsAtReadTimeout_PGUnaffected(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const conc = 8
	my := harness.NewProxy(t, harness.MySQLAddr())
	p := harness.Start(t, harness.Options{MySQLAddr: my.Addr()})
	// 先把池子撑到 conc 条连接：卡住之后并发的每一条查询各占一条池里的连接
	for _, r := range faultBurst(p, conc, "mysql", 10*time.Second) {
		if r.Status != http.StatusOK {
			t.Fatalf("故障前 SELECT 1 应 200，实际 %v", r)
		}
	}
	my.SetDelay(time.Hour)

	var pg []faultDepResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // MySQL 那一批卡着的同时，PG 每 100ms 探一次
		defer wg.Done()
		for range 20 {
			pg = append(pg, faultDep(p, "db", "", 5*time.Second))
			time.Sleep(100 * time.Millisecond)
		}
	}()
	res := faultBurst(p, conc, "mysql", mysqlReadTimeout+10*time.Second)
	wg.Wait()

	var took []time.Duration
	for i, r := range res {
		if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
			t.Errorf("第 %d 个：MySQL 不回话时查询应在 ReadTimeout 失败返回 503，实际 %v", i+1, r)
			continue
		}
		if r.Server > mysqlReadTimeout+faultSlack || r.Server < mysqlReadTimeout-20*time.Millisecond {
			t.Errorf("第 %d 个：文档说 MySQL.ReadTimeout 默认 3s，查询应在那时失败，实际 %v：%s", i+1, r.Server, r.Error)
		}
		took = append(took, r.Server)
	}
	t.Logf("数字：MySQL 不回话、并发 %d、不给截止时间：%s；错误：%s", conc, faultSummary(took), res[0].Error)

	var pgTook []time.Duration
	for i, r := range pg {
		if r.Status != http.StatusOK || r.Server > faultUnaffected {
			t.Errorf("第 %d 次：MySQL 卡住时 PG 那个实例应照常而且快，实际 %v", i+1, r)
		}
		pgTook = append(pgTook, r.Server)
	}
	t.Logf("数字：同一时刻 PG 的 SELECT 1：%s", faultSummary(pgTook))

	my.SetDelay(0)
	took2, n := mysqlWaitDep(t, p, 10*time.Second)
	t.Logf("数字：MySQL 恢复回话之后 %s（第 %d 次探测）回到 200", faultMS(took2), n)

	t.Run("go-sql-driver自己的日志也走slog", func(t *testing.T) {
		// xgorm/README.md XGorm：go-sql-driver 自己的日志接到了 slog，一条 xgorm go-sql-driver log，级别 WARN，原文在 detail。
		// 这条原先是 KNOWN BUG：xgorm 没调 mysql.SetLogger，驱动用它自己的 log.New(os.Stderr, "[mysql] ", …)，
		// 本用例里 8 条读超时就是 stderr 里 8 行 [mysql] … packets.go:58 read tcp …: i/o timeout
		if lines := mysqlDriverLogLine.FindAllString(p.Stderr(), -1); len(lines) > 0 {
			t.Errorf("go-sql-driver 的日志应走 slog，实际 stderr 里有 %d 行纯文本，例：%s", len(lines), lines[0])
		}
		ls := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm go-sql-driver log" })
		if len(ls) == 0 {
			t.Fatalf("读超时时驱动会记日志，应当是 xgorm go-sql-driver log，实际一条都没有\n%s", lastLines(p.Stderr(), 10))
		}
		for _, l := range ls {
			if l.Str("level") != "WARN" || !strings.Contains(l.Str("detail"), "packets.go") {
				t.Errorf("驱动的日志应是 WARN、原文在 detail 里，实际 %s", l.Line)
				break
			}
		}
		t.Logf("数字：%d 条 xgorm go-sql-driver log，例：%s", len(ls), ls[0].Str("detail"))
	})
}

// 启动时 MySQL 不可达，三种不可达：拒绝连接 / 对端不回话 / 主机宕机。
//
// docs/config.md、xgorm/README.md：「建连重试：连不上时按 3 次重试」「单次建连探测的预算：MySQL 是 DialTimeout + MySQL.ReadTimeout」，
// 所以启动最多等 3 × 3.5s + 3s = 13.5s（mysqlStartBudget）；错误要说清连不上的是哪个实例、哪个地址，
// 不能有密码和 DSN。
//
// PG 那个实例是好的，而且名字排在前面、先建（xclient.Build 按名字排序）：
// MySQL 起不来时它要跟着关掉，进程照样失败退出
func TestMySQL_UnreachableAtStartup_FailsInBudget_ErrHasNameAddrNoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := harness.MySQLPassword()
	for _, c := range []struct {
		name   string
		inject func(*harness.Proxy)
		reason string
		count  bool // 代理数得到尝试次数（连得上代理的只有「对端不回话」）
	}{
		{"拒绝连接", func(p *harness.Proxy) { p.Cut() }, "connection refused", false},
		{"对端不回话", func(p *harness.Proxy) { p.SetDelay(time.Hour) }, "invalid connection", true},
		{"主机宕机", func(p *harness.Proxy) { p.Blackhole() }, "i/o timeout", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			my := harness.NewProxy(t, harness.MySQLAddr())
			c.inject(my)
			exit, p := faultStartFails(t, harness.Options{MySQLAddr: my.Addr()})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "mysql"`, my.Addr(), c.reason)
			mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, harness.MySQLDSN(my.Addr()))
			if exit.Uptime > mysqlStartBudget+faultStartSlack {
				t.Errorf("按文档，MySQL 建连验证最多 %d 次 × %v + 退避上界 %v = %v，实际 %v 才失败退出",
					faultPingAttempts, mysqlAttempt, faultPingBackoffs, mysqlStartBudget, exit.Uptime)
			}
			// PG 先建好了，MySQL 失败时要关掉它：xgorm connected 只有 default 那一条，没有 xgorm ready
			if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgorm ready" })); n != 0 {
				t.Errorf("mysql 实例建不起来时 xgorm 不该报 ready，实际 %d 条", n)
			}
			t.Logf("数字：启动时 MySQL %s：%v 后以 %d 退出（预算 %v）；代理收到 %d 个连接；错误：%s",
				c.name, exit.Uptime.Round(time.Millisecond), exit.Code, mysqlStartBudget, my.Accepted(), lastLine(stderr))

			// 这条原先是 KNOWN BUG：代理只收到 1 个连接，进程在 3.0s（= ReadTimeout，握手那一读）后退出，
			// 错误是 open <addr> failed: invalid connection。GORM 的 MySQL Dialector 在 gorm.Open 里的
			// Initialize 先查一次 SELECT VERSION()（gorm.io/driver/mysql v1.6.0 mysql.go:130），
			// 失败了 gorm.Open 直接返回错误，xgorm.New 走不到后面那次带重试的 ping
			if c.count && my.Accepted() != faultPingAttempts {
				t.Errorf("文档说连不上时按 %d 次重试；MySQL 实际只试了 %d 次（错误：%s）", faultPingAttempts, my.Accepted(), lastLine(stderr))
			}
		})
	}
}

// 启动时 MySQL 拒绝了这组凭证：docs/behavior.md「启动期建连探测」——认证失败不重试，错误说清是认证失败、不说连不上。
// 对的密码、错的密码都不能出现在输出里。服务端回的错误（实测 MySQL 8.0.46）：
//
//	密码错         Error 1045 (28000): Access denied for user 'xone'@…（用户不存在也是 1045）
//	没有库的权限   Error 1044 (42000): Access denied for user 'xone'@… to database '…'
//	               （库不存在时，没有全局权限的账号拿到的也是 1044）
//
// 这条原先是 KNOWN BUG：xgorm 只认 PG 的 SQLSTATE 28 类，而 MySQL 的认证错误出在 gorm.Open 里
// （Dialector.Initialize 查版本是第一次建连），报成了 open <地址> failed: Error 1045 …
func TestMySQL_BadPasswordOrNoDBAccessAtStartup_AuthFail_NoRetry_NoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := harness.MySQLPassword()
	for _, c := range []struct {
		name string
		dsn  func(addr, wrong string) string
		code string
	}{
		{"密码错误", mysqlSecretDSN, "Error 1045"},
		{"没有库的权限", func(a, _ string) string { return mysqlSecretDSN(a, pw) + "_noaccess_" + harness.NewID() }, "Error 1044"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wrong := "wrong-pw-" + harness.NewID()
			my := harness.NewProxy(t, harness.MySQLAddr()) // 只为数连接
			dsn := c.dsn(my.Addr(), wrong)
			exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_MYSQL_DSN": dsn}})

			stderr := p.Stderr()
			mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, wrong, dsn)
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "mysql"`,
				"authentication to "+my.Addr()+" failed", c.code, "Access denied for user 'xone'")
			for _, s := range []string{"cannot reach", "open " + my.Addr() + " failed"} {
				if strings.Contains(stderr, s) {
					t.Errorf("认证失败不是连不上，不该报 %q：%s", s, lastLine(stderr))
				}
			}
			if exit.Uptime > faultStartSlack {
				t.Errorf("认证失败不重试，应在 %v 内退出，实际 %v", faultStartSlack, exit.Uptime)
			}
			if my.Accepted() != 1 {
				t.Errorf("认证失败不重试，代理应只收到 1 个连接，实际 %d 个", my.Accepted())
			}
			t.Logf("数字：启动时 MySQL %s：%v 后以 %d 退出，试了 %d 次；错误：%s", c.name, exit.Uptime.Round(time.Millisecond), exit.Code, my.Accepted(), lastLine(stderr))
		})
	}
}
