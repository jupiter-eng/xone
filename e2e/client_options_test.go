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
		t.Fatalf("GET /probe/cache get %s/%s: %v", name, key, r)
	}
	return r.Map(t)["found"] == true
}

func covCacheSet(t *testing.T, p *harness.Process, name, key, ttl string) {
	t.Helper()
	if r := p.Get(t, "/probe/cache?op=set&name="+name+"&key="+key+"&val=v&ttl="+ttl); r.Status != http.StatusOK || r.Map(t)["accepted"] != true {
		t.Fatalf("GET /probe/cache set %s/%s: %v", name, key, r)
	}
}

// xcache/README.md, XCache:
//
//	MaxCost     "this package writes with a cost fixed at 1, so it amounts to a number of entries", and does not count ristretto's 56 bytes of internal overhead per entry
//	            (counted in, MaxCost: 2000 would really hold 35 entries)
//	DefaultTTL  the expiry the package-level Set uses; 0 never expires; a negative value fails the start
//	several instances  XCache: {Clients: {hot: {...}, cold: {...}}}, taken with C("name")
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

	t.Run("the instances do not touch each other", func(t *testing.T) {
		covCacheSet(t, p, "forever", "iso", "")
		if !covCacheGet(t, p, "forever", "iso") {
			t.Fatalf("a key written into forever should be readable")
		}
		if covCacheGet(t, p, "short", "iso") || covCacheGet(t, p, "", "iso") {
			t.Errorf("a key written into forever should not turn up in short or in default")
		}
		if r := p.Get(t, "/probe/cache?op=get&name=nope&key=x"); r.Status != http.StatusInternalServerError {
			t.Errorf("C(\"nope\") with a wrong name should panic (and the recovery middleware answer 500), got %v", r)
		}
	})

	t.Run("DefaultTTL takes per instance, and 0 never expires", func(t *testing.T) {
		covCacheSet(t, p, "short", "ttl", "")
		covCacheSet(t, p, "forever", "ttl", "")
		if !covCacheGet(t, p, "short", "ttl") {
			t.Fatalf("a key just written into short should be readable")
		}
		start := time.Now()
		for covCacheGet(t, p, "short", "ttl") && time.Since(start) < 5*time.Second {
			time.Sleep(20 * time.Millisecond)
		}
		gone := time.Since(start)
		// The lower bound stays tight (disappearing before 1s is a real bug); the upper bound is 2s: measured at about 1s, and on a machine
		// running in parallel with -race a poll several times slower still stays under 2s, while a misconfiguration looks like 30s (default's TTL) or never expiring (polling up to 5s)
		if gone < 800*time.Millisecond || gone > 2*time.Second {
			t.Errorf("short's DefaultTTL is 1s and the key should expire at about 1s, got %v", gone)
		}
		if !covCacheGet(t, p, "forever", "ttl") {
			t.Errorf("DefaultTTL: 0 never expires, and the key in forever was gone after %v", gone)
		}
		t.Logf("numbers: a key with DefaultTTL 1s became unreadable about %v after it was written", gone.Round(10*time.Millisecond))
	})

	t.Run("MaxCost amounts to a number of entries", func(t *testing.T) {
		// The default instance goes through the package-level xcache.Set (cost 1), writing 1000 distinct keys into MaxCost: 200
		r := p.Get(t, "/probe/cache/fill?n=1000")
		var body struct {
			Written, Stored int
			MaxCost         int `json:"max_cost"`
		}
		r.JSON(t, &body)
		// The admission policy turns some new keys away and what is stored never passes MaxCost; were ristretto to count its 56 bytes
		// of internal overhead into the cost, a budget of 200 would hold not one entry (57 each)
		if body.Stored > 200 || body.Stored < 100 {
			t.Errorf("with MaxCost: 200 and a cost fixed at 1 it should store close to 200 entries (and no more), and writing %d stored %d", body.Written, body.Stored)
		}
		t.Logf("numbers: with MaxCost 200, writing 1000 keys stored %d", body.Stored)
	})

	t.Run("a negative DefaultTTL fails the start", func(t *testing.T) {
		cfg := covConfig(t, map[string]string{"XCache": "XCache:\n  DefaultTTL: -1s\n"})
		faultMustContain(t, "the start error for DefaultTTL: -1s", covStartupError(t, harness.Options{Config: cfg}), "xcache", "DefaultTTL")
	})
}

// covRedisDo GETs /probe/redis and returns the status code, the time the server measured and the body
func covRedisDo(t *testing.T, p *harness.Process, name, op, key, val string) (int, time.Duration, map[string]any) {
	t.Helper()
	r := p.Get(t, fmt.Sprintf("/probe/redis?name=%s&op=%s&key=%s&val=%s", name, op, key, val))
	m := r.Map(t)
	ms, _ := m["elapsed_ms"].(float64)
	return r.Status, time.Duration(ms * float64(time.Millisecond)), m
}

// xredis/README.md, XRedis: several instances are written under Clients, each with its own fields
// (DB, the retries and so on); MaxRetries "0 leaves it to go-redis (3 times) and -1 turns it off",
// and MinRetryBackoff / MaxRetryBackoff are the backoff between two retries.
// While Redis refuses connections, how long a command takes is almost all backoff: the dial fails
// there and then, and the rest is MaxRetries backoffs
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

	t.Run("second writes into DB 1", func(t *testing.T) {
		status, _, body := covRedisDo(t, p, "second", "set", "multi", "in-db1")
		if status != http.StatusOK {
			t.Fatalf("a SET through second: %v", body)
		}
		key := fmt.Sprint(body["key"])
		db1 := redis.NewClient(&redis.Options{Addr: harness.RedisAddr(), DB: 1})
		defer db1.Close()
		t.Cleanup(func() { db1.Del(context.Background(), key) })
		if v, err := db1.Get(ctx, key).Result(); err != nil || v != "in-db1" {
			t.Errorf("second is configured with DB: 1 and the key should be in DB 1, got %q (%v)", v, err)
		}
		if n, _ := rc.Exists(ctx, key).Result(); n != 0 {
			t.Errorf("a key second wrote should not turn up in DB 0 (the one default uses)")
		}
		if _, _, body := covRedisDo(t, p, "", "get", "multi", ""); body["val"] != "" || body["db"] != float64(0) {
			t.Errorf("default connects to DB 0 and cannot read the key second wrote into DB 1, got %v", body)
		}
		m := p.Metrics(t)
		for _, name := range []string{"default", "second", "flaky", "noretry"} {
			if len(m.Find("e2e_redis_pool_connections", "name", name)) != 1 {
				t.Errorf("XRedis.Metric takes per instance and there should be an e2e_redis_pool_connections{name=%q}", name)
			}
		}
	})

	t.Run("with connections refused it is MaxRetries x the backoff", func(t *testing.T) {
		flaky.Cut()
		noretry.Cut()
		var withRetry, without []time.Duration
		for range 3 {
			s1, d1, b1 := covRedisDo(t, p, "flaky", "get", "k", "")
			s2, d2, b2 := covRedisDo(t, p, "noretry", "get", "k", "")
			if s1 != http.StatusServiceUnavailable || s2 != http.StatusServiceUnavailable {
				t.Fatalf("a command should fail while Redis refuses connections, got flaky=%v noretry=%v", b1, b2)
			}
			withRetry, without = append(withRetry, d1), append(without, d2)
		}
		// MaxRetries: 2 -> 3 attempts and 2 backoffs, each exactly 300ms (Min = Max)
		for _, d := range withRetry {
			if d < 600*time.Millisecond-20*time.Millisecond || d > 600*time.Millisecond+faultSlack {
				t.Errorf("with MaxRetries: 2 and a 300ms backoff a command should fail at about 600ms, got %v", d)
			}
		}
		// noretry's backoff is 300ms as well: were MaxRetries: -1 not taking, even one retry would be 300ms at least.
		// The upper bound is that one backoff -- measured in milliseconds, which needs no tighter bound
		for _, d := range without {
			if d >= 300*time.Millisecond {
				t.Errorf("with MaxRetries: -1 turning the retries off, a refused connection should fail there and then, got %v", d)
			}
		}
		t.Logf("numbers: with connections refused, MaxRetries 2 plus a 300ms backoff: %s; MaxRetries -1: %s", faultSummary(withRetry), faultSummary(without))
	})
}

// The table in xhttp/README.md, "Behavior (measured)": "the cookie jar: resty.New() brings one, and
// every request of one client shares the session cookies | here: none".
// The downstream's first response sets a cookie, and no later call through xhttp may carry it back
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
			t.Fatalf("GET /proxy: %v", r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cookies) != 3 {
		t.Fatalf("the downstream should see 3 calls, got %d", len(cookies))
	}
	for i, c := range cookies {
		if c != "" {
			t.Errorf("call %d carried the cookie %q the downstream set on the previous one: xhttp should have no cookie jar", i+1, c)
		}
	}
}
