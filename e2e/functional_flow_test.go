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

// handler panic：xgin 的 Recover 兜住，记一条 Error 日志（带栈和 trace_id），回 500，
// 进程照常服务。xgin/middleware/middleware.go：「兜住 panic，把它变成一条错误日志和一个 500」
func TestFunctional_PanicReturns500AndProcessSurvives(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	p := harness.Start(t, harness.Options{Spans: true}) // 不配下游：顺带看 /proxy 的 503

	const n = 5
	var tids []string
	var worst time.Duration
	for range n {
		start := time.Now()
		r := p.Get(t, "/boom")
		worst = max(worst, time.Since(start))
		if r.Status != http.StatusInternalServerError {
			t.Fatalf("/boom 应返回 500，实际 %v", r)
		}
		tids = append(tids, traceIDOf(t, r))
	}
	if p.Exited() {
		t.Fatalf("panic 之后进程不该退出：%v\n%s", p.Exited(), p.Output())
	}
	if r := p.Get(t, "/ping"); r.Status != http.StatusOK {
		t.Fatalf("panic 之后服务应照常响应，/ping 实际 %v", r)
	}

	for _, tid := range tids {
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "panic while handling request" && l.Str("trace_id") == tid
		})
		if l.Level() != "ERROR" || l.Str("error") != "deliberate panic from /boom" || l.Str("path") != "/boom" || l.Str("method") != "GET" {
			t.Errorf("panic 的错误日志应是 ERROR，带 error、path、method，实际 %s", l.Line)
		}
		if !strings.Contains(l.Str("stack"), "main.boom") {
			t.Errorf("panic 的错误日志应带栈（里面有 main.boom），实际 stack=%.200q", l.Str("stack"))
		}
		if al := accessLog(t, p, tid); al.Str("status") != "500" {
			t.Errorf("/boom 的访问日志应记 500，实际 %s", al.Line)
		}
		if s := serverSpan(t, p, tid); s.StatusCode != "Error" {
			t.Errorf("/boom 的服务端 Span 应标成 Error，实际 %s", s.StatusCode)
		}
	}
	m := waitMetrics(t, p, "5 次 /boom 都计进了 500", func(m harness.Metrics) bool {
		return m.Sum("e2e_http_requests_total", "route", "/boom", "status", "500") == n
	})
	if got := m.Sum("e2e_log_errors_total", "level", "ERROR"); got != n {
		t.Errorf("5 次 panic 各一条 Error 日志，e2e_log_errors_total 应是 5，实际 %v", got)
	}

	if r := p.Get(t, "/proxy"); r.Status != http.StatusServiceUnavailable || r.Map(t)["error"] != "downstream is not configured" {
		t.Errorf("没配下游时 /proxy 约定 503 downstream is not configured，实际 %v", r)
	}

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 || exit.Signal != nil {
		t.Errorf("panic 过的进程收到 SIGTERM 之后应以 0 退出，实际 %v", exit)
	}
	t.Logf("数字：%d 次 /boom 最慢一次 %v 返回 500；退出 %v", n, worst, exit)
}

// xflow：强依赖失败时，已经做完的步骤按逆序回滚，失败的那一步自己不回滚。
// 每一步的副作用和回滚都落在查得到的地方（service/orders.go）：
//
//	1 create   PG 插一行（created）      回滚：DELETE
//	2 charge   Redis SET charged 键      回滚：DEL
//	3 confirm  PG 改成 confirmed          回滚：改回 created
//
// 「第 1 步真的做过、又真的被回滚了」光看最终 PG 里没有这一行证明不了（也可能根本没插），
// 所以另看这条链路上的 SQL Span：先有 INSERT，后有 DELETE
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
			t.Fatalf("POST /orders 约定成功 201、失败 500，body 带同一个 id，实际 %v", r)
		}
		tid := traceIDOf(t, r)
		serverSpan(t, p, tid)
		return res, traceSpans(t, p, tid), elapsed
	}
	charged := func(id string) bool {
		return rdb.Exists(context.Background(), p.KeyPrefix+"order:"+id+":charged").Val() == 1
	}
	// sqlOps 这条链路上 SQL 的动词，按执行顺序：INSERT、UPDATE、DELETE
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

	t.Run("三步都成功：不回滚", func(t *testing.T) {
		res, spans, el := order(t, nil)
		timings = append(timings, fmt.Sprintf("成功 %v", el))
		if !res.Success || res.Rolled {
			t.Errorf("应 success=true rolled=false，实际 %+v", res)
		}
		if st := orderStatus(t, db, p, res.ID); st != "confirmed" || !charged(res.ID) {
			t.Errorf("三步都做完：订单应是 confirmed、Redis 里有 charged 键，实际 status=%q charged=%v", st, charged(res.ID))
		}
		if got := sqlOps(spans); got != "INSERT,UPDATE" {
			t.Errorf("SQL 应依次是 INSERT、UPDATE，实际 %s", got)
		}
	})

	t.Run("第 2 步失败：第 1 步被回滚", func(t *testing.T) {
		res, spans, el := order(t, map[string]any{"fail_at": 2})
		timings = append(timings, fmt.Sprintf("第2步失败 %v", el))
		if res.Success || !res.Rolled || !strings.Contains(res.Error, `step "charge"`) || !strings.Contains(res.Error, "injected failure at step 2") {
			t.Errorf("应 success=false rolled=true，error 点名 charge 这一步，实际 %+v", res)
		}
		if len(res.RollbackErrors) != 0 {
			t.Errorf("回滚都该成功，实际 rollback_errors=%v", res.RollbackErrors)
		}
		if st := orderStatus(t, db, p, res.ID); st != "" {
			t.Errorf("第 1 步插的订单应已被回滚删掉，PG 里实际还有 status=%q", st)
		}
		if charged(res.ID) {
			t.Error("第 2 步失败在副作用之前，Redis 里不该有 charged 键")
		}
		// 第 1 步真的插过、又真的删了；第 2 步自己没有回滚（Redis 上没有 del）
		if got := sqlOps(spans); got != "INSERT,DELETE" {
			t.Errorf("SQL 应依次是 INSERT（第 1 步）、DELETE（回滚第 1 步），实际 %s", got)
		}
		if n := len(spansNamed(spans, "del")); n != 0 {
			t.Errorf("失败的那一步不回滚：链路上不该有 Redis del，实际 %d 个", n)
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "xflow step process failed" && l.Str("step") == "charge" && l.Str("trace_id") == spans[0].TraceID
		})
		if l.Str("flow") != "create_order" || l.Level() != "WARN" {
			t.Errorf("失败的那一步应记一条 WARN（flow=create_order），实际 %s", l.Line)
		}
	})

	t.Run("第 3 步失败：第 2、1 步按逆序回滚", func(t *testing.T) {
		res, spans, el := order(t, map[string]any{"fail_at": 3})
		timings = append(timings, fmt.Sprintf("第3步失败 %v", el))
		if res.Success || !res.Rolled || len(res.RollbackErrors) != 0 {
			t.Errorf("应 success=false rolled=true 且回滚无错，实际 %+v", res)
		}
		if st := orderStatus(t, db, p, res.ID); st != "" || charged(res.ID) {
			t.Errorf("第 1、2 步都应被回滚：订单删掉、charged 键删掉，实际 status=%q charged=%v", st, charged(res.ID))
		}
		// 逆序：先 DEL（回滚第 2 步），再 DELETE（回滚第 1 步）
		if got := clientSpanNames(spans); got != "gorm.raw,set,del,gorm.raw" {
			t.Errorf("应依次是 INSERT、SET、回滚时 DEL、DELETE，实际 %s", spanNames(spans))
		}
		if got := sqlOps(spans); got != "INSERT,DELETE" {
			t.Errorf("SQL 应依次是 INSERT、DELETE，实际 %s", got)
		}
	})

	t.Run("第 2 步 panic：和返回错误一样回滚", func(t *testing.T) {
		res, _, _ := order(t, map[string]any{"panic_at": 2})
		if res.Success || !res.Rolled || !strings.Contains(res.Error, "panicked: injected panic at step 2") {
			t.Errorf("panic 应被当成这一步失败并回滚，实际 %+v", res)
		}
		if st := orderStatus(t, db, p, res.ID); st != "" {
			t.Errorf("第 1 步应被回滚，PG 里实际还有 status=%q", st)
		}
		if p.Exited() {
			t.Fatal("步骤 panic 不该让进程退出")
		}
	})

	t.Run("第 1 步就失败：没有可回滚的", func(t *testing.T) {
		res, spans, _ := order(t, map[string]any{"fail_at": 1})
		if res.Success || res.Rolled {
			t.Errorf("第 1 步失败时没有做完的步骤，应 rolled=false，实际 %+v", res)
		}
		if got := sqlOps(spans); got != "" {
			t.Errorf("一条 SQL 都不该有，实际 %s", got)
		}
	})

	t.Run("回滚本身失败：记进 rollback_errors，资源留着，打 Error", func(t *testing.T) {
		res, _, _ := order(t, map[string]any{"fail_at": 3, "rollback_fail_at": 1})
		if res.Success || !res.Rolled || len(res.RollbackErrors) != 1 || !strings.Contains(res.RollbackErrors[0], `step "create"`) {
			t.Errorf("第 1 步的回滚失败应记进 rollback_errors（点名 create），实际 %+v", res)
		}
		// 第 2 步照样回滚了；第 1 步没回滚成，那一行还在
		if st := orderStatus(t, db, p, res.ID); st != "created" || charged(res.ID) {
			t.Errorf("应是订单留在 created（第 1 步没补偿成）、charged 键已删（第 2 步补偿了），实际 status=%q charged=%v", st, charged(res.ID))
		}
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool {
			return l.Msg() == "xflow rollback did not complete, resources may be left dangling"
		})
		if l.Level() != "ERROR" || l.Str("uncompensated_steps") != "1" {
			t.Errorf("回滚没做完应打一条 ERROR（uncompensated_steps=1），实际 %s", l.Line)
		}
	})

	m := waitMetrics(t, p, "订单计数到 6", func(m harness.Metrics) bool { return m.Sum("e2e_orders_total") == 6 })
	if s, f := m.Sum("e2e_orders_total", "result", "success"), m.Sum("e2e_orders_total", "result", "failed"); s != 1 || f != 5 {
		t.Errorf("e2e_orders_total 应是 success=1 failed=5，实际 %v / %v", s, f)
	}
	t.Logf("数字：POST /orders 耗时 %s", strings.Join(timings, "，"))
}
