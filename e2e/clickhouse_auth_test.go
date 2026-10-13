package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 启动时 ClickHouse 拒绝了这组凭证，native 协议：xgorm/clickhouse/README.md「配置」——
// 「密码错时不重试，报 authentication to <addr> failed：认的是 native 协议握手时服务端回的
// *clickhouse.Exception，错误码 516（AUTHENTICATION_FAILED，新版本服务端密码错、用户不存在都报它）」。
// 对的密码、错的密码都不能出现在输出里。服务端回的错误（实测 24.8.14）：
//
//	密码错 / 用户不存在  code: 516, message: <user>: Authentication failed: password is incorrect, or there is no user with such name.
//
// 库不存在不是认证失败（code: 81 UNKNOWN_DATABASE），照常重试、报 cannot reach。
// 报错里多数是 code: 81，偶尔是 driver: bad connection（xgorm/clickhouse/README.md「认证失败」：
// 驱动 ping 之前的连接检查把服务端的异常读走了），两种都认；并发跑整套 e2e 时后者会出现
func TestClickHouse_BadPasswordOrUnknownUserAtStartup_NativeAuthFail_NoRetry(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pw := harness.CHPassword()
	for _, c := range []struct {
		name string
		dsn  func(addr, wrong string) string
		user string
	}{
		{"密码错误", func(a, w string) string { return harness.CHDSNWith("clickhouse", a, w) }, "xone"},
		{"用户不存在", func(a, w string) string {
			return strings.Replace(harness.CHDSNWith("clickhouse", a, w), "xone:", "nobody_"+harness.NewID()[:6]+":", 1)
		}, "nobody_"},
		{"tcp协议密码错误", func(a, w string) string { return harness.CHDSNWith("tcp", a, w) }, "xone"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wrong := "wrong-pw-" + harness.NewID()
			chp := harness.NewProxy(t, harness.CHAddr()) // 只为数连接
			dsn := c.dsn(chp.Addr(), wrong)
			exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": dsn}})

			stderr := p.Stderr()
			mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, wrong, dsn)
			faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "ch"`,
				"authentication to "+chp.Addr()+" failed", "code: 516", c.user)
			if strings.Contains(stderr, "cannot reach") {
				t.Errorf("认证失败不是连不上，不该报 cannot reach：%s", lastLine(stderr))
			}
			if exit.Uptime > faultStartSlack {
				t.Errorf("认证失败不重试，应在 %v 内退出，实际 %v", faultStartSlack, exit.Uptime)
			}
			if chp.Accepted() != 1 {
				t.Errorf("认证失败不重试，代理应只收到 1 个连接，实际 %d 个", chp.Accepted())
			}
			t.Logf("数字：启动时 ClickHouse %s：%v 后以 %d 退出，试了 %d 次；错误：%s", c.name, exit.Uptime.Round(time.Millisecond), exit.Code, chp.Accepted(), lastLine(stderr))
		})
	}

	t.Run("库不存在不算认证失败", func(t *testing.T) {
		t.Parallel()
		chp := harness.NewProxy(t, harness.CHAddr())
		dsn := strings.Replace(harness.CHDSN(chp.Addr()), "/xone_e2e", "/nodb_"+harness.NewID(), 1)
		exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": dsn}})
		stderr := p.Stderr()
		mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, dsn)
		faultMustContain(t, "stderr", stderr, "cannot reach "+chp.Addr())
		if !strings.Contains(stderr, "code: 81") && !strings.Contains(stderr, "driver: bad connection") {
			t.Errorf("库不存在应报服务端的 code: 81（偶尔是 driver: bad connection），实际：%s", lastLine(stderr))
		}
		if strings.Contains(stderr, "authentication to") {
			t.Errorf("库不存在不是认证失败：%s", lastLine(stderr))
		}
		if chp.Accepted() != faultPingAttempts {
			t.Errorf("认不出来的错误照常重试，代理应收到 %d 个连接，实际 %d 个", faultPingAttempts, chp.Accepted())
		}
		if exit.Uptime > chStartBudget+faultStartSlack {
			t.Errorf("应在建连预算 %v 内失败，实际 %v", chStartBudget, exit.Uptime)
		}
		t.Logf("数字：库不存在：%v 后退出，试了 %d 次；错误：%s", exit.Uptime.Round(time.Millisecond), chp.Accepted(), lastLine(stderr))
	})
}

// HTTP 协议（http://…:8123）下密码错：xgorm/clickhouse/README.md「配置」——「HTTP 协议下同样认得出」。
// clickhouse-go v2.48.0 把非 200 的响应解析成 *clickhouse.HTTPError，里面包着 *clickhouse.Exception
// （conn_http_errors.go，错误码取自 X-ClickHouse-Exception-Code 头），errors.As 取得到。实测（ClickHouse 24.8.14）：
//
//	failed to query server hello: …: sendQuery: [HTTP 403] code: 516, message: xone: Authentication failed: … (AUTHENTICATION_FAILED) …
//
// 状态码是 403（不是 401）。v2.30.0 时这里是一段拼好的文本、错误链上没有 Exception，认不出，试满 3 次、报 cannot reach。
// 密码照样不出现在输出里（错误文本里没有它）
func TestClickHouse_BadPasswordOverHTTPAtStartup_AuthFail_NoRetry_NoPassword(t *testing.T) {
	harness.RequireCH(t)
	t.Parallel()
	pw := harness.CHPassword()
	wrong := "wrong-pw-" + harness.NewID()
	chp := harness.NewProxy(t, harness.CHHTTPAddr())
	dsn := harness.CHDSNWith("http", chp.Addr(), wrong)
	exit, p := faultStartFails(t, harness.Options{ClickHouse: true, Env: map[string]string{"E2E_CH_DSN": dsn}})
	stderr := p.Stderr()
	mustNotContain(t, "进程的 stdout / stderr", p.Output(), pw, wrong, dsn)
	faultMustContain(t, "stderr", stderr, "xgorm connect failed", `instance "ch"`,
		"authentication to "+chp.Addr()+" failed", "[HTTP 403] code: 516", "AUTHENTICATION_FAILED")
	if strings.Contains(stderr, "cannot reach") {
		t.Errorf("认证失败不是连不上，不该报 cannot reach：%s", lastLine(stderr))
	}
	if exit.Uptime > faultStartSlack {
		t.Errorf("认证失败不重试，应在 %v 内退出，实际 %v", faultStartSlack, exit.Uptime)
	}
	if chp.Accepted() != 1 {
		t.Errorf("认证失败不重试，代理应只收到 1 个连接，实际 %d 个", chp.Accepted())
	}
	t.Logf("数字：HTTP 协议密码错：%v 后以 %d 退出，试了 %d 次；错误：%s", exit.Uptime.Round(time.Millisecond), exit.Code, chp.Accepted(), grepLines(stderr, "xone start failed", 1))
}
