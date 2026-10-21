package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// Two kinds of "the far end says nothing":
//
//	silent  the process is alive and TCP connects, but not a byte comes back (SetDelay(time.Hour)): a process that is wedged, a server that is full
//	down    the host is down: the connections already open say nothing and a new connection's SYN is never answered (Blackhole)
//
// The "the process died" kind (connection refused) is in fault_test.go, where no timeout has to be
// waited out at all
var faultSilentModes = []struct {
	name   string
	inject func(*harness.Proxy)
}{
	{"the far end says nothing", func(p *harness.Proxy) { p.SetDelay(time.Hour) }},
	{"the host is down", func(p *harness.Proxy) { p.Blackhole() }},
}

// The summary table in docs/behavior.md:
//
//	XRedis command timeout | the library itself: ReadTimeout alone | here: the caller's ctx
//	otherwise a request on a 200ms budget would wait out ReadTimeout (measured 5s) on a slow Redis
//
// So ReadTimeout is set to 5s to reproduce that: with the caller giving 200ms, it should return at
// 200ms.
// A connection the pool already has (stuck on a read) and a new one (stuck in the handshake or on
// the SYN) both have to honor it, so each fault sends 12 in a row. MinIdleConns is 0, so the pool
// holds the one connection the Ping at the start used alone: the first use takes it (stuck on a
// read) and every one after it opens a new connection (stuck in the handshake or on the SYN).
// Both stretches report context deadline exceeded, which does not tell them apart; the proxy does:
// the proxy holding no connection when the command starts (the host is down), or the proxy
// accepting a new connection during the command (the far end says nothing), is the new-connection
// stretch

func TestFault_RedisSlowOrDown_CommandReturnsAtCallerDeadline_NoReadTimeoutWait(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const deadline = 200 * time.Millisecond
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			rp := harness.NewProxy(t, harness.RedisAddr())
			p := harness.Start(t, harness.Options{RedisAddr: rp.Addr(), Overlay: "XRedis:\n  ReadTimeout: 5s\n  MinIdleConns: 0\n"})
			m.inject(rp)

			var took []time.Duration
			phases := map[string]int{}
			for i := range 12 {
				empty, accepted := rp.Active() == 0, rp.Accepted()
				r := faultDep(p, "redis", deadline.String(), 10*time.Second)
				phase := "a pooled connection"
				if empty || rp.Accepted() > accepted {
					phase = "a new connection"
				}
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d: the caller gave %v and it should answer 503 at that moment, got %v", i+1, deadline, r)
				}
				if r.Server > deadline+faultSlack {
					t.Errorf("attempt %d: docs/architecture.md says a Redis command honors the caller's ctx, so with %v given it should return at %v, got %v (ReadTimeout being 5s): %s",
						i+1, deadline, deadline, r.Server, r.Error)
				}
				phases[phase]++
				took = append(took, r.Server)
			}
			t.Logf("numbers: %s with ReadTimeout=5s and the caller giving %v: the 12 attempts took %v in turn; %s; %v", m.name, deadline, took, faultSummary(took), phases)
			if phases["a new connection"] == 0 || phases["a pooled connection"] == 0 {
				t.Errorf("both the pooled and the new connection stretches should be measured, got %v", phases)
			}
		})
	}
}

// xredis/README.md, XRedis.ReadTimeout: with no deadline from the caller, this is what holds one
// command.
// It is set to 300ms, clearly apart from the default (500ms), and 300ms is what should be seen --
// what it goes by is the configuration and no other number.
//
// Measured: the first command after the fault takes about 2 x ReadTimeout (0.63 to 0.66s here; 1.03
// to 1.05s with the default 500ms), and every one after it about 1 x ReadTimeout (0.31 to 0.34s).
// The upper bound follows the documented model: one attempt stays under DialTimeout + ReadTimeout
// (xredis's pingTimeout is worked out the same way), and the median should be around ReadTimeout
func TestFault_RedisSilentNoCallerDeadline_CommandFailsAtReadTimeout(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const readTimeout = 300 * time.Millisecond
	rp := harness.NewProxy(t, harness.RedisAddr())
	p := harness.Start(t, harness.Options{RedisAddr: rp.Addr(), Overlay: "XRedis:\n  ReadTimeout: 300ms\n"})
	rp.SetDelay(time.Hour)

	var took []time.Duration
	for i := range 6 {
		r := faultDep(p, "redis", "", 2*faultRedisCmdBudget)
		if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "i/o timeout") {
			t.Fatalf("attempt %d: with Redis silent it should time out reading (503, i/o timeout), got %v", i+1, r)
		}
		if lim := faultRedisDialTimeout + readTimeout; r.Server > lim+faultSlack {
			t.Errorf("attempt %d: one attempt takes DialTimeout+ReadTimeout=%v at most, got %v", i+1, lim, r.Server)
		}
		took = append(took, r.Server)
	}
	med := faultMedian(took)
	if med < readTimeout-20*time.Millisecond || med > readTimeout+faultSlack/3 {
		t.Errorf("the documentation says ReadTimeout holds the read: with %v configured, the median of one command should be around it, got %v (%v)", readTimeout, med, took)
	}
	t.Logf("numbers: Redis silent with ReadTimeout=%v and no deadline: the 6 commands took %v in turn; %s", readTimeout, took, faultSummary(took))
}

// How long one command takes with the Redis host down and no deadline from the caller.
//
// xredis/README.md, XRedis: one command takes
// (MaxRetries+1)x(DialTimeout+ReadTimeout)+MaxRetriesxMaxRetryBackoff at most, which is 7s by
// default (faultRedisCmdBudget); with MaxRetries: -1 one attempt is left, under
// DialTimeout+ReadTimeout (1s).
//
// That formula holds because xredis sets DialerRetries to 1: go-redis v9.22.0 redials 5 times
// inside every connect by default, waiting DialerRetryTimeout=100ms between two of them, so one
// "connect" is 5x500ms+4x100ms = 2.9s and the default 4 attempts measured 11.7s.
// Only after PoolSize (10xGOMAXPROCS) failed connects in all does the pool start reporting the
// previous error outright.
//
// Before that "connect" stretch is reached, the connections the pool already has (warmed up by
// MinIdleConns) each time out reading once, and those commands take 0.5 to 1s; so it loops until
// dial tcp turns up in the error, and what is measured is the first command that reached a connect
func TestFault_RedisHostDownNoCallerDeadline_CommandWithinDocumentedBudget(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	cases := []struct {
		name    string
		overlay string
		budget  time.Duration
		why     string
	}{
		{"the retries turned off", "XRedis:\n  MaxRetries: -1\n  MinIdleConns: 0\n", faultRedisAttemptBudget,
			"MaxRetries: -1 leaves one attempt, whose connect stays under DialTimeout (500ms), plus one round trip"},
		{"the defaults", "", faultRedisCmdBudget,
			"(MaxRetries+1)x(DialTimeout+ReadTimeout)+MaxRetriesxMaxRetryBackoff"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rp := harness.NewProxy(t, harness.RedisAddr())
			p := harness.Start(t, harness.Options{RedisAddr: rp.Addr(), Overlay: c.overlay})
			rp.Blackhole()

			var pooled []time.Duration
			var first faultDepResult
			for i := 0; ; i++ {
				if i == 12 {
					t.Fatalf("12 commands never reached a connect, so the pool holds more connections than expected: %v", pooled)
				}
				r := faultDep(p, "redis", "", 5*faultRedisCmdBudget)
				if r.ReqErr != nil {
					t.Fatalf("command %d had not returned after %v", i+1, r.Took)
				}
				if strings.Contains(r.Error, "dial tcp") {
					first = r
					break
				}
				pooled = append(pooled, r.Server)
			}
			t.Logf("numbers: the Redis host is down (%s): the pooled connections each timed out reading once at %v; the first command that reached a connect took %v (%s)",
				c.name, pooled, first.Server.Round(time.Millisecond), first.Error)

			if first.Server > c.budget+faultSlack {
				t.Errorf("with the Redis host down, one command takes %v at most by the documentation (%s), and it measured %v",
					c.budget, c.why, first.Server.Round(time.Millisecond))
			}
		})
	}
}

// With a deadline from the caller, a gorm query returns at that moment: xgorm/README.md does not
// write this one down on its own, and what it rests on is xgorm.CWithCtx's comment, "take an
// instance and bind ctx to it, which is what carries the trace and the timeout downstream".
// A pooled connection (stuck on a read) and a new one (stuck in startup or on the SYN) both have to
// honor it, so it sends 6 in a row
func TestFault_PGSlowOrDown_QueryReturnsAtCallerDeadline(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const deadline = 200 * time.Millisecond
	for _, m := range faultSilentModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			pg := harness.NewProxy(t, harness.PGAddr())
			p := harness.Start(t, harness.Options{PGAddr: pg.Addr()})
			m.inject(pg)

			var took []time.Duration
			newConn := 0
			for i := range 6 {
				r := faultDep(p, "db", deadline.String(), 10*time.Second)
				if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d: the caller gave %v and it should answer 503 at that moment, got %v", i+1, deadline, r)
				}
				if r.Server > deadline+faultSlack || r.Server < deadline-10*time.Millisecond {
					t.Errorf("attempt %d: the caller gave %v and the query should return at that moment, got %v: %s", i+1, deadline, r.Server, r.Error)
				}
				if strings.Contains(r.Error, "failed to connect") {
					newConn++
				}
				took = append(took, r.Server)
			}
			if newConn == 0 {
				t.Errorf("all 6 used a pooled connection, and the new-connection half was never measured")
			}
			t.Logf("numbers: PG %s with the caller giving %v: the 6 attempts took %v in turn; %s; %d of them a new connection", m.name, deadline, took, faultSummary(took), newConn)
		})
	}
}

// xgorm/README.md, XGorm.DialTimeout: "for PostgreSQL it is injected as connect_timeout (rounded
// up to seconds)", and "what pgx governs with connect_timeout is the whole connect to one host ...
// measured against a server where TCP connects at once and startup says nothing, connect_timeout=1
// waited out 1.0s".
//
// With the caller giving a deadline far wider than that (5s), a new connection should fail at
// connect_timeout: the default 500ms -> 1s; 1500ms configured -> rounded up to 2s. Both faults
// (startup saying nothing, the SYN never answered) behave the same
func TestFault_PGSlowOrDown_NewConnFailsAtInjectedConnectTimeout(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	for _, dial := range []struct {
		cfg  string
		want time.Duration
	}{{"", time.Second}, {"1500ms", 2 * time.Second}} {
		for _, m := range faultSilentModes {
			name := fmt.Sprintf("DialTimeout=%s_%s", faultOr(dial.cfg, "500ms by default"), m.name)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				pg := harness.NewProxy(t, harness.PGAddr())
				o := harness.Options{PGAddr: pg.Addr()}
				if dial.cfg != "" {
					o.Overlay = "XGorm:\n  Clients:\n    default:\n      DialTimeout: " + dial.cfg + "\n"
				}
				p := harness.Start(t, o)
				m.inject(pg)
				// Use up the connections the pool already has with a short deadline first (they are stuck on a read, which connect_timeout does not govern)
				for i := 0; ; i++ {
					if i == 10 {
						t.Fatal("10 queries with a short deadline never reached a new connection")
					}
					if r := faultDep(p, "db", "100ms", 10*time.Second); strings.Contains(r.Error, "failed to connect") {
						break
					}
				}
				var took []time.Duration
				for i := range 2 {
					r := faultDep(p, "db", "5s", 10*time.Second)
					if r.ReqErr != nil || r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Error, "failed to connect") {
						t.Fatalf("attempt %d: a new connection should fail (503, failed to connect), got %v", i+1, r)
					}
					if !strings.Contains(r.Error, pg.Addr()) {
						t.Errorf("attempt %d: the error should hold the address it cannot reach, %s, got %q", i+1, pg.Addr(), r.Error)
					}
					if r.Server < dial.want-20*time.Millisecond || r.Server > dial.want+faultSlack {
						t.Errorf("attempt %d: the documentation says DialTimeout is injected as connect_timeout (rounded up to seconds)=%v and a new connection should fail then, got %v",
							i+1, dial.want, r.Server)
					}
					took = append(took, r.Server)
				}
				t.Logf("numbers: PG %s with DialTimeout=%s and the caller giving 5s: the new connections failed at %v in turn (connect_timeout=%v)",
					m.name, faultOr(dial.cfg, "500ms"), took, dial.want)
			})
		}
	}
}

// No deadline from the caller and a pooled connection meeting a silent PG: xgorm/README.md,
// "PostgreSQL" -- Postgres.StatementTimeout is "unlimited by default", PG has no ReadTimeout to
// configure the way MySQL does, and statement_timeout is a server-side GUC, which cannot reach
// anything once the network is gone.
// So by the documentation this query waits forever. This pins that behavior: after 3s it has still
// not returned.
// The day the framework gives PG a default read timeout, this goes red, and the documentation
// changes with it.
//
// The other half: when the caller gives up (breaking the connection) the request's ctx is cancelled
// and the query returns with it -- the handler does not hang there forever
func TestFault_PGSilentNoCallerDeadline_QueryWaitsUntilCallerGivesUp(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const giveUp = 3 * time.Second
	pg := harness.NewProxy(t, harness.PGAddr())
	p := harness.Start(t, harness.Options{PGAddr: pg.Addr()})
	pg.SetDelay(time.Hour)

	r := faultDep(p, "db", "", giveUp)
	if r.ReqErr == nil {
		t.Fatalf("the documentation says StatementTimeout is unlimited by default and PG has no read timeout: a query with no deadline should wait, and it returned after %v: %v", r.Took, r)
	}
	gaveUpAt := time.Now()
	t.Logf("numbers: PG silent with no deadline: the query had still not returned after %v, and the client gave up", r.Took.Round(time.Millisecond))

	l, ok := p.LookForLog(5*time.Second, func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == "/dep" })
	if !ok {
		t.Fatalf("the request's ctx is cancelled once the caller disconnects and the query should return with it (xgorm.CWithCtx: \"which is what carries the trace and the timeout downstream\"): the handler had still not returned after 5s and there is no /dep access log line")
	}
	t.Logf("numbers: %s after the client gave up, the handler returned and recorded an access log line (status=%s)", faultMS(time.Since(gaveUpAt)), l.Str("status"))
}

// faultMedian is the median
func faultMedian(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s[len(s)/2]
}

// faultOr is def when v is empty
func faultOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
