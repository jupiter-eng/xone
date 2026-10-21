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

// What every endpoint answers: the status code, the shape of the body, and the side effects really
// landing in PG / Redis.
// What the endpoints promise is written at the top of e2e/service/main.go and in each handler's
// comment
func TestFunctional_EveryEndpointReturnsAsSpecified(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	stub := harness.NewStub(t)
	// A proxy in front of the stub: after a Cut the downstream cannot be reached, which is how /proxy's 502 is seen
	down := harness.NewProxy(t, stub.Addr())
	p := harness.Start(t, harness.Options{Downstream: "http://" + down.Addr()})
	db := harness.DB(t)
	rdb := harness.Redis(t)
	ctx := context.Background()

	t.Run("GET /ping answers 200 pong", func(t *testing.T) {
		r := p.Get(t, "/ping")
		if r.Status != http.StatusOK || string(r.Body) != "pong" {
			t.Fatalf("it promises 200 pong, got %v", r)
		}
	})

	var alice user
	t.Run("POST /users writes PG and answers 201", func(t *testing.T) {
		alice = createUser(t, p, "alice", "alice@example.com")
		if alice.Name != "alice" || alice.Email != "alice@example.com" {
			t.Errorf("it promises {id, name, email} back, got %+v", alice)
		}
		var name, email string
		err := db.QueryRowContext(ctx, `SELECT name, email FROM `+p.Table+` WHERE id = $1`, alice.ID).Scan(&name, &email)
		if err != nil || name != "alice" || email != "alice@example.com" {
			t.Errorf("PG should hold the alice with id=%d, got name=%q email=%q err=%v", alice.ID, name, email, err)
		}

		for _, bad := range []string{`{"email":"no-name@example.com"}`, `{"name":`} {
			r := p.Do(t, http.MethodPost, "/users", bad, "Content-Type", "application/json")
			if r.Status != http.StatusBadRequest {
				t.Errorf("POST /users %s: it promises 400, got %v", bad, r)
			}
		}
	})

	t.Run("GET /users/:id has three outcomes", func(t *testing.T) {
		u, r, _ := getUser(t, p, alice.ID)
		if u != (user{ID: alice.ID, Name: "alice", Email: "alice@example.com", Source: "db"}) {
			t.Errorf("it promises the user and a source (db for the first read of a user just created), got %v", r)
		}
		// ?cache=off reads PG directly even when the cache holds it
		off := p.Get(t, fmt.Sprintf("/users/%d?cache=off", alice.ID))
		var o user
		off.JSON(t, &o)
		if off.Status != http.StatusOK || o.Source != "db" {
			t.Errorf("it promises that ?cache=off reads PG directly (source=db), got %v", off)
		}

		if r := p.Get(t, "/users/999999999"); r.Status != http.StatusNotFound || r.Map(t)["error"] != "user not found" {
			t.Errorf("no such user: it promises 404 user not found, got %v", r)
		}
		for _, bad := range []string{"abc", "0", "-3"} {
			if r := p.Get(t, "/users/"+bad); r.Status != http.StatusBadRequest {
				t.Errorf("GET /users/%s: it promises 400, got %v", bad, r)
			}
		}
	})

	t.Run("PUT /users/:id updates PG and invalidates the cache", func(t *testing.T) {
		// Read once first, so that both cache levels hold the old value
		getUser(t, p, alice.ID)
		key := p.KeyPrefix + "user:" + strconv.FormatInt(alice.ID, 10)
		if n := rdb.Exists(ctx, key).Val(); n != 1 {
			t.Fatalf("after the read Redis should hold %s, got EXISTS=%d", key, n)
		}

		r := p.Do(t, http.MethodPut, fmt.Sprintf("/users/%d", alice.ID), map[string]string{"name": "alice2", "email": "a2@example.com"})
		var u user
		r.JSON(t, &u)
		if r.Status != http.StatusOK || u.Name != "alice2" || u.Email != "a2@example.com" {
			t.Fatalf("it promises 200 with the updated user, got %v", r)
		}
		if n := rdb.Exists(ctx, key).Val(); n != 0 {
			t.Errorf("it promises that a PUT drops the cache, and Redis still holds %s", key)
		}
		got, gr, _ := getUser(t, p, alice.ID)
		if got.Name != "alice2" || got.Source != "db" {
			t.Errorf("after the update the read should give PG's new value (source=db), got %v", gr)
		}

		if r := p.Do(t, http.MethodPut, "/users/999999999", map[string]string{"name": "x"}); r.Status != http.StatusNotFound {
			t.Errorf("updating a user that does not exist: it promises 404, got %v", r)
		}
		if r := p.Do(t, http.MethodPut, fmt.Sprintf("/users/%d", alice.ID), map[string]string{"email": "x"}); r.Status != http.StatusBadRequest {
			t.Errorf("name missing: it promises 400, got %v", r)
		}
	})

	t.Run("POST /login answers a session_token", func(t *testing.T) {
		r := p.PostJSON(t, "/login", map[string]string{"username": "bob", "password": "pw", "token": "tk"})
		m := r.Map(t)
		tok, _ := m["session_token"].(string)
		if r.Status != http.StatusOK || m["ok"] != true || m["username"] != "bob" || !regexp.MustCompile(`^sess-[0-9a-f]{32}$`).MatchString(tok) {
			t.Errorf("it promises 200 {ok, username, session_token}, got %v", r)
		}
		if r := p.PostJSON(t, "/login", map[string]string{"password": "pw"}); r.Status != http.StatusBadRequest {
			t.Errorf("username missing: it promises 400, got %v", r)
		}
	})

	t.Run("GET /proxy passes the downstream's response back as it is", func(t *testing.T) {
		r := p.Get(t, "/proxy?token=abc")
		if r.Status != http.StatusOK || string(r.Body) != `{"ok":true}` || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			t.Errorf("it promises the downstream's 200 {\"ok\":true} passed back as it is, got %v (Content-Type %q)", r, r.Header.Get("Content-Type"))
		}
		last := stub.Last(t)
		if last.Method != http.MethodGet || last.Path != "/echo" || last.Query.Get("token") != "abc" {
			t.Errorf("it promises a downstream call of GET /echo?token=abc, and the downstream saw %s %s?%s", last.Method, last.Path, last.RawQuery)
		}

		p.Get(t, "/proxy")
		if got := stub.Last(t).Query.Get("token"); got != "e2e-downstream-token" {
			t.Errorf("with no token given it promises the fixed value e2e-downstream-token, and the downstream saw %q", got)
		}

		stub.SetStatus(http.StatusTeapot)
		stub.SetBody("text/plain", "short and stout")
		r = p.Get(t, "/proxy")
		stub.SetStatus(http.StatusOK)
		stub.SetBody("application/json", `{"ok":true}`)
		if r.Status != http.StatusTeapot || string(r.Body) != "short and stout" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("it promises the downstream's status code and body passed back as they are (418 text/plain), got %v (Content-Type %q)", r, r.Header.Get("Content-Type"))
		}

		down.Cut()
		start := time.Now()
		r = p.Get(t, "/proxy")
		elapsed := time.Since(start)
		down.Restore()
		if r.Status != http.StatusBadGateway || r.Map(t)["error"] == nil {
			t.Errorf("the downstream cannot be reached: it promises 502 with an error, got %v", r)
		}
		t.Logf("numbers: /proxy took %v to answer 502 while the downstream refused connections", elapsed)

		if r := p.Get(t, "/proxy"); r.Status != http.StatusOK {
			t.Errorf("it should be back to 200 once the downstream has recovered, got %v", r)
		}
	})

	t.Run("POST /orders runs all three steps", func(t *testing.T) {
		r := p.PostJSON(t, "/orders", map[string]any{"user_id": alice.ID, "amount": 42})
		m := r.Map(t)
		id, _ := m["id"].(string)
		if r.Status != http.StatusCreated || m["success"] != true || m["rolled"] != false || !regexp.MustCompile(`^o-[0-9a-f]{16}$`).MatchString(id) {
			t.Fatalf("it promises 201 {id, success:true, rolled:false} and an o-<16 hex digits> id when none is given, got %v", r)
		}
		if st := orderStatus(t, db, p, id); st != "confirmed" {
			t.Errorf("with all three steps done the order should be confirmed in PG, got %q", st)
		}
		if v, err := rdb.Get(ctx, p.KeyPrefix+"order:"+id+":charged").Result(); err != nil || v != "42" {
			t.Errorf("step 2 should record the charge of 42 in Redis, got %q err=%v", v, err)
		}

		r = p.PostJSON(t, "/orders", map[string]any{"id": "o-given-" + harness.NewID(), "user_id": alice.ID, "amount": 1})
		if m := r.Map(t); r.Status != http.StatusCreated || !strings.HasPrefix(fmt.Sprint(m["id"]), "o-given-") {
			t.Errorf("an id that is given is used, got %v", r)
		}
		if r := p.Do(t, http.MethodPost, "/orders", "not json", "Content-Type", "application/json"); r.Status != http.StatusBadRequest {
			t.Errorf("a body that is not JSON: it promises 400, got %v", r)
		}
	})

	t.Run("GET /slow and /stuck return once they have slept", func(t *testing.T) {
		start := time.Now()
		r := p.Get(t, "/slow?ms=80")
		slow := time.Since(start)
		if r.Status != http.StatusOK || r.Map(t)["slept_ms"] != float64(80) || slow < 80*time.Millisecond {
			t.Errorf("it promises 200 {slept_ms:80} and at least 80ms of waiting, got %v after %v", r, slow)
		}
		start = time.Now()
		r = p.Get(t, "/stuck?ms=30&db=1")
		stuck := time.Since(start)
		if r.Status != http.StatusOK || r.Map(t)["slept_ms"] != float64(30) || stuck < 30*time.Millisecond {
			t.Errorf("it promises 200 {slept_ms:30} and at least 30ms of waiting, got %v after %v", r, stuck)
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "stuck request finished" })
		if l.Str("db_error") != "none" {
			t.Errorf("/stuck?db=1 should reach the database after its sleep (db_error=none), got %q", l.Str("db_error"))
		}
		t.Logf("numbers: /slow?ms=80 took %v and /stuck?ms=30&db=1 took %v", slow, stuck)
	})

	t.Run("an unregistered route answers 404 and a wrong method 405", func(t *testing.T) {
		if r := p.Get(t, "/no/such/route"); r.Status != http.StatusNotFound {
			t.Errorf("an unregistered route should answer 404, got %v", r)
		}
		// xgin.go: HandleMethodNotAllowed = true, "without it a wrong method answers 404 rather than 405"
		if r := p.Do(t, http.MethodDelete, "/users/1", nil); r.Status != http.StatusMethodNotAllowed {
			t.Errorf("the route is there and the method is wrong, which should answer 405, got %v", r)
		}
	})
}

// orderStatus is the order's status in PG, or an empty string when there is no such row
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
