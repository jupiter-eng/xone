package e2e

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 每个接口的返回：状态码、body 的形状、副作用真的落进了 PG / Redis。
// 接口约定写在 e2e/service/main.go 开头和各 handler 的注释里
func TestFunctional_EveryEndpointReturnsAsSpecified(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	// 桩前面插一个代理：Cut 之后下游连不上，用来看 /proxy 的 502
	down := harness.NewProxy(t, stub.Addr())
	p := harness.Start(t, harness.Options{Downstream: "http://" + down.Addr()})
	db := harness.DB(t)
	rdb := harness.Redis(t)
	ctx := context.Background()

	t.Run("GET /ping 返回 200 pong", func(t *testing.T) {
		r := p.Get(t, "/ping")
		if r.Status != http.StatusOK || string(r.Body) != "pong" {
			t.Fatalf("约定 200 pong，实际 %v", r)
		}
	})

	var alice user
	t.Run("POST /users 写进 PG 并返回 201", func(t *testing.T) {
		alice = createUser(t, p, "alice", "alice@example.com")
		if alice.Name != "alice" || alice.Email != "alice@example.com" {
			t.Errorf("约定返回 {id, name, email}，实际 %+v", alice)
		}
		var name, email string
		err := db.QueryRowContext(ctx, `SELECT name, email FROM `+p.Table+` WHERE id = $1`, alice.ID).Scan(&name, &email)
		if err != nil || name != "alice" || email != "alice@example.com" {
			t.Errorf("PG 里应有 id=%d 的 alice，实际 name=%q email=%q err=%v", alice.ID, name, email, err)
		}

		for _, bad := range []string{`{"email":"no-name@example.com"}`, `{"name":`} {
			r := p.Do(t, http.MethodPost, "/users", bad, "Content-Type", "application/json")
			if r.Status != http.StatusBadRequest {
				t.Errorf("POST /users %s：约定 400，实际 %v", bad, r)
			}
		}
	})

	t.Run("GET /users/:id 三种结果", func(t *testing.T) {
		u, r, _ := getUser(t, p, alice.ID)
		if u != (user{ID: alice.ID, Name: "alice", Email: "alice@example.com", Source: "db"}) {
			t.Errorf("约定返回用户和 source（刚建的用户第一次读是 db），实际 %v", r)
		}
		// ?cache=off 即使缓存里有也直接读 PG
		off := p.Get(t, fmt.Sprintf("/users/%d?cache=off", alice.ID))
		var o user
		off.JSON(t, &o)
		if off.Status != http.StatusOK || o.Source != "db" {
			t.Errorf("约定 ?cache=off 直接读 PG（source=db），实际 %v", off)
		}

		if r := p.Get(t, "/users/999999999"); r.Status != http.StatusNotFound || r.Map(t)["error"] != "user not found" {
			t.Errorf("没有这个用户：约定 404 user not found，实际 %v", r)
		}
		for _, bad := range []string{"abc", "0", "-3"} {
			if r := p.Get(t, "/users/"+bad); r.Status != http.StatusBadRequest {
				t.Errorf("GET /users/%s：约定 400，实际 %v", bad, r)
			}
		}
	})

	t.Run("PUT /users/:id 改 PG 并让缓存失效", func(t *testing.T) {
		// 先读一次，让两级缓存里都有旧值
		getUser(t, p, alice.ID)
		key := p.KeyPrefix + "user:" + strconv.FormatInt(alice.ID, 10)
		if n := rdb.Exists(ctx, key).Val(); n != 1 {
			t.Fatalf("读过之后 Redis 里应有 %s，实际 EXISTS=%d", key, n)
		}

		r := p.Do(t, http.MethodPut, fmt.Sprintf("/users/%d", alice.ID), map[string]string{"name": "alice2", "email": "a2@example.com"})
		var u user
		r.JSON(t, &u)
		if r.Status != http.StatusOK || u.Name != "alice2" || u.Email != "a2@example.com" {
			t.Fatalf("约定 200 返回改后的用户，实际 %v", r)
		}
		if n := rdb.Exists(ctx, key).Val(); n != 0 {
			t.Errorf("约定 PUT 之后删掉缓存，实际 Redis 里 %s 还在", key)
		}
		got, gr, _ := getUser(t, p, alice.ID)
		if got.Name != "alice2" || got.Source != "db" {
			t.Errorf("改完之后读到的应是 PG 里的新值（source=db），实际 %v", gr)
		}

		if r := p.Do(t, http.MethodPut, "/users/999999999", map[string]string{"name": "x"}); r.Status != http.StatusNotFound {
			t.Errorf("改一个不存在的用户：约定 404，实际 %v", r)
		}
		if r := p.Do(t, http.MethodPut, fmt.Sprintf("/users/%d", alice.ID), map[string]string{"email": "x"}); r.Status != http.StatusBadRequest {
			t.Errorf("缺 name：约定 400，实际 %v", r)
		}
	})

	t.Run("POST /login 返回 session_token", func(t *testing.T) {
		r := p.PostJSON(t, "/login", map[string]string{"username": "bob", "password": "pw", "token": "tk"})
		m := r.Map(t)
		tok, _ := m["session_token"].(string)
		if r.Status != http.StatusOK || m["ok"] != true || m["username"] != "bob" || !regexp.MustCompile(`^sess-[0-9a-f]{32}$`).MatchString(tok) {
			t.Errorf("约定 200 {ok, username, session_token}，实际 %v", r)
		}
		if r := p.PostJSON(t, "/login", map[string]string{"password": "pw"}); r.Status != http.StatusBadRequest {
			t.Errorf("缺 username：约定 400，实际 %v", r)
		}
	})

	t.Run("GET /proxy 原样转回下游的响应", func(t *testing.T) {
		r := p.Get(t, "/proxy?token=abc")
		if r.Status != http.StatusOK || string(r.Body) != `{"ok":true}` || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			t.Errorf("约定原样转回下游的 200 {\"ok\":true}，实际 %v（Content-Type %q）", r, r.Header.Get("Content-Type"))
		}
		last := stub.Last(t)
		if last.Method != http.MethodGet || last.Path != "/echo" || last.Query.Get("token") != "abc" {
			t.Errorf("约定调下游 GET /echo?token=abc，下游收到 %s %s?%s", last.Method, last.Path, last.RawQuery)
		}

		p.Get(t, "/proxy")
		if got := stub.Last(t).Query.Get("token"); got != "e2e-downstream-token" {
			t.Errorf("没给 token 时约定用固定值 e2e-downstream-token，下游收到 %q", got)
		}

		stub.SetStatus(http.StatusTeapot)
		stub.SetBody("text/plain", "short and stout")
		r = p.Get(t, "/proxy")
		stub.SetStatus(http.StatusOK)
		stub.SetBody("application/json", `{"ok":true}`)
		if r.Status != http.StatusTeapot || string(r.Body) != "short and stout" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("约定原样转回下游的状态码和 body（418 text/plain），实际 %v（Content-Type %q）", r, r.Header.Get("Content-Type"))
		}

		down.Cut()
		start := time.Now()
		r = p.Get(t, "/proxy")
		elapsed := time.Since(start)
		down.Restore()
		if r.Status != http.StatusBadGateway || r.Map(t)["error"] == nil {
			t.Errorf("下游连不上：约定 502 带 error，实际 %v", r)
		}
		t.Logf("数字：下游 connection refused 时 /proxy 用了 %v 返回 502", elapsed)

		if r := p.Get(t, "/proxy"); r.Status != http.StatusOK {
			t.Errorf("下游恢复之后应回到 200，实际 %v", r)
		}
	})

	t.Run("POST /orders 三步都做完", func(t *testing.T) {
		r := p.PostJSON(t, "/orders", map[string]any{"user_id": alice.ID, "amount": 42})
		m := r.Map(t)
		id, _ := m["id"].(string)
		if r.Status != http.StatusCreated || m["success"] != true || m["rolled"] != false || !regexp.MustCompile(`^o-[0-9a-f]{16}$`).MatchString(id) {
			t.Fatalf("约定 201 {id, success:true, rolled:false}，没给 id 时生成 o-<16 位十六进制>，实际 %v", r)
		}
		if st := orderStatus(t, db, p, id); st != "confirmed" {
			t.Errorf("三步做完 PG 里订单应是 confirmed，实际 %q", st)
		}
		if v, err := rdb.Get(ctx, p.KeyPrefix+"order:"+id+":charged").Result(); err != nil || v != "42" {
			t.Errorf("第 2 步应在 Redis 里记下扣款 42，实际 %q err=%v", v, err)
		}

		r = p.PostJSON(t, "/orders", map[string]any{"id": "o-given-" + harness.NewID(), "user_id": alice.ID, "amount": 1})
		if m := r.Map(t); r.Status != http.StatusCreated || !strings.HasPrefix(fmt.Sprint(m["id"]), "o-given-") {
			t.Errorf("给了 id 就用给的，实际 %v", r)
		}
		if r := p.Do(t, http.MethodPost, "/orders", "not json", "Content-Type", "application/json"); r.Status != http.StatusBadRequest {
			t.Errorf("body 不是 JSON：约定 400，实际 %v", r)
		}
	})

	t.Run("GET /slow 与 /stuck 睡够了再返回", func(t *testing.T) {
		start := time.Now()
		r := p.Get(t, "/slow?ms=80")
		slow := time.Since(start)
		if r.Status != http.StatusOK || r.Map(t)["slept_ms"] != float64(80) || slow < 80*time.Millisecond {
			t.Errorf("约定 200 {slept_ms:80} 且至少等 80ms，实际 %v，用了 %v", r, slow)
		}
		start = time.Now()
		r = p.Get(t, "/stuck?ms=30&db=1")
		stuck := time.Since(start)
		if r.Status != http.StatusOK || r.Map(t)["slept_ms"] != float64(30) || stuck < 30*time.Millisecond {
			t.Errorf("约定 200 {slept_ms:30} 且至少等 30ms，实际 %v，用了 %v", r, stuck)
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "stuck request finished" })
		if l.Str("db_error") != "none" {
			t.Errorf("/stuck?db=1 睡完查库应成功（db_error=none），实际 %q", l.Str("db_error"))
		}
		t.Logf("数字：/slow?ms=80 用了 %v，/stuck?ms=30&db=1 用了 %v", slow, stuck)
	})

	t.Run("未注册的路由 404，方法不对 405", func(t *testing.T) {
		if r := p.Get(t, "/no/such/route"); r.Status != http.StatusNotFound {
			t.Errorf("未注册的路由应 404，实际 %v", r)
		}
		// xgin.go：HandleMethodNotAllowed = true，「不开的话，方法不对会返回 404 而不是 405」
		if r := p.Do(t, http.MethodDelete, "/users/1", nil); r.Status != http.StatusMethodNotAllowed {
			t.Errorf("路由在、方法不对应 405，实际 %v", r)
		}
	})
}

// orderStatus 订单在 PG 里的状态，没有这一行时返回空串
func orderStatus(t *testing.T, db *sql.DB, p *harness.Process, id string) string {
	t.Helper()
	var st string
	err := db.QueryRowContext(context.Background(), `SELECT status FROM `+p.Table+`_orders WHERE id = $1`, id).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("query order %s: %v", id, err)
	}
	return st
}
