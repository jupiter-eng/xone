package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covCacheGet GET /probe/cache?op=get
func covCacheGet(t *testing.T, p *harness.Process, name, key string) bool {
	t.Helper()
	r := p.Get(t, "/probe/cache?op=get&name="+name+"&key="+key)
	if r.Status != http.StatusOK {
		t.Fatalf("GET /probe/cache get %s/%s：%v", name, key, r)
	}
	return r.Map(t)["found"] == true
}

func covCacheSet(t *testing.T, p *harness.Process, name, key, ttl string) {
	t.Helper()
	if r := p.Get(t, "/probe/cache?op=set&name="+name+"&key="+key+"&val=v&ttl="+ttl); r.Status != http.StatusOK || r.Map(t)["accepted"] != true {
		t.Fatalf("GET /probe/cache set %s/%s：%v", name, key, r)
	}
}

// xcache/README.md XCache：
//
//	MaxCost     「本包写入时 cost 固定为 1，所以等价于条目数」，不含 ristretto 每条 56 字节的内部开销
//	            （算进去的话 MaxCost: 2000 实际只存得下 35 条）
//	DefaultTTL  包级 Set 用的过期时间；0 是永不过期；负数启动失败
//	多实例      XCache: {Clients: {hot: {...}, cold: {...}}}，C("name") 取
func TestCoverage_LocalCacheMultiInstanceOwnCapacityAndTTL(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	cfg := covConfig(t, map[string]string{"XCache": `XCache:
  Clients:
    default: {NumCounters: 10000, MaxCost: 200, DefaultTTL: 30s}
    short:   {NumCounters: 10000, MaxCost: 1000, DefaultTTL: 1s}
    forever: {NumCounters: 10000, MaxCost: 1000, DefaultTTL: 0s}
`})
	p := harness.Start(t, harness.Options{Config: cfg})

	t.Run("实例之间互不相干", func(t *testing.T) {
		covCacheSet(t, p, "forever", "iso", "")
		if !covCacheGet(t, p, "forever", "iso") {
			t.Fatalf("写进 forever 的键应读得到")
		}
		if covCacheGet(t, p, "short", "iso") || covCacheGet(t, p, "", "iso") {
			t.Errorf("写进 forever 的键不该出现在 short / default 里")
		}
		if r := p.Get(t, "/probe/cache?op=get&name=nope&key=x"); r.Status != http.StatusInternalServerError {
			t.Errorf("C(\"nope\") 名字写错时应 panic（恢复中间件回 500），实际 %v", r)
		}
	})

	t.Run("DefaultTTL 按实例生效，0 是永不过期", func(t *testing.T) {
		covCacheSet(t, p, "short", "ttl", "")
		covCacheSet(t, p, "forever", "ttl", "")
		if !covCacheGet(t, p, "short", "ttl") {
			t.Fatalf("刚写进 short 的键应读得到")
		}
		start := time.Now()
		for covCacheGet(t, p, "short", "ttl") && time.Since(start) < 5*time.Second {
			time.Sleep(20 * time.Millisecond)
		}
		gone := time.Since(start)
		// 下界照旧卡紧（没到 1s 就不见了是真 bug）；上界给 2s：实测约 1s，并行 + -race 的机器上
		// 轮询慢几倍也不到 2s，而配错的样子是 30s（default 的 TTL）或永不过期（轮询到 5s 为止）
		if gone < 800*time.Millisecond || gone > 2*time.Second {
			t.Errorf("short 的 DefaultTTL 是 1s，键应在 1s 左右过期，实际 %v", gone)
		}
		if !covCacheGet(t, p, "forever", "ttl") {
			t.Errorf("DefaultTTL: 0 是永不过期，forever 里的键 %v 后不见了", gone)
		}
		t.Logf("数字：DefaultTTL 1s 的键在写入后约 %v 读不到了", gone.Round(10*time.Millisecond))
	})

	t.Run("MaxCost 等价于条目数", func(t *testing.T) {
		// 默认实例走包级 xcache.Set（cost 1），往 MaxCost: 200 里写 1000 个不同的键
		r := p.Get(t, "/probe/cache/fill?n=1000")
		var body struct {
			Written, Stored int
			MaxCost         int `json:"max_cost"`
		}
		r.JSON(t, &body)
		// 准入策略会拒掉一部分新键，存下的不会超过 MaxCost；ristretto 默认把 56 字节的内部开销
		// 算进 cost 的话，200 的预算一条都存不下（每条 57）
		if body.Stored > 200 || body.Stored < 100 {
			t.Errorf("MaxCost: 200、cost 固定为 1 时应存下接近 200 条（且不超过），实际写 %d 存下 %d", body.Written, body.Stored)
		}
		t.Logf("数字：MaxCost 200 写 1000 个键，存下 %d 个", body.Stored)
	})

	t.Run("DefaultTTL 为负时启动失败", func(t *testing.T) {
		cfg := covConfig(t, map[string]string{"XCache": "XCache:\n  DefaultTTL: -1s\n"})
		faultMustContain(t, "DefaultTTL: -1s 的启动错误", covStartupError(t, harness.Options{Config: cfg}), "xcache", "DefaultTTL")
	})
}

// covRedisDo GET /probe/redis，回状态码、服务端量的耗时和 body
func covRedisDo(t *testing.T, p *harness.Process, name, op, key, val string) (int, time.Duration, map[string]any) {
	t.Helper()
	r := p.Get(t, fmt.Sprintf("/probe/redis?name=%s&op=%s&key=%s&val=%s", name, op, key, val))
	m := r.Map(t)
	ms, _ := m["elapsed_ms"].(float64)
	return r.Status, time.Duration(ms * float64(time.Millisecond)), m
}

// xredis/README.md XRedis：多实例写在 Clients 下面，每个实例一套自己的字段（DB、重试……）；
// MaxRetries「0 交给 go-redis（3 次），-1 关闭」，MinRetryBackoff / MaxRetryBackoff 是两次重试之间的退避。
// Redis 拒绝连接时一条命令要多久几乎全是退避：拨号当场失败，剩下的是 MaxRetries 次退避
func TestCoverage_RedisMultiInstanceSeparateConns_RetryAndBackoffPerInstance(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	flaky, noretry := harness.NewProxy(t, harness.RedisAddr()), harness.NewProxy(t, harness.RedisAddr())
	cfg := covConfig(t, map[string]string{"XRedis": fmt.Sprintf(`XRedis:
  Clients:
    default: {Addr: "${E2E_REDIS_ADDR}"}
    second:  {Addr: "${E2E_REDIS_ADDR}", DB: 1}
    flaky:   {Addr: %q, MinIdleConns: 0, MaxRetries: 2, MinRetryBackoff: 300ms, MaxRetryBackoff: 300ms}
    noretry: {Addr: %q, MinIdleConns: 0, MaxRetries: -1, MinRetryBackoff: 300ms, MaxRetryBackoff: 300ms}
`, flaky.Addr(), noretry.Addr())})
	p := harness.Start(t, harness.Options{Config: cfg})
	rc := harness.Redis(t)
	ctx := context.Background()

	t.Run("second 写的是 DB 1", func(t *testing.T) {
		status, _, body := covRedisDo(t, p, "second", "set", "multi", "in-db1")
		if status != http.StatusOK {
			t.Fatalf("SET 经 second：%v", body)
		}
		key := fmt.Sprint(body["key"])
		db1 := redis.NewClient(&redis.Options{Addr: harness.RedisAddr(), DB: 1})
		defer db1.Close()
		t.Cleanup(func() { db1.Del(context.Background(), key) })
		if v, err := db1.Get(ctx, key).Result(); err != nil || v != "in-db1" {
			t.Errorf("second 配的是 DB: 1，键应在 DB 1 里，实际 %q（%v）", v, err)
		}
		if n, _ := rc.Exists(ctx, key).Result(); n != 0 {
			t.Errorf("second 写的键不该出现在 DB 0（default 用的库）")
		}
		if _, _, body := covRedisDo(t, p, "", "get", "multi", ""); body["val"] != "" || body["db"] != float64(0) {
			t.Errorf("default 连的是 DB 0，读不到 second 写进 DB 1 的键，实际 %v", body)
		}
		m := p.Metrics(t)
		for _, name := range []string{"default", "second", "flaky", "noretry"} {
			if len(m.Find("e2e_redis_pool_connections", "name", name)) != 1 {
				t.Errorf("XRedis.Metric 按实例生效，应有 e2e_redis_pool_connections{name=%q}", name)
			}
		}
	})

	t.Run("拒绝连接时 MaxRetries × 退避", func(t *testing.T) {
		flaky.Cut()
		noretry.Cut()
		var withRetry, without []time.Duration
		for range 3 {
			s1, d1, b1 := covRedisDo(t, p, "flaky", "get", "k", "")
			s2, d2, b2 := covRedisDo(t, p, "noretry", "get", "k", "")
			if s1 != http.StatusServiceUnavailable || s2 != http.StatusServiceUnavailable {
				t.Fatalf("Redis 拒绝连接时命令应失败，实际 flaky=%v noretry=%v", b1, b2)
			}
			withRetry, without = append(withRetry, d1), append(without, d2)
		}
		// MaxRetries: 2 → 3 次尝试、2 次退避，每次退避正好 300ms（Min = Max）
		for _, d := range withRetry {
			if d < 600*time.Millisecond-20*time.Millisecond || d > 600*time.Millisecond+faultSlack {
				t.Errorf("MaxRetries: 2、退避 300ms 时一条命令应在 600ms 左右失败，实际 %v", d)
			}
		}
		// noretry 的退避同样是 300ms：MaxRetries: -1 没生效的话哪怕重试一次也至少 300ms。
		// 上界就取这一次退避——实测几毫秒，不必再卡得更紧
		for _, d := range without {
			if d >= 300*time.Millisecond {
				t.Errorf("MaxRetries: -1 关掉重试时拒绝连接应当场失败，实际 %v", d)
			}
		}
		t.Logf("数字：拒绝连接时 MaxRetries 2 + 退避 300ms：%s；MaxRetries -1：%s", faultSummary(withRetry), faultSummary(without))
	})
}

// xhttp/README.md「行为与实测」那张表：「cookie jar：resty.New() 自带一个，同一 client 的所有请求共享会话 cookie ｜ 这里：没有」。
// 下游第一次响应种一个 cookie，之后经 xhttp 的每次调用都不该把它带回去
func TestCoverage_XhttpNoCookieJar_DownstreamCookiesDontLeakToNextCall(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	var (
		mu      sync.Mutex
		cookies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cookies = append(cookies, r.Header.Get("Cookie"))
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-of-caller-" + harness.NewID(), Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	p := harness.Start(t, harness.Options{Downstream: srv.URL})
	for i := range 3 {
		if r := p.Get(t, fmt.Sprintf("/proxy?token=caller-%d", i)); r.Status != http.StatusOK {
			t.Fatalf("GET /proxy：%v", r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cookies) != 3 {
		t.Fatalf("下游应收到 3 次调用，实际 %d", len(cookies))
	}
	for i, c := range cookies {
		if c != "" {
			t.Errorf("第 %d 次调用带着上一次下游种的 cookie %q：xhttp 不该有 cookie jar", i+1, c)
		}
	}
}
