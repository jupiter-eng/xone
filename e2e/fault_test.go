package e2e

// 故障端到端测试：TestFault_* 系列。
//
//	scripts/e2e.sh -run Fault
//
// 故障一律经 harness 的 TCP 代理注入，不去停真实的 PG / Redis：
//
//	Proxy.Cut()        进程挂了：现有连接断开，新连接 connection refused
//	Proxy.Blackhole()  主机宕机：现有连接不再回话，新连接的 SYN 没有回音
//	Proxy.SetDelay(d)  对端变慢；time.Hour 就是「连得上、但一个字节都不回」
//
// 断言对照 README.md / docs/architecture.md / docs/config.md / 各模块 README 写下的行为，失败信息写成「文档说 X，实际 Y」；
// 文档没写死的数（比如「恢复要多久」）只量、不卡，打在 t.Logf 里。
// 数字都以「数字：」开头，方便从 -v 的输出里 grep。
//
// 文件划分：
//
//	fault_test.go             本文件：共用的小工具，运行中切断 Redis / PG
//	fault_timeout_test.go     go-redis / gorm 的超时听不听调用方的 ctx、听不听配置
//	fault_downstream_test.go  下游慢、下游报错时 xhttp 的超时与重试
//	fault_startup_test.go     启动时依赖不可达、密码错、DSN 写错
//
// 耗时的余量：本机（4 核）上故障路径的耗时由超时决定，抖动在几毫秒到几十毫秒；
// 断言的上限在文档推出来的数上统一加 faultSlack，理由见它的注释。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// faultSlack 文档推出来的时长上限之外再给的余量。
//
// 被测的耗时由超时决定（几百毫秒到几秒），本机实测它们比超时多出来的部分
// 在 1–30ms（全部 TestFault 并行、-count=3）。给到 300ms，是量出来的十倍，
// 又远小于任何一条被测的超时，不会把「超时没生效」放过去
const faultSlack = 300 * time.Millisecond

// faultUnaffected 「不碰出故障的依赖，照常而且快」的上限。
//
// 这些请求实测 1–5ms（全部 TestFault 并行）。出故障的依赖真把它们拖住的话，
// 拖的是一次超时，被测的超时里最短的是 500ms（XRedis.DialTimeout / ReadTimeout、XGorm.DialTimeout）。
// 给到 400ms：比量出来的大两个数量级，服务带 -race、机器满载也够，又仍短于最短的那个超时
const faultUnaffected = 400 * time.Millisecond

// xredis/README.md XRedis 的默认值；service/application.yml 没改这几项
const (
	faultRedisDialTimeout     = 500 * time.Millisecond
	faultRedisReadTimeout     = 500 * time.Millisecond
	faultRedisMaxRetries      = 3           // 「MaxRetries: 0 交给 go-redis（3 次）」
	faultRedisMaxRetryBackoff = time.Second // 「MaxRetryBackoff: 0 交给 go-redis（1s）」
)

// faultRedisAttemptBudget 一次尝试的上限：建连加一个往返。
// 这是 xredis 自己的模型——xredis.go 的 pingTimeout 就是这么算的（DialTimeout + ReadTimeout）
const faultRedisAttemptBudget = faultRedisDialTimeout + faultRedisReadTimeout

// faultRedisCmdBudget 按文档的默认值推出来的一条命令最多要多久：
// MaxRetries+1 次尝试，每次不超过 faultRedisAttemptBudget，两次之间的退避不超过 MaxRetryBackoff
const faultRedisCmdBudget = (faultRedisMaxRetries+1)*faultRedisAttemptBudget + faultRedisMaxRetries*faultRedisMaxRetryBackoff // 7s

// faultDepResult GET /dep 的结果
type faultDepResult struct {
	Status int
	Took   time.Duration // 客户端量的整个请求
	Server time.Duration // handler 里量的那一下依赖操作
	Error  string        // 依赖操作的错误
	ReqErr error         // 请求本身没完成（客户端到点放弃）时非 nil
}

func (r faultDepResult) String() string {
	if r.ReqErr != nil {
		return fmt.Sprintf("请求 %v 后放弃：%v", r.Took.Round(time.Millisecond), r.ReqErr)
	}
	return fmt.Sprintf("status=%d 用时 %v（依赖操作 %v）error=%q", r.Status, r.Took.Round(time.Millisecond), r.Server.Round(time.Millisecond), r.Error)
}

// faultDep 调 GET /dep?target=...[&timeout=...]，客户端最多等 wait。不带 t，哪个协程里都能调
func faultDep(p *harness.Process, target, timeout string, wait time.Duration) faultDepResult {
	path := "/dep?target=" + target
	if timeout != "" {
		path += "&timeout=" + timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	start := time.Now()
	r, err := p.Request(ctx, http.MethodGet, path, nil)
	res := faultDepResult{Status: r.Status, Took: time.Since(start), ReqErr: err}
	if err == nil {
		var body struct {
			ElapsedMS float64 `json:"elapsed_ms"`
			Error     string  `json:"error"`
		}
		_ = json.Unmarshal(r.Body, &body)
		res.Server = time.Duration(body.ElapsedMS * float64(time.Millisecond))
		res.Error = body.Error
	}
	return res
}

// faultTimed 发一个请求并计时，失败时 t.Fatal
func faultTimed(t *testing.T, p *harness.Process, method, path string, body any) (harness.Response, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	r, err := p.Request(ctx, method, path, body)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("%s %s 在 %v 后仍没有返回：%v", method, path, took, err)
	}
	return r, took
}

// faultWaitRecover 反复探一个依赖，直到它回 200，返回用了多久；limit 内没恢复就 t.Fatal
func faultWaitRecover(t *testing.T, p *harness.Process, target string, limit time.Duration) (time.Duration, int) {
	t.Helper()
	start := time.Now()
	for n := 1; ; n++ {
		r := faultDep(p, target, "", 10*time.Second)
		if r.Status == http.StatusOK {
			return time.Since(start), n
		}
		if time.Since(start) > limit {
			t.Fatalf("故障恢复之后 %v 内 %s 仍不可用（第 %d 次探测：%v）", limit, target, n, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// faultMS 毫秒，保留一位小数
func faultMS(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }

// faultSummary 一组耗时的最小 / 中位 / 最大
func faultSummary(ds []time.Duration) string {
	if len(ds) == 0 {
		return "（无）"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return fmt.Sprintf("n=%d min=%s p50=%s max=%s", len(s), faultMS(s[0]), faultMS(s[len(s)/2]), faultMS(s[len(s)-1]))
}

// faultGoRedisLogLine go-redis 自带的 logger（internal.Logger，标准库 log 的格式）写出来的一行
var faultGoRedisLogLine = regexp.MustCompile(`(?m)^redis: \d{4}/\d{2}/\d{2} .*$`)

// faultRedisACLUser 在 Redis 上建一个带密码的 ACL 用户，测试结束时删掉。
//
// 本机的 Redis 没有设密码，直接给 xredis 配 Password 会在 AUTH 时被拒；
// 建一个专用的 ACL 用户，才测得到「密码不进日志」。用户名带随机后缀，并行不冲突
func faultRedisACLUser(t *testing.T) (name, password string) {
	t.Helper()
	rc := harness.Redis(t)
	name, password = "e2e_fault_"+harness.NewID(), "redis-secret-"+harness.NewID()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rc.Do(ctx, "ACL", "SETUSER", name, "on", ">"+password, "~*", "&*", "+@all").Err(); err != nil {
		t.Fatalf("create redis ACL user: %v", err)
	}
	t.Cleanup(func() { // 比 rc 的关闭登记得晚，所以先跑，rc 还开着
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rc.Do(ctx, "ACL", "DELUSER", name).Err(); err != nil {
			t.Logf("cleanup: delete redis ACL user %s: %v", name, err)
		}
	})
	return name, password
}

// faultBurst 并发发 n 个同样的 /dep 请求，返回各自的结果
func faultBurst(p *harness.Process, n int, target string, wait time.Duration) []faultDepResult {
	out := make([]faultDepResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = faultDep(p, target, "", wait)
		}()
	}
	wg.Wait()
	return out
}

// faultQuick 断言一个不碰故障依赖的请求照常返回，而且快
func faultQuick(t *testing.T, p *harness.Process, what, path string, want int) harness.Response {
	t.Helper()
	r, took := faultTimed(t, p, http.MethodGet, path, nil)
	if r.Status != want {
		t.Errorf("%s 不碰出故障的依赖，应照常回 %d，实际 %v", what, want, r)
	}
	if took > faultUnaffected {
		t.Errorf("%s 不碰出故障的依赖，应该不受影响（< %v），实际用了 %v", what, faultUnaffected, took)
	}
	t.Logf("数字：故障期间 %s 用了 %s", what, faultMS(took))
	return r
}

// 运行中 Redis 进程挂了（端口拒绝连接）：
//
//   - 用到 Redis 的一下操作在文档推出来的预算（faultRedisCmdBudget）内报错，不挂住；并发地打也一样
//   - 三级读降级到 PG（service/users.go：Redis 出错记 WARN，降级到 PG），下单在扣款那一步失败并回滚
//   - 不碰 Redis 的接口不受影响：/ping、?cache=off、本地缓存命中
//   - Redis 回来之后不用重启就恢复
//   - 全程的输出里没有 Redis 的密码（xredis/README.md XRedis.Password：「本模块不会把它写进任何日志」）
func TestFault_RedisRefusedAtRuntime_ErrsInBudget_ReadsFallBackToPG_Recovers(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	acl, pw := faultRedisACLUser(t)
	rp := harness.NewProxy(t, harness.RedisAddr())
	p := harness.Start(t, harness.Options{
		RedisAddr: rp.Addr(),
		Overlay:   fmt.Sprintf("XRedis:\n  Username: %s\n  Password: %q\n", acl, pw),
	})

	cached := createUser(t, p, "cached", "cached@example.com")
	getUser(t, p, cached.ID) // db → 写进本地缓存和 Redis
	if u, _, _ := getUser(t, p, cached.ID); u.Source != "local" {
		t.Fatalf("故障前第二次读应命中本地缓存，实际 source=%s", u.Source)
	}
	cold := createUser(t, p, "cold", "cold@example.com") // 还没读过：两级缓存里都没有
	if r := faultDep(p, "redis", "", 10*time.Second); r.Status != http.StatusOK {
		t.Fatalf("故障前 Redis 应该是好的：%v", r)
	}

	rp.Cut()

	t.Run("用到Redis的操作在预算内报错", func(t *testing.T) {
		var took []time.Duration
		for i := range 8 {
			r := faultDep(p, "redis", "", 2*faultRedisCmdBudget)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("第 %d 次：Redis 拒绝连接时 /dep 应回 503，实际 %v", i+1, r)
			}
			if !strings.Contains(r.Error, "connection refused") {
				t.Errorf("第 %d 次：错误应说清是连接被拒，实际 %q", i+1, r.Error)
			}
			if r.Server > faultRedisCmdBudget+faultSlack {
				t.Errorf("第 %d 次：按文档的默认值一条命令最多 %v（(MaxRetries+1)×(DialTimeout+ReadTimeout)+MaxRetries×MaxRetryBackoff），实际 %v",
					i+1, faultRedisCmdBudget, r.Server)
			}
			took = append(took, r.Server)
		}
		// 实测 70–150ms（两轮），全是命令级重试的退避：xredis 把 DialerRetries 配成 1，拒绝连接时建连当场失败。
		// 从前 go-redis 默认每次建连内部重拨 5 次、间隔 100ms，前两次命令要 1.69–1.74s
		t.Logf("数字：Redis 拒绝连接时一条命令依次用了 %v；%s（预算 %v）", took, faultSummary(took), faultRedisCmdBudget)

		burst := faultBurst(p, 20, "redis", 2*faultRedisCmdBudget)
		var bt []time.Duration
		for i, r := range burst {
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || r.Server > faultRedisCmdBudget+faultSlack {
				t.Errorf("并发第 %d 个：应在 %v 内回 503，实际 %v", i, faultRedisCmdBudget, r)
			}
			bt = append(bt, r.Took)
		}
		t.Logf("数字：20 个并发 /dep?target=redis：%s", faultSummary(bt))
	})

	t.Run("三级读降级到PG_下单失败并回滚", func(t *testing.T) {
		r, took := faultTimed(t, p, http.MethodGet, fmt.Sprintf("/users/%d", cold.ID), nil)
		var u user
		r.JSON(t, &u)
		if r.Status != http.StatusOK || u.Source != "db" || u.Name != "cold" {
			t.Errorf("Redis 出错时读用户应降级到 PG（200，source=db），实际 %v", r)
		}
		// GET 和回填的 SET 各碰一次 Redis
		if took > 2*faultRedisCmdBudget+faultSlack {
			t.Errorf("一次三级读碰两次 Redis，按文档最多 %v，实际 %v", 2*faultRedisCmdBudget, took)
		}
		trace := traceIDOf(t, r)
		warn, ok := p.LookForLog(waitFor, func(l harness.Log) bool {
			return l.Msg() == "redis read failed, falling back to the database" && l.Str("trace_id") == trace
		})
		if !ok {
			t.Fatalf("降级时应记一条带这个请求 trace_id（%s）的 WARN（redis read failed, falling back to the database），%v 内没有；同名消息 %d 条",
				trace, waitFor, len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "redis read failed, falling back to the database" })))
		}
		if warn.Level() != "WARN" {
			t.Errorf("降级应记 WARN，实际 %s：%s", warn.Level(), warn.Line)
		}
		t.Logf("数字：Redis 拒绝连接时一次降级读用了 %s", faultMS(took))

		r, took = faultTimed(t, p, http.MethodPost, "/orders", map[string]any{"user_id": cold.ID, "amount": 7})
		if r.Status != http.StatusInternalServerError {
			t.Fatalf("Redis 不可用时下单（第 2 步写 Redis）应失败 500，实际 %v", r)
		}
		var o struct {
			ID     string `json:"id"`
			Rolled bool   `json:"rolled"`
			Error  string `json:"error"`
		}
		r.JSON(t, &o)
		if !strings.Contains(o.Error, `step "charge"`) || !o.Rolled {
			t.Errorf("应在 charge 这一步失败、回滚已做完的 create，实际 %v", r)
		}
		if st := orderStatus(t, harness.DB(t), p, o.ID); st != "" {
			t.Errorf("回滚之后 PG 里不该还有这张订单，实际 status=%q", st)
		}
		if took > faultRedisCmdBudget+faultSlack {
			t.Errorf("下单只碰一次 Redis，按文档最多 %v，实际 %v", faultRedisCmdBudget, took)
		}
		t.Logf("数字：Redis 拒绝连接时一次下单（失败 + 回滚）用了 %s", faultMS(took))
	})

	t.Run("不碰Redis的接口不受影响", func(t *testing.T) {
		faultQuick(t, p, "GET /ping", "/ping", http.StatusOK)
		faultQuick(t, p, "GET /users/:id?cache=off（只读 PG）", fmt.Sprintf("/users/%d?cache=off", cold.ID), http.StatusOK)
		var u user
		faultQuick(t, p, "GET /users/:id（本地缓存命中）", fmt.Sprintf("/users/%d", cached.ID), http.StatusOK).JSON(t, &u)
		if u.Source != "local" {
			t.Errorf("本地缓存里有的用户应直接命中 local，实际 source=%s", u.Source)
		}
	})

	t.Run("Redis回来之后自动恢复", func(t *testing.T) {
		rp.Restore()
		took, n := faultWaitRecover(t, p, "redis", 10*time.Second)
		t.Logf("数字：Redis 恢复监听之后 %s（第 %d 次探测）/dep 回到 200", faultMS(took), n)

		fresh := createUser(t, p, "after", "after@example.com")
		if u, _, _ := getUser(t, p, fresh.ID); u.Source != "db" {
			t.Errorf("恢复后第一次读应落到 PG，实际 source=%s", u.Source)
		}
		// 回填 Redis 成功：直连 Redis 看得到这个 key
		key := p.KeyPrefix + "user:" + fmt.Sprint(fresh.ID)
		if n, err := harness.Redis(t).Exists(context.Background(), key).Result(); err != nil || n != 1 {
			t.Errorf("恢复后三级读应把用户回填进 Redis（%s），实际 exists=%d err=%v", key, n, err)
		}
		if r := p.PostJSON(t, "/orders", map[string]any{"user_id": fresh.ID, "amount": 1}); r.Status != http.StatusCreated {
			t.Errorf("恢复后下单应成功，实际 %v", r)
		}
	})

	t.Run("输出里没有Redis密码", func(t *testing.T) {
		exit := p.Terminate(t, 20*time.Second)
		if exit.Code != 0 {
			t.Errorf("SIGTERM 之后应以 0 退出，实际 %v", exit)
		}
		out := p.Output()
		mustNotContain(t, "进程的 stdout / stderr", out, pw)
		if !strings.Contains(out, "connection refused") {
			t.Errorf("这一圈应该在日志里留下过 connection refused，一条都没有：核对范围不够")
		}
		t.Logf("数字：核对了 %d 字节输出", len(out))
	})

	// docs/behavior.md「总表」：GORM 默认的 stdout logger、resty 写 stderr 的 logger
	// 都被接回了 slog，理由是「绕开 slog 的日志进不了日志平台」。go-redis 的 internal.Logger
	// 是同一类东西：xredis 在 init 里用 redis.SetLogger 接到 slog，消息是 xredis go-redis log、
	// 级别 WARN（xredis/README.md XRedis）。这一圈 Redis 拒绝连接，go-redis 每次建连失败都记一句
	// failed to dial。trace_id 不断言：xredis/README.md 写明了建连在 go-redis 自己的协程里、
	// 用它自己的 context.Background()，这类日志带不上请求的 trace_id（实测 45 条一条都没有）
	t.Run("go-redis自己的日志也走slog", func(t *testing.T) {
		lines := faultGoRedisLogLine.FindAllString(p.Stderr(), -1)
		if len(lines) > 0 {
			t.Errorf("go-redis 自己的日志应进 slog，实际 stderr 里有 %d 行纯文本，例：%s", len(lines), lines[0])
		}
		logs := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xredis go-redis log" })
		if len(logs) == 0 {
			t.Fatalf("Redis 拒绝连接时 go-redis 会记 failed to dial，slog 里应当有 xredis go-redis log，一条都没有")
		}
		traced := 0
		for _, l := range logs {
			if l.Level() != "WARN" {
				t.Errorf("go-redis 的日志应记成 WARN，实际 %s", l.Line)
			}
			if l.Str("trace_id") != "" {
				traced++
			}
		}
		t.Logf("数字：slog 里有 %d 条 go-redis 自己的日志，%d 条带 trace_id；例：%s", len(logs), traced, logs[0].Line)
	})
}

// 运行中 PG 进程挂了（端口拒绝连接）：
//
//   - 用到 PG 的操作当场报错（新连接被拒不需要等任何超时），错误里有地址、没有密码
//   - 不碰 PG 的接口不受影响：/ping、Redis、本地缓存命中
//   - PG 回来之后不用重启就恢复
func TestFault_PGRefusedAtRuntime_FailsFast_OthersUnaffected_Recovers(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	pg := harness.NewProxy(t, harness.PGAddr())
	// DialTimeout 从默认的 500ms 调到 2s：「被拒不等超时」的上限取 1s，离实测（几毫秒）
	// 和反例（等满一次 DialTimeout）都远。上限贴着 500ms 的话，满载的机器上会误报
	p := harness.Start(t, harness.Options{PGAddr: pg.Addr(), Overlay: "XGorm:\n  Clients:\n    default:\n      DialTimeout: 2s\n"})

	cached := createUser(t, p, "cached", "cached@example.com")
	getUser(t, p, cached.ID)
	if u, _, _ := getUser(t, p, cached.ID); u.Source != "local" {
		t.Fatalf("故障前第二次读应命中本地缓存，实际 source=%s", u.Source)
	}

	pg.Cut()

	t.Run("用到PG的操作当场报错", func(t *testing.T) {
		var took []time.Duration
		for i := range 5 {
			r := faultDep(p, "db", "", 30*time.Second)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("第 %d 次：PG 拒绝连接时 /dep?target=db 应回 503，实际 %v", i+1, r)
			}
			if !strings.Contains(r.Error, "connection refused") || !strings.Contains(r.Error, pg.Addr()) {
				t.Errorf("第 %d 次：错误应说清连不上哪（%s，connection refused），实际 %q", i+1, pg.Addr(), r.Error)
			}
			// 连接被拒是立即的：database/sql 丢掉坏连接重拨，pgx 拨号立刻失败，没有要等的超时。
			// 上限给 DialTimeout（上面调成了 2s）的一半：真等了哪怕一次超时也会超过它
			if r.Server > time.Second {
				t.Errorf("第 %d 次：新连接被拒不该等任何超时，实际 %v", i+1, r.Server)
			}
			took = append(took, r.Server)
		}
		t.Logf("数字：PG 拒绝连接时 SELECT 1 依次用了 %v；%s", took, faultSummary(took))

		r, took2 := faultTimed(t, p, http.MethodPost, "/users", map[string]string{"name": "while-cut"})
		if r.Status != http.StatusInternalServerError {
			t.Errorf("PG 不可用时建用户应 500，实际 %v", r)
		}
		r, took3 := faultTimed(t, p, http.MethodPost, "/orders", map[string]any{"user_id": cached.ID, "amount": 1})
		var o struct {
			Rolled bool   `json:"rolled"`
			Error  string `json:"error"`
		}
		r.JSON(t, &o)
		if r.Status != http.StatusInternalServerError || !strings.Contains(o.Error, `step "create"`) || o.Rolled {
			t.Errorf("PG 不可用时下单应在第 1 步失败、没有要回滚的，实际 %v", r)
		}
		t.Logf("数字：PG 拒绝连接时 POST /users 用了 %s，POST /orders 用了 %s", faultMS(took2), faultMS(took3))
	})

	t.Run("不碰PG的接口不受影响", func(t *testing.T) {
		faultQuick(t, p, "GET /ping", "/ping", http.StatusOK)
		faultQuick(t, p, "GET /dep?target=redis", "/dep?target=redis", http.StatusOK)
		var u user
		faultQuick(t, p, "GET /users/:id（本地缓存命中）", fmt.Sprintf("/users/%d", cached.ID), http.StatusOK).JSON(t, &u)
		if u.Source != "local" {
			t.Errorf("本地缓存里有的用户应直接命中 local，实际 source=%s", u.Source)
		}
	})

	t.Run("PG回来之后自动恢复", func(t *testing.T) {
		pg.Restore()
		took, n := faultWaitRecover(t, p, "db", 10*time.Second)
		t.Logf("数字：PG 恢复监听之后 %s（第 %d 次探测）SELECT 1 回到 200", faultMS(took), n)
		fresh := createUser(t, p, "after", "after@example.com")
		if u, _, _ := getUser(t, p, fresh.ID); u.Source != "db" || u.Name != "after" {
			t.Errorf("恢复后读写应照常，实际 %+v", u)
		}
	})

	t.Run("输出里没有PG密码", func(t *testing.T) {
		exit := p.Terminate(t, 20*time.Second)
		if exit.Code != 0 {
			t.Errorf("SIGTERM 之后应以 0 退出，实际 %v", exit)
		}
		out := p.Output()
		mustNotContain(t, "进程的 stdout / stderr", out, pw, harness.PGDSN(pg.Addr()))
		if !strings.Contains(out, "connection refused") {
			t.Errorf("这一圈应该在日志里留下过 connection refused，一条都没有：核对范围不够")
		}
		t.Logf("数字：核对了 %d 字节输出", len(out))
	})
}
