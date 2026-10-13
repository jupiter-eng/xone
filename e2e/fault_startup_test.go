package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 启动期建连验证的预算。
//
// docs/behavior.md「启动期建连探测」：连不上时按 3 次重试，两次之间的等待逐次翻倍并带抖动；
// xutil.Retry 的注释：「整轮有一个总预算（attempts × timeout + 各次退避的上界之和）」。
// 起始间隔 1s（docs/behavior.md「启动期建连探测」，即 xgorm.go / xredis.go 里的 pingInterval），
// 所以两次退避的上界是 1s、2s。单次尝试的超时：
//
//	XGorm（PostgreSQL）「注入的 connect_timeout（DialTimeout 向上取整到整秒）+ DialTimeout，默认 1.5s」
//	XRedis              xredis.go pingTimeout「建连加一个往返」= DialTimeout + ReadTimeout = 1s
const (
	faultPingAttempts     = 3
	faultPingBackoffs     = time.Second + 2*time.Second
	faultPGAttempt        = 1500 * time.Millisecond
	faultPGConnect        = time.Second                                                   // 注入的 connect_timeout
	faultPGStartBudget    = faultPingAttempts*faultPGAttempt + faultPingBackoffs          // 7.5s
	faultRedisStartBudget = faultPingAttempts*faultRedisAttemptBudget + faultPingBackoffs // 6s
)

// faultStartSlack 进程起停本身的开销：配置写错时整个进程 16–20ms 就退出了（本机空闲时实测）。
// 但 Uptime 从 exec 算起，并发跑整套 e2e 时光把进程拉起来就要等很久：GitHub runner（4 核、-parallel=16）上
// ClickHouse 密码错的那一例 1.075s 才退出，进程自己的日志从读配置到退出只占 140ms。
// 给到 2s，留一倍余量，又远小于任何一个被测的预算（最小 6s）。
// 「不重试」靠数代理收到的连接，不靠这个时长：重试第一次退避 1s，落在余量之内也照样被连接数抓到
const faultStartSlack = 2 * time.Second

// faultStartFails 起一个注定起不来的进程，等它退出：必须是非 0 退出、从没开始监听
func faultStartFails(t *testing.T, o harness.Options) (harness.Exit, *harness.Process) {
	t.Helper()
	o.NoWait = true
	p := harness.Start(t, o)
	exit, ok := p.Wait(30 * time.Second)
	if !ok {
		t.Fatalf("启动应该失败退出，30s 了还在跑\n%s", p.Output())
	}
	if exit.Code == 0 || exit.Signal != nil {
		t.Fatalf("启动应该失败（非 0 退出），实际 %v\n%s", exit, p.Output())
	}
	if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xgin listening" })) != 0 {
		t.Errorf("依赖起不来时服务不该开始监听，实际看到了 xgin listening")
	}
	return exit, p
}

// faultMustContain text 里每一个片段都得有
func faultMustContain(t *testing.T, where, text string, parts ...string) {
	t.Helper()
	for _, s := range parts {
		if !strings.Contains(text, s) {
			t.Errorf("%s 里应有 %q，实际：\n%s", where, s, text)
		}
	}
}

// 启动时 PG 不可达，三种不可达：
//
//	拒绝连接    进程挂了，端口没人听：每次尝试立刻失败，总耗时只剩两次退避
//	对端不回话  TCP 连得上、startup 没有回音：每次尝试等满 connect_timeout（1s）
//	主机宕机    SYN 没有回音：同上，等满 connect_timeout
//
// 都要在预算（faultPGStartBudget，7.5s）内失败；错误由 MustRun 打在 stderr 上，
// 要说清连不上的是哪个地址（docs/observability.md「框架自己的日志」），不能有密码和 DSN
func TestFault_PGUnreachableAtStartup_FailsInBudget_ErrHasAddrNoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	for _, c := range []struct {
		name     string
		inject   func(*harness.Proxy)
		lo       time.Duration // 至少这么久：每次尝试都等满 connect_timeout
		reason   string
		attempts bool // 代理数得到尝试次数（连得上代理的那两种里只有「对端不回话」）
	}{
		{"拒绝连接", func(p *harness.Proxy) { p.Cut() }, 0, "connection refused", false},
		{"对端不回话", func(p *harness.Proxy) { p.SetDelay(time.Hour) }, faultPingAttempts * faultPGConnect, "timeout", true},
		{"主机宕机", func(p *harness.Proxy) { p.Blackhole() }, faultPingAttempts * faultPGConnect, "timeout", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pg := harness.NewProxy(t, harness.PGAddr())
			c.inject(pg)
			exit, p := faultStartFails(t, harness.Options{PGAddr: pg.Addr()})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", "cannot reach "+pg.Addr(), c.reason)
			mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, harness.PGDSN(pg.Addr()))

			if exit.Uptime > faultPGStartBudget+faultStartSlack {
				t.Errorf("按文档，PG 建连验证最多 %d 次 × %v + 退避上界 %v = %v，实际 %v 才失败退出",
					faultPingAttempts, faultPGAttempt, faultPingBackoffs, faultPGStartBudget, exit.Uptime)
			}
			if exit.Uptime < c.lo {
				t.Errorf("%s时每次尝试都该等满 connect_timeout（%v），%d 次至少 %v，实际 %v 就退出了：没按文档试满 3 次，或没等满超时",
					c.name, faultPGConnect, faultPingAttempts, c.lo, exit.Uptime)
			}
			if c.attempts && pg.Accepted() != faultPingAttempts {
				t.Errorf("文档说建连探测最多试 %d 次，代理实际收到 %d 个连接", faultPingAttempts, pg.Accepted())
			}
			t.Logf("数字：启动时 PG %s：%v 后以 %d 退出（预算 %v）；代理收到 %d 个连接；错误：%s",
				c.name, exit.Uptime.Round(time.Millisecond), exit.Code, faultPGStartBudget, pg.Accepted(), lastLine(stderr))
		})
	}
}

// 启动时 PG 密码错误：docs/behavior.md「启动期建连探测」——认证失败不重试，错误说清是认证失败、不说连不上；
// 对的密码、错的密码都不能出现在输出里
func TestFault_PGBadPasswordAtStartup_ErrSaysAuthFailed_NoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	wrong := "wrong-pw-" + harness.NewID()
	pg := harness.NewProxy(t, harness.PGAddr()) // 只为数连接
	dsn := strings.Replace(harness.PGDSN(pg.Addr()), ":"+pw+"@", ":"+wrong+"@", 1)
	exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": dsn}})

	stderr := p.Stderr()
	faultMustContain(t, "stderr", stderr, "xgorm connect failed", "authentication to "+pg.Addr()+" failed", `password authentication failed for user "xone"`)
	mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, wrong, dsn)
	if strings.Contains(stderr, "cannot reach") {
		t.Errorf("认证失败不是连不上，不该报 cannot reach：\n%s", stderr)
	}
	// 只试一次就没有退避可等：进程起停的开销之内就该退出
	if exit.Uptime > faultStartSlack {
		t.Errorf("认证失败不重试，应在 %v 内退出，实际 %v", faultStartSlack, exit.Uptime)
	}
	if pg.Accepted() != 1 {
		t.Errorf("认证失败不重试，代理应只收到 1 个连接，实际 %d 个", pg.Accepted())
	}
	t.Logf("数字：启动时 PG 密码错误：%v 后以 %d 退出，试了 %d 次；错误：%s", exit.Uptime.Round(time.Millisecond), exit.Code, pg.Accepted(), lastLine(stderr))
}

// 启动时 Redis 不可达。配了密码（随机串，反正连不上，服务端不会校验）：错误里要有地址、没有它。
// xredis/README.md XRedis.Password：「本模块不会把它写进任何日志」。
// 预算 faultRedisStartBudget（6s）：3 次 × (DialTimeout + ReadTimeout) + 两次退避的上界
func TestFault_RedisUnreachableAtStartup_FailsInBudget_ErrHasAddrNoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	for _, c := range []struct {
		name   string
		inject func(*harness.Proxy)
		lo     time.Duration
		reason string
	}{
		// 拒绝连接的下限不卡：每次尝试当场失败，总耗时只剩两次带抖动的退避
		{"拒绝连接", func(p *harness.Proxy) { p.Cut() }, 0, "connection refused"},
		// 对端不回话：每次尝试至少等满 ReadTimeout（握手那一下）
		{"对端不回话", func(p *harness.Proxy) { p.SetDelay(time.Hour) }, faultPingAttempts * faultRedisReadTimeout, "i/o timeout"},
		// 主机宕机：每次尝试等满单次 Ping 的超时 DialTimeout + ReadTimeout
		{"主机宕机", func(p *harness.Proxy) { p.Blackhole() }, faultPingAttempts * faultRedisAttemptBudget, "deadline exceeded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			secret := "redis-secret-" + harness.NewID()
			rp := harness.NewProxy(t, harness.RedisAddr())
			c.inject(rp)
			exit, p := faultStartFails(t, harness.Options{RedisAddr: rp.Addr(), Overlay: fmt.Sprintf("XRedis:\n  Password: %q\n", secret)})
			stderr := p.Stderr()
			faultMustContain(t, "stderr", stderr, "xredis connect failed", "cannot reach "+rp.Addr(), c.reason)
			mustNotContain(t, "进程的 stdout / stderr", p.Output(), secret)
			if exit.Uptime > faultRedisStartBudget+faultStartSlack {
				t.Errorf("按文档，Redis 建连验证最多 %d 次 × %v + 退避上界 %v = %v，实际 %v 才失败退出",
					faultPingAttempts, faultRedisAttemptBudget, faultPingBackoffs, faultRedisStartBudget, exit.Uptime)
			}
			if exit.Uptime < c.lo {
				t.Errorf("%s时 %d 次尝试至少 %v，实际 %v 就退出了", c.name, faultPingAttempts, c.lo, exit.Uptime)
			}
			t.Logf("数字：启动时 Redis %s：%v 后以 %d 退出（预算 %v）；错误：%s",
				c.name, exit.Uptime.Round(time.Millisecond), exit.Code, faultRedisStartBudget, lastLine(stderr))
		})
	}
}

// 启动时 Redis 密码错误：xredis/README.md XRedis「认证失败（WRONGPASS / NOAUTH）报的是
// authentication to <地址> failed，不重试」，错误里是服务端回的原文，不含凭证。
//
// 「不重试」数连接：经代理连 Redis、不预热（MinIdleConns: 0），建连验证每试一次是一条新连接
// （认证失败的连接 go-redis 当场关掉），所以代理只该收到 1 条；重试的话是 3 条
func TestFault_RedisBadPasswordAtStartup_ErrSaysAuthFailed_NoRetry_NoPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	acl, pw := faultRedisACLUser(t)
	wrong := "wrong-" + harness.NewID()
	rp := harness.NewProxy(t, harness.RedisAddr())
	exit, p := faultStartFails(t, harness.Options{RedisAddr: rp.Addr(),
		Overlay: fmt.Sprintf("XRedis:\n  Username: %s\n  Password: %q\n  MinIdleConns: 0\n", acl, wrong)})
	stderr := p.Stderr()
	faultMustContain(t, "stderr", stderr, "xredis connect failed", "authentication to "+rp.Addr()+" failed", "WRONGPASS")
	if strings.Contains(stderr, "cannot reach") {
		t.Errorf("认证失败不该说成连不上，实际：%s", lastLine(stderr))
	}
	mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, wrong)
	if n := rp.Accepted(); n != 1 {
		t.Errorf("密码不对再试也不对：建连验证应只试 1 次（1 条连接），实际代理收到 %d 条", n)
	}
	if exit.Uptime > faultStartSlack {
		t.Errorf("不重试就没有退避：应在 %v 内失败退出，实际 %v", faultStartSlack, exit.Uptime)
	}
	t.Logf("数字：启动时 Redis 密码错误：%v 后以 %d 退出，代理收到 %d 条连接；错误：%s",
		exit.Uptime.Round(time.Millisecond), exit.Code, rp.Accepted(), lastLine(stderr))
}

// 回归：key=value 写法里 password 的等号两边带空格，同时另一个参数写错。
//
// xgorm/README.md XGorm：「pgx 自己的错误原文是整串 DSN、只遮得住 password=x 这种规整写法
// （实测 v5.10.0，password = hunter2 原样带出），所以不回传它」；预检用 pgx.ParseConfig，
// 「它比 pgconn.ParseConfig 多校验 default_query_exec_mode、statement_cache_capacity、
// description_cache_capacity，这三项写错也在预检时报一句不带 DSN 的错」。
// 上一轮修的正是 password = hunter2 配 default_query_exec_mode=bogus：密码原样进了错误。
//
// 预检失败是配置错，不该去建连、也不该重试：应当立刻退出（本机实测 16–20ms）。
// 另外两例 DSN 解得开、但建连失败（密码错、端口没人听），走的是 pgx 建连的错误，同样不能带出密码
func TestFault_DSNPasswordWithSpacedEqualsAndBadParam_FailsWithoutPassword(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	secret := "hunter2-" + harness.NewID()
	base := "host=127.0.0.1 port=" + strings.Split(harness.PGAddr(), ":")[1] + " user=xone dbname=xone_e2e"
	parse := []struct{ name, dsn string }{
		{"default_query_exec_mode 写错（上一轮的 bug）", base + " password = " + secret + " default_query_exec_mode=bogus"},
		{"statement_cache_capacity 不是数字", base + " password = " + secret + " statement_cache_capacity=abc"},
		{"description_cache_capacity 不是数字", base + " password = " + secret + " description_cache_capacity=abc"},
		{"sslmode 写错", base + " password = " + secret + " sslmode=bogus"},
		{"connect_timeout 不是数字", base + " password = " + secret + " connect_timeout=abc"},
		{"port 不是数字", "host=127.0.0.1 port=notaport user=xone dbname=xone_e2e password = " + secret},
		{"只有等号后面有空格", base + " password= " + secret + " default_query_exec_mode=bogus"},
		{"只有等号前面有空格", base + " password =" + secret + " default_query_exec_mode=bogus"},
		{"带引号、密码里有空格", base + " password = '" + secret + " x' default_query_exec_mode=bogus"},
		{"带引号、密码里有转义的单引号", base + ` password = 'it\'s-` + secret + `' default_query_exec_mode=bogus`},
		{"URL 写法里 pgx 专有参数写错", "postgres://xone:" + secret + "@" + harness.PGAddr() + "/xone_e2e?sslmode=disable&default_query_exec_mode=bogus"},
	}
	for _, c := range parse {
		t.Run("预检失败："+c.name, func(t *testing.T) {
			t.Parallel()
			exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": c.dsn}})
			stderr := p.Stderr()
			mustNotContain(t, "进程的 stdout / stderr", p.Output(), secret)
			faultMustContain(t, "stderr", stderr, "xgorm config failed", "failed to parse DSN")
			if exit.Uptime > faultStartSlack {
				t.Errorf("DSN 预检失败是配置错，不该建连、不该重试，应立刻退出，实际用了 %v", exit.Uptime)
			}
			t.Logf("数字：%s：%v 后以 %d 退出；错误：%s", c.name, exit.Uptime.Round(time.Millisecond), exit.Code, lastLine(stderr))
		})
	}

	t.Run("解得开但密码错", func(t *testing.T) {
		t.Parallel()
		exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": base + " sslmode=disable password = " + secret}})
		mustNotContain(t, "进程的 stdout / stderr", p.Output(), secret)
		faultMustContain(t, "stderr", p.Stderr(), "xgorm connect failed", "password authentication failed")
		t.Logf("数字：DSN 解得开、密码错：%v 后以 %d 退出；错误：%s", exit.Uptime.Round(time.Millisecond), exit.Code, lastLine(p.Stderr()))
	})

	t.Run("解得开但端口没人听", func(t *testing.T) {
		t.Parallel()
		addr := fmt.Sprintf("127.0.0.1:%d", harness.FreePort(t))
		dsn := "host=127.0.0.1 port=" + strings.Split(addr, ":")[1] + " user=xone dbname=xone_e2e sslmode=disable password = " + secret
		exit, p := faultStartFails(t, harness.Options{Env: map[string]string{"E2E_PG_DSN": dsn}})
		mustNotContain(t, "进程的 stdout / stderr", p.Output(), secret)
		faultMustContain(t, "stderr", p.Stderr(), "xgorm connect failed", "cannot reach "+addr, "connection refused")
		t.Logf("数字：DSN 解得开、端口没人听：%v 后以 %d 退出；错误：%s", exit.Uptime.Round(time.Millisecond), exit.Code, lastLine(p.Stderr()))
	})
}
