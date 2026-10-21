package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// The smoke test: the harness's whole chain works -- it builds, starts a process, connects to a
// real PG / Redis, takes requests and exits cleanly on SIGTERM
func TestSmoke_ServiceStartsAndStops(t *testing.T) {
	harness.Require(t)
	p := harness.Start(t, harness.Options{})

	if r := p.Get(t, "/ping"); r.Status != http.StatusOK || string(r.Body) != "pong" {
		t.Fatalf("GET /ping: %v", r)
	}

	created := p.PostJSON(t, "/users", map[string]string{"name": "alice", "email": "alice@example.com"})
	if created.Status != http.StatusCreated {
		t.Fatalf("POST /users: %v", created)
	}
	var u struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Email  string `json:"email"`
		Source string `json:"source"`
	}
	created.JSON(t, &u)
	if u.ID <= 0 {
		t.Fatalf("POST /users answered no id: %v", created)
	}

	got := p.Get(t, fmt.Sprintf("/users/%d", u.ID))
	if got.Status != http.StatusOK {
		t.Fatalf("GET /users/%d: %v", u.ID, got)
	}
	got.JSON(t, &u)
	if u.Name != "alice" || u.Email != "alice@example.com" {
		t.Errorf("the user read back is wrong: %v", got)
	}
	// A user that was just created is in no cache level, so the first read has to reach PG
	if u.Source != "db" {
		t.Errorf("the first read has source=%q, want db", u.Source)
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Fatalf("it did not exit with 0 after the SIGTERM: %v\n%s", exit, p.Output())
	}
	t.Logf("exit: %v", exit)
}
