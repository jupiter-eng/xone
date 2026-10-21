package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// A panicking handler: xgin's Recover catches it, records one Error log line (with the stack and
// trace_id), answers 500, and the process serves on. xgin/middleware/middleware.go: "it catches a
// panic and turns it into one error log line and one 500"
func TestFunctional_PanicReturns500AndProcessSurvives(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Spans: true}) // no downstream configured: /proxy's 503 is seen along the way

	const n = 5
	var tids []string
	var worst time.Duration
	for range n {
		start := time.Now()
		r := p.Get(t, "/boom")
		worst = max(worst, time.Since(start))
		if r.Status != http.StatusInternalServerError {
			t.Fatalf("/boom should answer 500, got %v", r)
		}
		tids = append(tids, traceIDOf(t, r))
	}
	if p.Exited() {
		t.Fatalf("the process should not exit after a panic: %v\n%s", p.Exited(), p.Output())
	}
	if r := p.Get(t, "/ping"); r.Status != http.StatusOK {
		t.Fatalf("the service should answer as usual after a panic, and /ping gave %v", r)
	}

	for _, tid := range tids {
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "panic while handling request" && l.Str("trace_id") == tid
		})
		if l.Level() != "ERROR" || l.Str("error") != "deliberate panic from /boom" || l.Str("path") != "/boom" || l.Str("method") != "GET" {
			t.Errorf("the panic's error log should be ERROR carrying error, path and method, got %s", l.Line)
		}
		if !strings.Contains(l.Str("stack"), "main.boom") {
			t.Errorf("the panic's error log should carry the stack (holding main.boom), got stack=%.200q", l.Str("stack"))
		}
		if al := accessLog(t, p, tid); al.Str("status") != "500" {
			t.Errorf("/boom's access log should record 500, got %s", al.Line)
		}
		if s := serverSpan(t, p, tid); s.StatusCode != "Error" {
			t.Errorf("/boom's server span should be marked Error, got %s", s.StatusCode)
		}
	}
	m := waitMetrics(t, p, "all 5 /boom requests counted as 500", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/boom", "status", "500") == n
	})
	if got := m.Sum("e2e_log_errors_total", "level", "ERROR"); got != n {
		t.Errorf("5 panics make one Error log line each, so e2e_log_errors_total should be 5, got %v", got)
	}

	if r := p.Get(t, "/proxy"); r.Status != http.StatusServiceUnavailable || r.Map(t)["error"] != "downstream is not configured" {
		t.Errorf("with no downstream configured /proxy promises 503 downstream is not configured, got %v", r)
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("a process that has panicked should exit with 0 on SIGTERM, got %v", exit)
	}
	t.Logf("numbers: the slowest of the %d /boom requests answered 500 after %v; the exit %v", n, worst, exit)
}

// xflow: when a strong dependency fails, the steps already done are rolled back in reverse and the
// failing step is not rolled back itself.
// Every step's side effect and rollback land where they can be found (service/orders.go):
//
//	1 create   inserts a row into PG (created)   rollback: DELETE
//	2 charge   Redis SET of the charged key      rollback: DEL
//	3 confirm  sets it to confirmed in PG        rollback: back to created
//
// "step 1 really ran and really was rolled back" cannot be proven by PG holding no such row in the
// end (it may never have been inserted), so the SQL spans of that trace are checked as well: first
// an INSERT and then a DELETE
func TestFunctional_XflowStrongDepFailureRollsBackDoneStepsInReverse(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Spans: true})
	db := harness.DB(t)
	rdb := harness.Redis(t)
	u := createUser(t, p, "buyer", "buyer@example.com")

	type result struct {
		ID             string   `json:"id"`
		Success        bool     `json:"success"`
		Rolled         bool     `json:"rolled"`
		Error          string   `json:"error"`
		RollbackErrors []string `json:"rollback_errors"`
	}
	order := func(t *testing.T, extra map[string]any) (result, []harness.Span, time.Duration) {
		t.Helper()
		body := map[string]any{"id": "o-" + harness.NewID(), "user_id": u.ID, "amount": 99}
		for k, v := range extra {
			body[k] = v
		}
		start := time.Now()
		r := p.PostJSON(t, "/orders", body)
		elapsed := time.Since(start)
		var res result
		r.JSON(t, &res)
		want := http.StatusCreated
		if !res.Success {
			want = http.StatusInternalServerError
		}
		if r.Status != want || res.ID != body["id"] {
			t.Fatalf("POST /orders promises 201 on success and 500 on failure, with the same id in the body, got %v", r)
		}
		tid := traceIDOf(t, r)
		serverSpan(t, p, tid)
		return res, traceSpans(t, p, tid), elapsed
	}
	charged := func(id string) bool {
		return rdb.Exists(context.Background(), p.KeyPrefix+"order:"+id+":charged").Val() == 1
	}
	// sqlOps are the verbs of the SQL on this trace, in execution order: INSERT, UPDATE, DELETE
	sqlOps := func(spans []harness.Span) string {
		var ops []string
		for _, s := range spans {
			if strings.HasPrefix(s.Name, "gorm.") {
				ops = append(ops, strings.Fields(s.Str("db.query.text"))[0])
			}
		}
		return strings.Join(ops, ",")
	}
	var timings []string

	t.Run("all three steps succeed: no rollback", func(t *testing.T) {
		res, spans, el := order(t, nil)
		timings = append(timings, fmt.Sprintf("success %v", el))
		if !res.Success || res.Rolled {
			t.Errorf("it should be success=true rolled=false, got %+v", res)
		}
		if st := orderStatus(t, db, p, res.ID); st != "confirmed" || !charged(res.ID) {
			t.Errorf("with all three steps done the order should be confirmed and Redis should hold the charged key, got status=%q charged=%v", st, charged(res.ID))
		}
		if got := sqlOps(spans); got != "INSERT,UPDATE" {
			t.Errorf("the SQL should be INSERT then UPDATE, got %s", got)
		}
	})

	t.Run("step 2 fails: step 1 is rolled back", func(t *testing.T) {
		res, spans, el := order(t, map[string]any{"fail_at": 2})
		timings = append(timings, fmt.Sprintf("step 2 failing %v", el))
		if res.Success || !res.Rolled || !strings.Contains(res.Error, `step "charge"`) || !strings.Contains(res.Error, "injected failure at step 2") {
			t.Errorf("it should be success=false rolled=true with error naming the charge step, got %+v", res)
		}
		if len(res.RollbackErrors) != 0 {
			t.Errorf("every rollback should succeed, got rollback_errors=%v", res.RollbackErrors)
		}
		if st := orderStatus(t, db, p, res.ID); st != "" {
			t.Errorf("the order step 1 inserted should have been rolled back and deleted, and PG still holds status=%q", st)
		}
		if charged(res.ID) {
			t.Error("step 2 fails before its side effect, so Redis should hold no charged key")
		}
		// Step 1 really was inserted and really was deleted; step 2 did not roll itself back (no del on Redis)
		if got := sqlOps(spans); got != "INSERT,DELETE" {
			t.Errorf("the SQL should be INSERT (step 1) then DELETE (rolling step 1 back), got %s", got)
		}
		if n := len(spansNamed(spans, "del")); n != 0 {
			t.Errorf("the failing step is not rolled back: the trace should hold no Redis del, got %d", n)
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "xflow step process failed" && l.Str("step") == "charge" && l.Str("trace_id") == spans[0].TraceID
		})
		if l.Str("flow") != "create_order" || l.Level() != "WARN" {
			t.Errorf("the failing step should record one WARN (flow=create_order), got %s", l.Line)
		}
	})

	t.Run("step 3 fails: steps 2 and 1 are rolled back in reverse", func(t *testing.T) {
		res, spans, el := order(t, map[string]any{"fail_at": 3})
		timings = append(timings, fmt.Sprintf("step 3 failing %v", el))
		if res.Success || !res.Rolled || len(res.RollbackErrors) != 0 {
			t.Errorf("it should be success=false rolled=true with no rollback error, got %+v", res)
		}
		if st := orderStatus(t, db, p, res.ID); st != "" || charged(res.ID) {
			t.Errorf("steps 1 and 2 should both be rolled back: the order deleted and the charged key deleted, got status=%q charged=%v", st, charged(res.ID))
		}
		// In reverse: DEL first (rolling step 2 back) and then DELETE (rolling step 1 back)
		if got := clientSpanNames(spans); got != "gorm.raw,set,del,gorm.raw" {
			t.Errorf("it should be INSERT, SET, then DEL and DELETE on the rollback, got %s", spanNames(spans))
		}
		if got := sqlOps(spans); got != "INSERT,DELETE" {
			t.Errorf("the SQL should be INSERT then DELETE, got %s", got)
		}
	})

	t.Run("step 2 panics: it rolls back as it does for a returned error", func(t *testing.T) {
		res, _, _ := order(t, map[string]any{"panic_at": 2})
		if res.Success || !res.Rolled || !strings.Contains(res.Error, "panicked: injected panic at step 2") {
			t.Errorf("a panic should count as that step failing and roll back, got %+v", res)
		}
		if st := orderStatus(t, db, p, res.ID); st != "" {
			t.Errorf("step 1 should be rolled back, and PG still holds status=%q", st)
		}
		if p.Exited() {
			t.Fatal("a panicking step should not make the process exit")
		}
	})

	t.Run("step 1 fails: there is nothing to roll back", func(t *testing.T) {
		res, spans, _ := order(t, map[string]any{"fail_at": 1})
		if res.Success || res.Rolled {
			t.Errorf("with step 1 failing no step was done, so it should be rolled=false, got %+v", res)
		}
		if got := sqlOps(spans); got != "" {
			t.Errorf("there should be no SQL at all, got %s", got)
		}
	})

	t.Run("the rollback itself fails: it goes into rollback_errors, the resource stays and an Error is logged", func(t *testing.T) {
		res, _, _ := order(t, map[string]any{"fail_at": 3, "rollback_fail_at": 1})
		if res.Success || !res.Rolled || len(res.RollbackErrors) != 1 || !strings.Contains(res.RollbackErrors[0], `step "create"`) {
			t.Errorf("step 1's failing rollback should go into rollback_errors (naming create), got %+v", res)
		}
		// Step 2 was rolled back all the same; step 1's rollback did not succeed, so that row is still there
		if st := orderStatus(t, db, p, res.ID); st != "created" || charged(res.ID) {
			t.Errorf("the order should stay at created (step 1 was not compensated) with the charged key deleted (step 2 was), got status=%q charged=%v", st, charged(res.ID))
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "xflow rollback did not complete, resources may be left dangling"
		})
		if l.Level() != "ERROR" || l.Str("uncompensated_steps") != "1" {
			t.Errorf("an incomplete rollback should log one ERROR (uncompensated_steps=1), got %s", l.Line)
		}
	})

	m := waitMetrics(t, p, "the order count reaching 6", func(m harness.Metrics) bool { return m.Sum("e2e_orders_total") == 6 })
	if s, f := m.Sum("e2e_orders_total", "result", "success"), m.Sum("e2e_orders_total", "result", "failed"); s != 1 || f != 5 {
		t.Errorf("e2e_orders_total should be success=1 failed=5, got %v / %v", s, f)
	}
	t.Logf("numbers: POST /orders took %s", strings.Join(timings, ", "))
}
