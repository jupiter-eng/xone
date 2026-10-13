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

// GET /users/:id 是「本地缓存（xcache）→ Redis（xredis）→ PG（xgorm）」三级读。
// 响应里的 source 是服务自己说的，所以每一级都另找证据：
//
//	db     这条链路上有 gorm.query 的 Span，读完之后 Redis 里有了这个 key
//	redis  Redis 里被直接塞了一份和 PG 不一样的值，读到的正是它；链路上只有 get、没有 SQL
//	local  链路上一个客户端 Span 都没有：既没问 Redis 也没问 PG
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
		t.Fatalf("刚建的用户缓存应是空的（POST 会删两级缓存），实际 Redis 里已有 %s", key)
	}

	// 第一次：两级缓存都没有，落到 PG，再回填 Redis 和本地
	got, r, dbElapsed := getUser(t, p, u.ID)
	if got.Source != "db" {
		t.Fatalf("第一次读 source 应是 db，实际 %v", r)
	}
	serverSpan(t, p, traceIDOf(t, r))
	spans := traceSpans(t, p, traceIDOf(t, r))
	if got := clientSpanNames(spans); got != "get,gorm.query,set" {
		t.Errorf("落到 PG 的那次读，链路上应依次是 Redis get（未命中）→ gorm.query → Redis set（回填），实际 %s", spanNames(spans))
	}
	cached, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatalf("读过 PG 之后 Redis 里应回填了 %s：%v", key, err)
	}
	var inRedis user
	if err := json.Unmarshal(cached, &inRedis); err != nil || inRedis.ID != u.ID || inRedis.Name != "tier" {
		t.Errorf("Redis 里回填的应是这个用户，实际 %s（%v）", cached, err)
	}
	// service/application.yml：Service.UserTTL: 60s
	if ttl := rdb.TTL(ctx, key).Val(); ttl <= 0 || ttl > 60*time.Second {
		t.Errorf("回填的 key 应带 UserTTL（60s）的过期时间，实际 TTL=%v", ttl)
	}

	// 第二次：本地缓存命中，连 Redis 都不问
	got, r, localElapsed := getUser(t, p, u.ID)
	if got.Source != "local" || got.Name != "tier" {
		t.Fatalf("第二次读应命中本地缓存（source=local），实际 %v", r)
	}
	serverSpan(t, p, traceIDOf(t, r))
	if spans := traceSpans(t, p, traceIDOf(t, r)); len(spans) != 1 {
		t.Errorf("本地缓存命中时链路上应只有服务端 Span，实际 %s", spanNames(spans))
	}

	// Redis 这一级：给一个从没读过的用户（本地缓存里没有）在 Redis 里塞一份和 PG 不一样的值
	v := createUser(t, p, "pg-name", "pg@example.com")
	marker := "from-redis-" + harness.NewID()
	fake, _ := json.Marshal(user{ID: v.ID, Name: marker, Email: "redis@example.com"})
	if err := rdb.Set(ctx, keyOf(v.ID), fake, time.Minute).Err(); err != nil {
		t.Fatalf("seed redis: %v", err)
	}
	got, r, redisElapsed := getUser(t, p, v.ID)
	if got.Source != "redis" || got.Name != marker {
		t.Fatalf("本地没有、Redis 有：应读到 Redis 里的值（source=redis，name=%s），实际 %v", marker, r)
	}
	serverSpan(t, p, traceIDOf(t, r))
	spans = traceSpans(t, p, traceIDOf(t, r))
	if got := clientSpanNames(spans); got != "get" {
		t.Errorf("Redis 命中时链路上应只有一次 Redis get、没有 SQL，实际 %s", spanNames(spans))
	}
	// Redis 命中之后回填本地：下一次是 local，读到的仍是 Redis 那份
	got, r, _ = getUser(t, p, v.ID)
	if got.Source != "local" || got.Name != marker {
		t.Errorf("Redis 命中之后应回填本地缓存（source=local，name=%s），实际 %v", marker, r)
	}

	// 写：PUT 之后两级都失效，下一次读回到 PG，读到新值
	if r := p.Do(t, http.MethodPut, fmt.Sprintf("/users/%d", u.ID), map[string]string{"name": "tier2", "email": "tier@example.com"}); r.Status != http.StatusOK {
		t.Fatalf("PUT：%v", r)
	}
	if n := rdb.Exists(ctx, key).Val(); n != 0 {
		t.Errorf("PUT 之后 Redis 里的 %s 应被删掉", key)
	}
	got, r, _ = getUser(t, p, u.ID)
	if got.Source != "db" || got.Name != "tier2" {
		t.Errorf("PUT 之后本地和 Redis 都该失效，下一次读回到 PG（source=db，name=tier2），实际 %v", r)
	}

	// 指标和上面的读次数对得上：db 2 次、local 2 次、redis 1 次
	m := waitMetrics(t, p, "e2e_user_reads_total 计到 5 次", func(m harness.Metrics) bool {
		return m.Sum("e2e_user_reads_total") == 5
	})
	for src, want := range map[string]float64{"db": 2, "local": 2, "redis": 1} {
		if got := m.Sum("e2e_user_reads_total", "source", src); got != want {
			t.Errorf("e2e_user_reads_total{source=%q} 应是 %v，实际 %v", src, want, got)
		}
	}

	t.Logf("数字：三级读单次耗时（含客户端往返）db=%v redis=%v local=%v", dbElapsed, redisElapsed, localElapsed)
}
