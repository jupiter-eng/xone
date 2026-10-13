package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 两种「对端不回话」：
//
//	silent  进程还在、TCP 连得上，但一个字节都不回（SetDelay(time.Hour)）：卡死的进程、打满的服务端
//	down    主机宕机：现有连接不回话，新连接的 SYN 没有回音（Blackhole）
//
// 进程挂了（connection refused）的那一种见 fault_test.go，那里什么超时都不用等
var faultSilentModes = []struct {
	name   string
	inject func(*harness.Proxy)
}{
	{"对端不回话", func(p *harness.Proxy) { p.SetDelay(time.Hour) }},
	{"主机宕机", func(p *harness.Proxy) { p.Blackhole() }},
}

// docs/behavior.md「总表」：
//
//	XRedis 命令超时 | 库自己：只认 ReadTimeout | 这里：听调用方的 ctx
//	不然 200ms 预算的请求会在慢 Redis 上等满 ReadTimeout（实测 5s）
//
// 所以把 ReadTimeout 配成 5s 重现那个场景：调用方给 200ms，就该在 200ms 返回。
// 连接池里现成的连接（卡在读上）和新建的连接（卡在握手或 SYN 上）都要听它，
// 所以每种故障连发 12 次。MinIdleConns 配 0，池里只有启动时 Ping 用过的那一条：
// 第一次用它（卡在读上），之后每次都新建（卡在握手或 SYN 上）。
// 两段的错误都是 context deadline exceeded，分不出来；靠代理分：命令开始时代理上
// 一条连接都没有（主机宕机），或者命令期间代理接受了新连接（对端不回话），就是新建的那一段

func TestFault_RedisSlowOrDown_CommandReturnsAtCallerDeadline_NoReadTimeoutWait(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const deadline = 200 * time.Millisecond
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			rp := harness.NewProxy(t, harness.RedisAddr())
			p := harness.Start(t, harness.Options{RedisAddr: rp.Addr(), Overlay: "XRedis:\n  ReadTimeout: 5s\n  MinIdleConns: 0\n"})
			m.inject(rp)

			var took []time.Duration
			phases := map[string]int{}
			for i := range 12 {
				empty, accepted := rp.Active() == 0, rp.Accepted()
				r := faultDep(p, "redis", deadline.String(), 10*time.Second)
				phase := "池里的连接"
				if empty || rp.Accepted() > accepted {
					phase = "新建连接"
				}
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("第 %d 次：调用方给了 %v，应在那一刻以 503 返回，实际 %v", i+1, deadline, r)
				}
				if r.Server > deadline+faultSlack {
					t.Errorf("第 %d 次：docs/architecture.md 说 Redis 命令听调用方的 ctx，给了 %v 就该在 %v 返回，实际 %v（ReadTimeout 是 5s）：%s",
						i+1, deadline, deadline, r.Server, r.Error)
				}
				phases[phase]++
				took = append(took, r.Server)
			}
			t.Logf("数字：%s、ReadTimeout=5s、调用方给 %v：12 次依次用了 %v；%s；%v", m.name, deadline, took, faultSummary(took), phases)
			if phases["新建连接"] == 0 || phases["池里的连接"] == 0 {
				t.Errorf("池里的连接和新建连接两段都该测到，实际 %v", phases)
			}
		})
	}
}

// xredis/README.md XRedis.ReadTimeout：调用方没给截止时间时，管住一条命令的是它。
// 配一个和默认值（500ms）明显不同的 300ms，看到的应该是 300ms——听的是配置，不是别的什么数。
//
// 实测：故障之后的第一条命令约 2×ReadTimeout（本机 0.63–0.66s；默认 500ms 时 1.03–1.05s），
// 之后每条约 1×ReadTimeout（0.31–0.34s）。上限按文档的模型给：一次尝试不超过
// DialTimeout + ReadTimeout（xredis 的 pingTimeout 同样这么算），中位数应在 ReadTimeout 附近
func TestFault_RedisSilentNoCallerDeadline_CommandFailsAtReadTimeout(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const readTimeout = 300 * time.Millisecond
	rp := harness.NewProxy(t, harness.RedisAddr())
	p := harness.Start(t, harness.Options{RedisAddr: rp.Addr(), Overlay: "XRedis:\n  ReadTimeout: 300ms\n"})
	rp.SetDelay(time.Hour)

	var took []time.Duration
	for i := range 6 {
		r := faultDep(p, "redis", "", 2*faultRedisCmdBudget)
		if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "i/o timeout") {
			t.Fatalf("第 %d 次：Redis 不回话时应读超时（503，i/o timeout），实际 %v", i+1, r)
		}
		if lim := faultRedisDialTimeout + readTimeout; r.Server > lim+faultSlack {
			t.Errorf("第 %d 次：一次尝试最多 DialTimeout+ReadTimeout=%v，实际 %v", i+1, lim, r.Server)
		}
		took = append(took, r.Server)
	}
	med := faultMedian(took)
	if med < readTimeout-20*time.Millisecond || med > readTimeout+faultSlack/3 {
		t.Errorf("文档说 ReadTimeout 管住读：配了 %v，一条命令的中位数应在它附近，实际 %v（%v）", readTimeout, med, took)
	}
	t.Logf("数字：Redis 不回话、ReadTimeout=%v、不给截止时间：6 条命令依次用了 %v；%s", readTimeout, took, faultSummary(took))
}

// Redis 主机宕机、调用方没给截止时间时，一条命令要多久。
//
// xredis/README.md XRedis：一条命令最多 (MaxRetries+1)×(DialTimeout+ReadTimeout)+MaxRetries×MaxRetryBackoff，
// 默认 7s（faultRedisCmdBudget）；MaxRetries: -1 时只剩一次尝试，不超过 DialTimeout+ReadTimeout（1s）。
//
// 这条式子成立靠的是 xredis 把 DialerRetries 配成 1：go-redis v9.22.0 默认每次建连内部还要
// 重拨 5 次、每两次之间等 DialerRetryTimeout=100ms，一次「建连」就是 5×500ms+4×100ms = 2.9s，
// 默认的 4 次尝试实测 11.7s。
// 连接池累计 PoolSize（10×GOMAXPROCS）次建连失败后才改成直接报上一次的错。
//
// 进入「建连」这一段之前，池里现成的连接（MinIdleConns 预热的）会先各自读超时一次，
// 那些命令要 0.5–1s；所以循环到错误里出现 dial tcp 为止，量的是第一条走到建连的命令
func TestFault_RedisHostDownNoCallerDeadline_CommandWithinDocumentedBudget(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	cases := []struct {
		name    string
		overlay string
		budget  time.Duration
		why     string
	}{
		{"关闭重试", "XRedis:\n  MaxRetries: -1\n  MinIdleConns: 0\n", faultRedisAttemptBudget,
			"MaxRetries: -1 只剩一次尝试，建连不超过 DialTimeout（500ms）、再加一个往返"},
		{"默认配置", "", faultRedisCmdBudget,
			"(MaxRetries+1)×(DialTimeout+ReadTimeout)+MaxRetries×MaxRetryBackoff"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rp := harness.NewProxy(t, harness.RedisAddr())
			p := harness.Start(t, harness.Options{RedisAddr: rp.Addr(), Overlay: c.overlay})
			rp.Blackhole()

			var pooled []time.Duration
			var first faultDepResult
			for i := 0; ; i++ {
				if i == 12 {
					t.Fatalf("12 条命令都没走到建连，池里的连接比预想的多：%v", pooled)
				}
				r := faultDep(p, "redis", "", 5*faultRedisCmdBudget)
				if r.ReqErr != nil {
					t.Fatalf("第 %d 条命令 %v 都没返回", i+1, r.Took)
				}
				if strings.Contains(r.Error, "dial tcp") {
					first = r
					break
				}
				pooled = append(pooled, r.Server)
			}
			t.Logf("数字：Redis 主机宕机（%s）：池里的连接各读超时一次 %v；第一条走到建连的命令用了 %v（%s）",
				c.name, pooled, first.Server.Round(time.Millisecond), first.Error)

			if first.Server > c.budget+faultSlack {
				t.Errorf("Redis 主机宕机时一条命令按文档最多 %v（%s），实测 %v",
					c.budget, c.why, first.Server.Round(time.Millisecond))
			}
		})
	}
}

// 调用方给了截止时间，gorm 的查询就在那一刻返回：xgorm/README.md 没有单写这一条，
// 依据是 xgorm.CWithCtx 的注释「取实例并绑定 ctx，链路和超时才能传到下游」。
// 池里的连接（卡在读上）和新建的连接（卡在 startup 或 SYN 上）都要听，所以连发 6 次
func TestFault_PGSlowOrDown_QueryReturnsAtCallerDeadline(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const deadline = 200 * time.Millisecond
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			pg := harness.NewProxy(t, harness.PGAddr())
			p := harness.Start(t, harness.Options{PGAddr: pg.Addr()})
			m.inject(pg)

			var took []time.Duration
			newConn := 0
			for i := range 6 {
				r := faultDep(p, "db", deadline.String(), 10*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("第 %d 次：调用方给了 %v，应在那一刻以 503 返回，实际 %v", i+1, deadline, r)
				}
				if r.Server > deadline+faultSlack || r.Server < deadline-10*time.Millisecond {
					t.Errorf("第 %d 次：调用方给了 %v，查询应在那一刻返回，实际 %v：%s", i+1, deadline, r.Server, r.Error)
				}
				if strings.Contains(r.Error, "failed to connect") {
					newConn++
				}
				took = append(took, r.Server)
			}
			if newConn == 0 {
				t.Errorf("6 次都用的是池里的连接，没测到新建连接的那半")
			}
			t.Logf("数字：PG %s、调用方给 %v：6 次依次用了 %v；%s；其中新建连接 %d 次", m.name, deadline, took, faultSummary(took), newConn)
		})
	}
}

// xgorm/README.md XGorm.DialTimeout：「PostgreSQL 注入 connect_timeout（向上取整为秒）」，
// 以及「pgx 拿 connect_timeout 管的是每个主机的整个建连……实测 TCP 秒连、startup 不回话的服务端，
// connect_timeout=1 等满 1.0s」。
//
// 调用方给一个比它宽得多的截止时间（5s），新建连接就该在 connect_timeout 那一刻失败：
// 默认 500ms → 1s；配 1500ms → 向上取整成 2s。两种故障（startup 不回话、SYN 没回音）都一样
func TestFault_PGSlowOrDown_NewConnFailsAtInjectedConnectTimeout(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	for _, dial := range []struct {
		cfg  string
		want time.Duration
	}{{"", time.Second}, {"1500ms", 2 * time.Second}} {
		for _, m := range faultSilentModes {
			name := fmt.Sprintf("DialTimeout=%s_%s", faultOr(dial.cfg, "默认500ms"), m.name)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				pg := harness.NewProxy(t, harness.PGAddr())
				o := harness.Options{PGAddr: pg.Addr()}
				if dial.cfg != "" {
					o.Overlay = "XGorm:\n  Clients:\n    default:\n      DialTimeout: " + dial.cfg + "\n"
				}
				p := harness.Start(t, o)
				m.inject(pg)
				// 先用一个短截止时间把池里现成的连接耗掉（它们卡在读上，connect_timeout 管不到）
				for i := 0; ; i++ {
					if i == 10 {
						t.Fatal("10 次短截止时间的查询都没走到新建连接")
					}
					if r := faultDep(p, "db", "100ms", 10*time.Second); strings.Contains(r.Error, "failed to connect") {
						break
					}
				}
				var took []time.Duration
				for i := range 2 {
					r := faultDep(p, "db", "5s", 10*time.Second)
					if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "failed to connect") {
						t.Fatalf("第 %d 次：新建连接应失败（503，failed to connect），实际 %v", i+1, r)
					}
					if !strings.Contains(r.Error, pg.Addr()) {
						t.Errorf("第 %d 次：错误里应有连不上的地址 %s，实际 %q", i+1, pg.Addr(), r.Error)
					}
					if r.Server < dial.want-20*time.Millisecond || r.Server > dial.want+faultSlack {
						t.Errorf("第 %d 次：文档说 DialTimeout 注入为 connect_timeout（向上取整为秒）=%v，新建连接应在那时失败，实际 %v",
							i+1, dial.want, r.Server)
					}
					took = append(took, r.Server)
				}
				t.Logf("数字：PG %s、DialTimeout=%s、调用方给 5s：新建连接依次在 %v 失败（connect_timeout=%v）",
					m.name, faultOr(dial.cfg, "500ms"), took, dial.want)
			})
		}
	}
}

// 调用方没给截止时间、池里的连接遇上不回话的 PG：xgorm/README.md「PostgreSQL」——
// Postgres.StatementTimeout「默认不限制」，PG 也没有 MySQL 那样的 ReadTimeout 可配，
// 而且 statement_timeout 是服务端的 GUC，网络那头没了它根本管不到。
// 所以按文档，这个查询会一直等下去。这里钉住这个行为：3s 后仍没返回。
// 哪天框架给 PG 加了默认的读超时，这条会红，那时连文档一起改。
//
// 另一半：调用方放弃（断开连接）时请求的 ctx 被取消，查询跟着返回——handler 不会一直挂在那里
func TestFault_PGSilentNoCallerDeadline_QueryWaitsUntilCallerGivesUp(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const giveUp = 3 * time.Second
	pg := harness.NewProxy(t, harness.PGAddr())
	p := harness.Start(t, harness.Options{PGAddr: pg.Addr()})
	pg.SetDelay(time.Hour)

	r := faultDep(p, "db", "", giveUp)
	if r.ReqErr == nil {
		t.Fatalf("文档说 StatementTimeout 默认不限制、PG 没有读超时：不给截止时间的查询应一直等，实际 %v 就返回了：%v", r.Took, r)
	}
	gaveUpAt := time.Now()
	t.Logf("数字：PG 不回话、不给截止时间：%v 后查询仍没返回，客户端放弃", r.Took.Round(time.Millisecond))

	l, ok := p.LookForLog(5*time.Second, func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/dep" })
	if !ok {
		t.Fatalf("调用方断开之后请求的 ctx 被取消，查询应跟着返回（xgorm.CWithCtx：「链路和超时才能传到下游」）：5s 内 handler 仍没返回，没有 /dep 的访问日志")
	}
	t.Logf("数字：客户端放弃之后 %s，handler 返回并记了访问日志（status=%s）", faultMS(time.Since(gaveUpAt)), l.Str("status"))
}

// faultMedian 中位数
func faultMedian(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s[len(s)/2]
}

// faultOr v 为空时用 def
func faultOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
