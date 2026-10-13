package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 冒烟：harness 的整条链路是通的——构建、起进程、连上真的 PG / Redis、
// 收请求、收到 SIGTERM 之后干净退出
func TestSmoke_ServiceStartsAndStops(t *testing.T) {
	harness.Require(t)
	p := harness.Start(t, harness.Options{})

	if r := p.Get(t, "/ping"); r.Status != http.StatusOK || string(r.Body) != "pong" {
		t.Fatalf("GET /ping：%v", r)
	}

	created := p.PostJSON(t, "/users", map[string]string{"name": "alice", "email": "alice@example.com"})
	if created.Status != http.StatusCreated {
		t.Fatalf("POST /users：%v", created)
	}
	var u struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
		Source string `json:"source"`
	}
	created.JSON(t, &u)
	if u.ID <= 0 {
		t.Fatalf("POST /users 没回 id：%v", created)
	}

	got := p.Get(t, fmt.Sprintf("/users/%d", u.ID))
	if got.Status != http.StatusOK {
		t.Fatalf("GET /users/%d：%v", u.ID, got)
	}
	got.JSON(t, &u)
	if u.Name != "alice" || u.Email != "alice@example.com" {
		t.Errorf("读回来的用户不对：%v", got)
	}
	// 刚建的用户哪级缓存里都没有，第一次读必然落到 PG
	if u.Source != "db" {
		t.Errorf("第一次读 source=%q，want db", u.Source)
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("SIGTERM 之后没有以 0 退出：%v\n%s", exit, p.Output())
	}
	t.Logf("退出：%v", exit)
}
