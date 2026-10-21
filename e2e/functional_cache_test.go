package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// GET /users/:id reads through three levels: the local cache (xcache) -> Redis (xredis) -> PG
// (xgorm).
// source in the response is what the service says about itself, so every level gets evidence of
// its own:
//
//	db     the trace holds a gorm.query span, and after the read Redis holds this key
//	redis  a value different from PG's was put into Redis directly and that is what comes back; the trace holds a get alone and no SQL
//	local  the trace holds not one client span: neither Redis nor PG was asked
func TestFunctional_ThreeTierReadHitsLocalCacheRedisAndPG(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Spans: true})
	rdb := harness.Redis(t)
	ctx := context.Background()
	keyOf := func(id int64) string { return p.KeyPrefix + "user:" + strconv.FormatInt(id, 10) }

	u := createUser(t, p, "tier", "tier@example.com")
	key := keyOf(u.ID)
	if n := rdb.Exists(ctx, key).Val(); n != 0 {
		t.Fatalf("the cache of a user that was just created should be empty (a POST drops both levels), and Redis already holds %s", key)
	}

	// The first read: neither cache level holds it, so it reaches PG and fills Redis and the local cache back in
	got, r, dbElapsed := getUser(t, p, u.ID)
	if got.Source != "db" {
		t.Fatalf("the first read should have source db, got %v", r)
	}
	serverSpan(t, p, traceIDOf(t, r))
	spans := traceSpans(t, p, traceIDOf(t, r))
	if got := clientSpanNames(spans); got != "get,gorm.query,set" {
		t.Errorf("the read that reached PG should hold, in order, a Redis get (a miss) -> gorm.query -> a Redis set (the fill), got %s", spanNames(spans))
	}
	cached, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatalf("after the read reached PG, Redis should hold %s filled back in: %v", key, err)
	}
	var inRedis user
	if err := json.Unmarshal(cached, &inRedis); err != nil || inRedis.ID != u.ID || inRedis.Name != "tier" {
		t.Errorf("what was filled into Redis should be this user, got %s (%v)", cached, err)
	}
	// service/application.yml: Service.UserTTL: 60s
	if ttl := rdb.TTL(ctx, key).Val(); ttl <= 0 || ttl > 60*time.Second {
		t.Errorf("the key filled back in should carry UserTTL's (60s) expiry, got TTL=%v", ttl)
	}

	// The second read: the local cache hits and Redis is not even asked
	got, r, localElapsed := getUser(t, p, u.ID)
	if got.Source != "local" || got.Name != "tier" {
		t.Fatalf("the second read should hit the local cache (source=local), got %v", r)
	}
	serverSpan(t, p, traceIDOf(t, r))
	if spans := traceSpans(t, p, traceIDOf(t, r)); len(spans) != 1 {
		t.Errorf("on a local cache hit the trace should hold the server span alone, got %s", spanNames(spans))
	}

	// The Redis level: for a user never read before (and so absent from the local cache), put a value into Redis that differs from PG's
	v := createUser(t, p, "pg-name", "pg@example.com")
	marker := "from-redis-" + harness.NewID()
	fake, _ := json.Marshal(user{ID: v.ID, Name: marker, Email: "redis@example.com"})
	if err := rdb.Set(ctx, keyOf(v.ID), fake, time.Minute).Err(); err != nil {
		t.Fatalf("seed redis: %v", err)
	}
	got, r, redisElapsed := getUser(t, p, v.ID)
	if got.Source != "redis" || got.Name != marker {
		t.Fatalf("absent locally and present in Redis: the value from Redis should come back (source=redis, name=%s), got %v", marker, r)
	}
	serverSpan(t, p, traceIDOf(t, r))
	spans = traceSpans(t, p, traceIDOf(t, r))
	if got := clientSpanNames(spans); got != "get" {
		t.Errorf("on a Redis hit the trace should hold one Redis get and no SQL, got %s", spanNames(spans))
	}
	// A Redis hit fills the local cache back in: the next read is local and still holds Redis's value
	got, r, _ = getUser(t, p, v.ID)
	if got.Source != "local" || got.Name != marker {
		t.Errorf("a Redis hit should fill the local cache back in (source=local, name=%s), got %v", marker, r)
	}

	// Writing: both levels are dropped after a PUT, so the next read goes back to PG and reads the new value
	if r := p.Do(t, http.MethodPut, fmt.Sprintf("/users/%d", u.ID), map[string]string{"name": "tier2", "email": "tier@example.com"}); r.Status != http.StatusOK {
		t.Fatalf("PUT: %v", r)
	}
	if n := rdb.Exists(ctx, key).Val(); n != 0 {
		t.Errorf("%s should be deleted from Redis after the PUT", key)
	}
	got, r, _ = getUser(t, p, u.ID)
	if got.Source != "db" || got.Name != "tier2" {
		t.Errorf("after the PUT both the local cache and Redis should be gone and the next read should go back to PG (source=db, name=tier2), got %v", r)
	}

	// The metrics add up to the reads above: 2 db, 2 local and 1 redis
	m := waitMetrics(t, p, "e2e_user_reads_total reaching 5", func(m harness.Metrics) bool {
		return m.Sum("e2e_user_reads_total") == 5
	})
	for src, want := range map[string]float64{"db": 2, "local": 2, "redis": 1} {
		if got := m.Sum("e2e_user_reads_total", "source", src); got != want {
			t.Errorf("e2e_user_reads_total{source=%q} should be %v, got %v", src, want, got)
		}
	}

	t.Logf("numbers: one three-level read (the client round trip included) db=%v redis=%v local=%v", dbElapsed, redisElapsed, localElapsed)
}
