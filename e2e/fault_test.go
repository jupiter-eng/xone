package e2e

// The fault end-to-end tests: the TestFault_* family.
//
//	scripts/e2e.sh -run Fault
//
// Every fault is injected through the harness's TCP proxy, and the real PG / Redis is never
// stopped:
//
//	Proxy.Cut()        the process died: the connections open are broken and a new one is refused
//	Proxy.Blackhole()  the host is down: the connections open answer nothing and a new one's SYN is never answered
//	Proxy.SetDelay(d)  the far end slows down; time.Hour is "it connects and not a byte comes back"
//
// The assertions follow the behavior written down in README.md / docs/architecture.md /
// docs/config.md / each module's README, and a failure message reads "the documentation says X,
// got Y"; a number the documentation does not pin down (how long a recovery takes, say) is
// measured and not asserted, and printed with t.Logf.
// Every number carries a "numbers:" prefix, to grep out of a -v run.
//
// How the files divide up:
//
//	fault_test.go             this file: the shared helpers, and Redis / PG cut off while running
//	fault_timeout_test.go     whether go-redis / gorm timeouts honor the caller's ctx and the configuration
//	fault_downstream_test.go  xhttp's timeout and retries with a slow or failing downstream
//	fault_startup_test.go     a dependency unreachable at the start, a wrong password, a malformed DSN
//
// The slack on the timings: on this machine (4 cores) the time a fault path takes is decided by the
// timeouts, and the jitter is a few to a few dozen milliseconds; the asserted upper bounds add
// faultSlack to the number the documentation implies, for the reason in its comment.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// faultSlack is the slack added on top of the upper bound the documentation implies.
//
// What is measured is decided by the timeouts (hundreds of milliseconds to seconds), and measured
// here, what they take beyond their timeout is 1 to 30ms (every TestFault in parallel, -count=3).
// 300ms is ten times that and still far under every timeout under test, so it lets no "the timeout
// did not take" through
const faultSlack = 300 * time.Millisecond

// faultUnaffected is the upper bound of "it does not touch the broken dependency, works as usual
// and is fast".
//
// These requests measured 1 to 5ms (every TestFault in parallel). Were the broken dependency really
// to hold them up, it would hold them for a timeout, and the shortest timeout under test is 500ms
// (XRedis.DialTimeout / ReadTimeout, XGorm.DialTimeout).
// 400ms is two orders of magnitude over what was measured, enough for a service with -race on a
// loaded machine, and still shorter than the shortest of those timeouts
const faultUnaffected = 400 * time.Millisecond

// The defaults of XRedis in xredis/README.md; service/application.yml changes none of them
const (
	faultRedisDialTimeout     = 500 * time.Millisecond
	faultRedisReadTimeout     = 500 * time.Millisecond
	faultRedisMaxRetries      = 3           // "MaxRetries: 0 leaves it to go-redis (3 times)"
	faultRedisMaxRetryBackoff = time.Second // "MaxRetryBackoff: 0 leaves it to go-redis (1s)"
)

// faultRedisAttemptBudget is the upper bound of one attempt: a connect plus one round trip.
// That is xredis's own model -- pingTimeout in xredis.go is worked out exactly this way
// (DialTimeout + ReadTimeout)
const faultRedisAttemptBudget = faultRedisDialTimeout + faultRedisReadTimeout

// faultRedisCmdBudget is how long one command takes at most, following the documented defaults:
// MaxRetries+1 attempts, each under faultRedisAttemptBudget, with a backoff between two of them
// under MaxRetryBackoff
const faultRedisCmdBudget = (faultRedisMaxRetries+1)*faultRedisAttemptBudget + faultRedisMaxRetries*faultRedisMaxRetryBackoff // 7s

// faultDepResult is the result of a GET /dep
type faultDepResult struct {
	Status int
	Took   time.Duration // the whole request, measured by the client
	Server time.Duration // the one dependency operation, measured inside the handler
	Error  string        // the dependency operation's error
	ReqErr error         // non-nil when the request itself did not finish (the client gave up at its deadline)
}

func (r faultDepResult) String() string {
	if r.ReqErr != nil {
		return fmt.Sprintf("the request gave up after %v: %v", r.Took.Round(time.Millisecond), r.ReqErr)
	}
	return fmt.Sprintf("status=%d took %v (the dependency operation %v) error=%q", r.Status, r.Took.Round(time.Millisecond), r.Server.Round(time.Millisecond), r.Error)
}

// faultDep calls GET /dep?target=...[&timeout=...] with the client waiting wait at most. It takes no t and can be called from any goroutine
func faultDep(p *harness.Process, target, timeout string, wait time.Duration) faultDepResult {
	path := "/dep?target=" + target
	if timeout != "" {
		path += "&timeout=" + timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	start := time.Now()
	r, err := p.Request(ctx, http.MethodGet, path, nil)
	res := faultDepResult{Status: r.Status, Took: time.Since(start), ReqErr: err}
	if err == nil {
		var body struct {
			ElapsedMS float64 `json:"elapsed_ms"`
			Error     string  `json:"error"`
		}
		_ = json.Unmarshal(r.Body, &body)
		res.Server = time.Duration(body.ElapsedMS * float64(time.Millisecond))
		res.Error = body.Error
	}
	return res
}

// faultTimed sends one request and times it, and t.Fatals when it fails
func faultTimed(t *testing.T, p *harness.Process, method, path string, body any) (harness.Response, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	r, err := p.Request(ctx, method, path, body)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("%s %s had still not returned after %v: %v", method, path, took, err)
	}
	return r, took
}

// faultWaitRecover probes one dependency until it answers 200 and returns how long that took; it t.Fatals when it has not recovered within limit
func faultWaitRecover(t *testing.T, p *harness.Process, target string, limit time.Duration) (time.Duration, int) {
	t.Helper()
	start := time.Now()
	for n := 1; ; n++ {
		r := faultDep(p, target, "", 10*time.Second)
		if r.Status == http.StatusOK {
			return time.Since(start), n
		}
		if time.Since(start) > limit {
			t.Fatalf("%s was still unavailable %v after the fault was lifted (probe %d: %v)", target, limit, n, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// faultMS is milliseconds with one decimal
func faultMS(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }

// faultSummary is the min / median / max of a set of timings
func faultSummary(ds []time.Duration) string {
	if len(ds) == 0 {
		return "(none)"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return fmt.Sprintf("n=%d min=%s p50=%s max=%s", len(s), faultMS(s[0]), faultMS(s[len(s)/2]), faultMS(s[len(s)-1]))
}

// faultGoRedisLogLine is a line written by go-redis's own logger (internal.Logger, in the standard library log's format)
var faultGoRedisLogLine = regexp.MustCompile(`(?m)^redis: \d{4}/\d{2}/\d{2} .*$`)

// faultRedisACLUser creates an ACL user with a password on Redis and deletes it when the test ends.
//
// The Redis on this host has no password, so configuring a Password for xredis would be refused at
// the AUTH; an ACL user of its own is what makes "the password stays out of the logs" testable. The
// user name carries a random suffix, so running in parallel does not collide
func faultRedisACLUser(t *testing.T) (name, password string) {
	t.Helper()
	rc := harness.Redis(t)
	name, password = "e2e_fault_"+harness.NewID(), "redis-secret-"+harness.NewID()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rc.Do(ctx, "ACL", "SETUSER", name, "on", ">"+password, "~*", "&*", "+@all").Err(); err != nil {
		t.Fatalf("create redis ACL user: %v", err)
	}
	t.Cleanup(func() { // registered after rc's close, so it runs first while rc is still open
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rc.Do(ctx, "ACL", "DELUSER", name).Err(); err != nil {
			t.Logf("cleanup: delete redis ACL user %s: %v", name, err)
		}
	})
	return name, password
}

// faultBurst sends n identical /dep requests concurrently and returns each one's result
func faultBurst(p *harness.Process, n int, target string, wait time.Duration) []faultDepResult {
	out := make([]faultDepResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = faultDep(p, target, "", wait)
		}()
	}
	wg.Wait()
	return out
}

// faultQuick asserts that a request not touching the broken dependency answers as usual, and fast
func faultQuick(t *testing.T, p *harness.Process, what, path string, want int) harness.Response {
	t.Helper()
	r, took := faultTimed(t, p, http.MethodGet, path, nil)
	if r.Status != want {
		t.Errorf("%s does not touch the broken dependency and should answer %d as usual, got %v", what, want, r)
	}
	if took > faultUnaffected {
		t.Errorf("%s does not touch the broken dependency and should be unaffected (< %v), and it took %v", what, faultUnaffected, took)
	}
	t.Logf("numbers: %s took %s during the fault", what, faultMS(took))
	return r
}

// The Redis process dies while running (the port refuses connections):
//
//   - an operation using Redis fails within the budget the documentation implies
//     (faultRedisCmdBudget) and does not hang; the same holds under concurrency
//   - the three-level read falls back to PG (service/users.go: a Redis error records a WARN and
//     falls back to PG), and placing an order fails at the charge step and rolls back
//   - the endpoints that do not touch Redis are unaffected: /ping, ?cache=off, a local cache hit
//   - once Redis is back it recovers without a restart
//   - the Redis password appears nowhere in the output (xredis/README.md, XRedis.Password: "this
//     module writes it into no log at all")
func TestFault_RedisRefusedAtRuntime_ErrsInBudget_ReadsFallBackToPG_Recovers(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	acl, pw := faultRedisACLUser(t)
	rp := harness.NewProxy(t, harness.RedisAddr())
	p := harness.Start(t, harness.Options{
		RedisAddr: rp.Addr(),
		Overlay:   fmt.Sprintf("XRedis:\n  Username: %s\n  Password: %q\n", acl, pw),
	})

	cached := createUser(t, p, "cached", "cached@example.com")
	getUser(t, p, cached.ID) // db -> written into the local cache and into Redis
	if u, _, _ := getUser(t, p, cached.ID); u.Source != "local" {
		t.Fatalf("the second read before the fault should hit the local cache, got source=%s", u.Source)
	}
	cold := createUser(t, p, "cold", "cold@example.com") // never read: in neither cache level
	if r := faultDep(p, "redis", "", 10*time.Second); r.Status != http.StatusOK {
		t.Fatalf("Redis should be fine before the fault: %v", r)
	}

	rp.Cut()

	t.Run("an operation using Redis fails within the budget", func(t *testing.T) {
		var took []time.Duration
		for i := range 8 {
			r := faultDep(p, "redis", "", 2*faultRedisCmdBudget)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("attempt %d: /dep should answer 503 while Redis refuses connections, got %v", i+1, r)
			}
			if !strings.Contains(r.Error, "connection refused") {
				t.Errorf("attempt %d: the error should say the connection was refused, got %q", i+1, r.Error)
			}
			if r.Server > faultRedisCmdBudget+faultSlack {
				t.Errorf("attempt %d: by the documented defaults one command takes %v at most ((MaxRetries+1)x(DialTimeout+ReadTimeout)+MaxRetriesxMaxRetryBackoff), got %v",
					i+1, faultRedisCmdBudget, r.Server)
			}
			took = append(took, r.Server)
		}
		// Measured at 70 to 150ms (two rounds), all of it the command-level retry backoff: xredis sets DialerRetries to 1, so a connect fails there and then when connections are refused.
		// go-redis used to redial 5 times inside every connect, 100ms apart, which made the first two commands take 1.69 to 1.74s
		t.Logf("numbers: one command took %v in turn while Redis refused connections; %s (the budget being %v)", took, faultSummary(took), faultRedisCmdBudget)

		burst := faultBurst(p, 20, "redis", 2*faultRedisCmdBudget)
		var bt []time.Duration
		for i, r := range burst {
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || r.Server > faultRedisCmdBudget+faultSlack {
				t.Errorf("concurrent request %d: it should answer 503 within %v, got %v", i, faultRedisCmdBudget, r)
			}
			bt = append(bt, r.Took)
		}
		t.Logf("numbers: 20 concurrent /dep?target=redis: %s", faultSummary(bt))
	})

	t.Run("the three-level read falls back to PG and the order fails and rolls back", func(t *testing.T) {
		r, took := faultTimed(t, p, http.MethodGet, fmt.Sprintf("/users/%d", cold.ID), nil)
		var u user
		r.JSON(t, &u)
		if r.Status != http.StatusOK || u.Source != "db" || u.Name != "cold" {
			t.Errorf("a read of a user should fall back to PG when Redis fails (200, source=db), got %v", r)
		}
		// The GET and the SET that fills it back in each touch Redis once
		if took > 2*faultRedisCmdBudget+faultSlack {
			t.Errorf("one three-level read touches Redis twice, which the documentation puts at %v at most, got %v", 2*faultRedisCmdBudget, took)
		}
		trace := traceIDOf(t, r)
		warn, ok := p.LookForLog(waitFor, func(l harness.Log) bool {
			return l.Msg() == "redis read failed, falling back to the database" && l.Str("trace_id") == trace
		})
		if !ok {
			t.Fatalf("falling back should record one WARN carrying this request's trace_id (%s) (redis read failed, falling back to the database), and there is none within %v; there are %d lines with that message",
				trace, waitFor, len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "redis read failed, falling back to the database" })))
		}
		if warn.Level() != "WARN" {
			t.Errorf("falling back should record a WARN, got %s: %s", warn.Level(), warn.Line)
		}
		t.Logf("numbers: one falling-back read took %s while Redis refused connections", faultMS(took))

		r, took = faultTimed(t, p, http.MethodPost, "/orders", map[string]any{"user_id": cold.ID, "amount": 7})
		if r.Status != http.StatusInternalServerError {
			t.Fatalf("placing an order (whose step 2 writes Redis) should fail with 500 while Redis is unavailable, got %v", r)
		}
		var o struct {
			ID     string `json:"id"`
			Rolled bool   `json:"rolled"`
			Error  string `json:"error"`
		}
		r.JSON(t, &o)
		if !strings.Contains(o.Error, `step "charge"`) || !o.Rolled {
			t.Errorf("it should fail at the charge step and roll the create that was done back, got %v", r)
		}
		if st := orderStatus(t, harness.DB(t), p, o.ID); st != "" {
			t.Errorf("after the rollback PG should hold no such order, got status=%q", st)
		}
		if took > faultRedisCmdBudget+faultSlack {
			t.Errorf("placing an order touches Redis once, which the documentation puts at %v at most, got %v", faultRedisCmdBudget, took)
		}
		t.Logf("numbers: one order (failing and rolling back) took %s while Redis refused connections", faultMS(took))
	})

	t.Run("the endpoints that do not touch Redis are unaffected", func(t *testing.T) {
		faultQuick(t, p, "GET /ping", "/ping", http.StatusOK)
		faultQuick(t, p, "GET /users/:id?cache=off (PG alone)", fmt.Sprintf("/users/%d?cache=off", cold.ID), http.StatusOK)
		var u user
		faultQuick(t, p, "GET /users/:id (a local cache hit)", fmt.Sprintf("/users/%d", cached.ID), http.StatusOK).JSON(t, &u)
		if u.Source != "local" {
			t.Errorf("a user the local cache holds should hit local outright, got source=%s", u.Source)
		}
	})

	t.Run("it recovers by itself once Redis is back", func(t *testing.T) {
		rp.Restore()
		took, n := faultWaitRecover(t, p, "redis", 10*time.Second)
		t.Logf("numbers: /dep was back to 200 %s after Redis started listening again (probe %d)", faultMS(took), n)

		fresh := createUser(t, p, "after", "after@example.com")
		if u, _, _ := getUser(t, p, fresh.ID); u.Source != "db" {
			t.Errorf("the first read after the recovery should reach PG, got source=%s", u.Source)
		}
		// Filling Redis back in succeeded: Redis read directly holds this key
		key := p.KeyPrefix + "user:" + fmt.Sprint(fresh.ID)
		if n, err := harness.Redis(t).Exists(context.Background(), key).Result(); err != nil || n != 1 {
			t.Errorf("after the recovery a three-level read should fill the user back into Redis (%s), got exists=%d err=%v", key, n, err)
		}
		if r := p.PostJSON(t, "/orders", map[string]any{"user_id": fresh.ID, "amount": 1}); r.Status != http.StatusCreated {
			t.Errorf("placing an order should succeed after the recovery, got %v", r)
		}
	})

	t.Run("the Redis password is not in the output", func(t *testing.T) {
		exit := p.Terminate(t, 20*time.Second)
		if exit.Code != 0 {
			t.Errorf("it should exit with 0 after the SIGTERM, got %v", exit)
		}
		out := p.Output()
		mustNotContain(t, "the process's stdout / stderr", out, pw)
		if !strings.Contains(out, "connection refused") {
			t.Errorf("this round should have left a connection refused in the log and there is not one: the check does not cover enough")
		}
		t.Logf("numbers: checked %d bytes of output", len(out))
	})

	// The summary table in docs/behavior.md: GORM's default stdout logger and resty's logger that
	// writes to stderr are both wired back into slog, for the reason that "a log that goes around
	// slog never reaches the logging platform". go-redis's internal.Logger is the same kind of
	// thing: xredis wires it into slog with redis.SetLogger in init, with the message xredis
	// go-redis log at WARN (xredis/README.md, XRedis). Redis refuses connections in this round, so
	// go-redis records a failed to dial on every failed connect. trace_id is not asserted:
	// xredis/README.md writes down that a connect happens on go-redis's own goroutine with its own
	// context.Background(), so such a log line cannot carry the request's trace_id (measured: not
	// one of 45 lines did)
	t.Run("go-redis's own log goes through slog as well", func(t *testing.T) {
		lines := faultGoRedisLogLine.FindAllString(p.Stderr(), -1)
		if len(lines) > 0 {
			t.Errorf("go-redis's own log should reach slog, and stderr holds %d lines of plain text, for instance: %s", len(lines), lines[0])
		}
		logs := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xredis go-redis log" })
		if len(logs) == 0 {
			t.Fatalf("go-redis records a failed to dial while Redis refuses connections, so slog should hold an xredis go-redis log, and there is not one")
		}
		traced := 0
		for _, l := range logs {
			if l.Level() != "WARN" {
				t.Errorf("go-redis's log should be recorded at WARN, got %s", l.Line)
			}
			if l.Str("trace_id") != "" {
				traced++
			}
		}
		t.Logf("numbers: slog holds %d of go-redis's own log lines, %d of them with a trace_id; for instance: %s", len(logs), traced, logs[0].Line)
	})
}

// The PG process dies while running (the port refuses connections):
//
//   - an operation using PG fails there and then (a refused connection waits for no timeout), with
//     the address in the error and no password
//   - the endpoints that do not touch PG are unaffected: /ping, Redis, a local cache hit
//   - once PG is back it recovers without a restart
func TestFault_PGRefusedAtRuntime_FailsFast_OthersUnaffected_Recovers(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	pw := pgPassword(t)
	pg := harness.NewProxy(t, harness.PGAddr())
	// DialTimeout is raised from the default 500ms to 2s: the bound of "a refusal waits for no timeout" is 1s, which is far
	// from both what was measured (milliseconds) and the counterexample (waiting out one DialTimeout). A bound hugging 500ms would misfire on a loaded machine
	p := harness.Start(t, harness.Options{PGAddr: pg.Addr(), Overlay: "XGorm:\n  Clients:\n    default:\n      DialTimeout: 2s\n"})

	cached := createUser(t, p, "cached", "cached@example.com")
	getUser(t, p, cached.ID)
	if u, _, _ := getUser(t, p, cached.ID); u.Source != "local" {
		t.Fatalf("the second read before the fault should hit the local cache, got source=%s", u.Source)
	}

	pg.Cut()

	t.Run("an operation using PG fails there and then", func(t *testing.T) {
		var took []time.Duration
		for i := range 5 {
			r := faultDep(p, "db", "", 30*time.Second)
			if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
				t.Fatalf("attempt %d: /dep?target=db should answer 503 while PG refuses connections, got %v", i+1, r)
			}
			if !strings.Contains(r.Error, "connection refused") || !strings.Contains(r.Error, pg.Addr()) {
				t.Errorf("attempt %d: the error should say what it cannot reach (%s, connection refused), got %q", i+1, pg.Addr(), r.Error)
			}
			// A refused connection is immediate: database/sql drops the broken connection and redials, pgx's dial fails at once and there is no timeout to wait out.
			// The bound is half of DialTimeout (raised to 2s above): waiting out even one timeout would pass it
			if r.Server > time.Second {
				t.Errorf("attempt %d: a refused connection should wait for no timeout, got %v", i+1, r.Server)
			}
			took = append(took, r.Server)
		}
		t.Logf("numbers: SELECT 1 took %v in turn while PG refused connections; %s", took, faultSummary(took))

		r, took2 := faultTimed(t, p, http.MethodPost, "/users", map[string]string{"name": "while-cut"})
		if r.Status != http.StatusInternalServerError {
			t.Errorf("creating a user should answer 500 while PG is unavailable, got %v", r)
		}
		r, took3 := faultTimed(t, p, http.MethodPost, "/orders", map[string]any{"user_id": cached.ID, "amount": 1})
		var o struct {
			Rolled bool   `json:"rolled"`
			Error  string `json:"error"`
		}
		r.JSON(t, &o)
		if r.Status != http.StatusInternalServerError || !strings.Contains(o.Error, `step "create"`) || o.Rolled {
			t.Errorf("placing an order should fail at step 1 with nothing to roll back while PG is unavailable, got %v", r)
		}
		t.Logf("numbers: POST /users took %s and POST /orders took %s while PG refused connections", faultMS(took2), faultMS(took3))
	})

	t.Run("the endpoints that do not touch PG are unaffected", func(t *testing.T) {
		faultQuick(t, p, "GET /ping", "/ping", http.StatusOK)
		faultQuick(t, p, "GET /dep?target=redis", "/dep?target=redis", http.StatusOK)
		var u user
		faultQuick(t, p, "GET /users/:id (a local cache hit)", fmt.Sprintf("/users/%d", cached.ID), http.StatusOK).JSON(t, &u)
		if u.Source != "local" {
			t.Errorf("a user the local cache holds should hit local outright, got source=%s", u.Source)
		}
	})

	t.Run("it recovers by itself once PG is back", func(t *testing.T) {
		pg.Restore()
		took, n := faultWaitRecover(t, p, "db", 10*time.Second)
		t.Logf("numbers: SELECT 1 was back to 200 %s after PG started listening again (probe %d)", faultMS(took), n)
		fresh := createUser(t, p, "after", "after@example.com")
		if u, _, _ := getUser(t, p, fresh.ID); u.Source != "db" || u.Name != "after" {
			t.Errorf("reads and writes should work as usual after the recovery, got %+v", u)
		}
	})

	t.Run("the PG password is not in the output", func(t *testing.T) {
		exit := p.Terminate(t, 20*time.Second)
		if exit.Code != 0 {
			t.Errorf("it should exit with 0 after the SIGTERM, got %v", exit)
		}
		out := p.Output()
		mustNotContain(t, "the process's stdout / stderr", out, pw, harness.PGDSN(pg.Addr()))
		if !strings.Contains(out, "connection refused") {
			t.Errorf("this round should have left a connection refused in the log and there is not one: the check does not cover enough")
		}
		t.Logf("numbers: checked %d bytes of output", len(out))
	})
}
