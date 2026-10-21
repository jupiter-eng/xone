package xredis

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// This group pins down the behavior when Redis fails: how many times a connect dials, what the ctx's cancellation and
// deadline each govern, how a failed authentication is reported, and where go-redis's own logs go. The numbers are written into xredis/README.md's XRedis section.

func TestNew_RedisDownDialsOncePerConnect(t *testing.T) {
	// go-redis v9.22.0 redials 5 times internally per connect, 100ms apart: a refused connection wastes 400ms per
	// connect, and a host that is down is 5xDialTimeout+400ms. xredis sets it to dial once, leaving only the MaxRetries layer.
	f := newFakeRedis(t)
	c := liveCfg(f)
	c.MinIdleConns, c.MaxRetries, c.Trace = 0, -1, false
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	f.stop()
	// The connection in the pool is already dead: the first command may hit it first (EOF) and only a later one reaches a connect.
	for i := range 3 {
		start := time.Now()
		err := client.Get(context.Background(), "k").Err()
		took := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			continue
		}
		t.Logf("with the connection refused, command %d took %v: %v", i+1, took, err)
		// The bound is a little over half of go-redis's default behavior (400ms): measured it returns on the spot (milliseconds), with room to spare on a slow machine
		if took > 250*time.Millisecond {
			t.Errorf("one connect must dial once: a refused connection must return on the spot, got %v (go-redis's default of 5 redials 100ms apart is 400ms)", took)
		}
		return
	}
	t.Fatal("none of the 3 commands reached a connect")
}

func TestNew_CommandsIgnoreCtxCancelOnlyDeadline(t *testing.T) {
	// Pins down go-redis's behavior, which the documentation (the ContextTimeoutEnabled comment, xredis/README.md XRedis) follows:
	// cancelling the ctx cannot wake a command blocked on a read, and it returns only at ReadTimeout.
	// If this fails after an upgrade, go-redis has started honoring cancellation -- delete that limitation from the docs.
	f := newFakeRedis(t)
	f.setStall("get")
	c := liveCfg(f)
	c.ReadTimeout, c.MaxRetries = time.Second, -1
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	err = client.Get(ctx, "k").Err()
	took := time.Since(start)
	t.Logf("ReadTimeout=%v, ctx cancelled at 100ms: the command returned after %v: %v", c.ReadTimeout, took.Round(time.Millisecond), err)
	if took < c.ReadTimeout-100*time.Millisecond {
		t.Errorf("go-redis now honors ctx cancellation (returned in %v), so \"cancelling cannot wake a blocked command\" in the docs needs changing", took)
	}
}

func TestNew_ExitSignalAtStartupSkipsReadTimeout(t *testing.T) {
	// The peer accepts the connection and never answers, so Ping blocks on a read. go-redis honors only the ctx's
	// deadline, so Pinging in the current goroutine would make a cancellation wait for that read to hit ReadTimeout (5s here).
	// The 1.5s bound is far from both the 100ms cancellation point and the 5s read timeout, so it will not misfire on a slow machine.
	f := newFakeRedis(t)
	f.setStall("ping")
	c := liveCfg(f)
	c.ReadTimeout, c.MinIdleConns = 5*time.Second, 0

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := New(ctx, c)
	took := time.Since(start)
	t.Logf("cancelled at 100ms, New returned after %v: %v", took.Round(time.Millisecond), err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancellation must be reported as one, got %v", err)
	}
	if took > 1500*time.Millisecond {
		t.Errorf("a stop signal must make it give up on the spot, but it returned after %v (ReadTimeout=%v)", took, c.ReadTimeout)
	}
	// The abandoned Ping does not leak: New closes the client and the blocked connection is dropped with it.
	if n := f.waitConns(0); n != 0 {
		t.Errorf("every connection must be closed after giving up, %d remain", n)
	}
}

func TestNew_AuthFailureReportedClearlyWithoutRetry(t *testing.T) {
	f := newFakeRedis(t)
	f.setFailAuth(true)
	c := liveCfg(f)
	c.Password, c.MinIdleConns = "wrong", 0 // no warm-up: warmed connections would each send an AUTH of their own

	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("a wrong password must be an error")
	}
	t.Logf("error: %v", err)
	if !strings.Contains(err.Error(), "authentication to "+c.Addr+" failed") || strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("a failed authentication must say so rather than report a failed connection, got %v", err)
	}
	if !redis.IsAuthError(err) {
		t.Errorf("the original authentication error must be recognizable by redis.IsAuthError, got %v", err)
	}
	if n := f.count("auth"); n != 1 {
		t.Errorf("a wrong password stays wrong: there must be exactly one AUTH, got %d", n)
	}
	assertOneFrame(t, err, "xredis", "connect")
}

// ctxKey is the key used to test that go-redis's logging passes the ctx through.
type ctxKey struct{}

// recorded is the part of one log line the test cares about.
type recorded struct {
	level  slog.Level
	msg    string
	detail string
	ctxVal any
}

// ctxRecorder records every log line and the ctxKey it saw in the ctx.
type ctxRecorder struct {
	mu   *sync.Mutex
	recs *[]recorded
}

func (h ctxRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h ctxRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h ctxRecorder) WithGroup(string) slog.Handler            { return h }
func (h ctxRecorder) Handle(ctx context.Context, r slog.Record) error {
	rec := recorded{level: r.Level, msg: r.Message, ctxVal: ctx.Value(ctxKey{})}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "detail" {
			rec.detail = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	*h.recs = append(*h.recs, rec)
	h.mu.Unlock()
	return nil
}

func TestGoRedisOwnLogsGoToSlogWithCallerCtx(t *testing.T) {
	// go-redis writes plain text to stderr with the standard library log by default: neither JSON nor a trace_id.
	// Make it write one: an expiry shorter than 1ms makes it log "truncating to 1ms", using the command's ctx.
	f := newFakeRedis(t)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	var recs []recorded
	h := ctxRecorder{mu: &sync.Mutex{}, recs: &recs}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)

	ctx := context.WithValue(context.Background(), ctxKey{}, "trace-1")
	if err := client.Set(ctx, "k", "v", time.Microsecond).Err(); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range recs {
		if r.msg == "xredis go-redis log" && strings.Contains(r.detail, "truncating to 1ms") {
			if r.level != slog.LevelWarn {
				t.Errorf("the level must be WARN, got %v", r.level)
			}
			if r.ctxVal != "trace-1" {
				t.Errorf("the ctx must be passed to slog as it is (the trace_id relies on it), got %v", r.ctxVal)
			}
			return
		}
	}
	t.Errorf("go-redis's own logs must reach slog, but slog received %+v", recs)
}
