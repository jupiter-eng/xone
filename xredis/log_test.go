package xredis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// secret is a long random string: the "must not appear in the log" checks use it, since a short number would collide with a timestamp or a duration.
func secret(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// logClient builds an instance with Log on, under the instance name name.
func logClient(t *testing.T, f *fakeRedis, name string, mutate ...func(*ClientConfig)) *redis.Client {
	t.Helper()
	c := liveCfg(f)
	c.Log = true
	for _, m := range mutate {
		m(&c)
	}
	client, closer, err := newClient(context.Background(), name, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	return client
}

// cmdLines picks out the logs whose msg is one of these, dropping the HELLO on each new connection (the fake Redis does not know it and answers an error once per connection).
func cmdLines(all []map[string]any, msgs ...string) []map[string]any {
	var out []map[string]any
	for _, l := range all {
		for _, m := range msgs {
			if l["msg"] == m && l["cmd"] != "hello" {
				out = append(out, l)
			}
		}
	}
	return out
}

func TestLog_CommandLineHasNameCmdFirstKeyElapsed(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "session")

	if err := client.Set(context.Background(), "session:42", "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	got := cmdLines(lines(), "redis command")
	if len(got) != 1 {
		t.Fatalf("one redis command line per command, got=%v", lines())
	}
	l := got[0]
	if l["level"] != "INFO" || l["name"] != "session" || l["cmd"] != "set" || l["key"] != "session:42" {
		t.Errorf("it must carry the instance name, the command name and the first key at INFO, got=%v", l)
	}
	if _, ok := l["elapsed_ms"].(float64); !ok {
		t.Errorf("it must carry elapsed_ms (milliseconds, a number), got=%v", l)
	}
	for _, k := range []string{"error", "nil", "key_truncated", "threshold_ms"} {
		if _, ok := l[k]; ok {
			t.Errorf("a successful, non-nil, short-key, non-slow command must not carry %s, got=%v", k, l)
		}
	}
}

func TestLog_InstanceNameFromConfigReachesLog(t *testing.T) {
	// Aimed at the call site: the framework has to hand the configured name down when building an instance, or several instances cannot be told apart.
	f := newFakeRedis(t)
	lines := capture(t)
	c := liveCfg(f)
	c.Log = true
	if err := initComponent(t, Config{Clients: map[string]ClientConfig{"cache": c}}); err != nil {
		t.Fatal(err)
	}
	if err := C("cache").Get(context.Background(), "k").Err(); err != nil {
		t.Fatal(err)
	}
	got := cmdLines(lines(), "redis command")
	if len(got) != 1 || got[0]["name"] != "cache" {
		t.Errorf("the log's name must be the configured instance name cache, got=%v", got)
	}
}

func TestLog_DirectNewHasNoNameField(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)
	c := liveCfg(f)
	c.Log = true
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	client.Get(context.Background(), "k")
	got := cmdLines(lines(), "redis command")
	if len(got) != 1 {
		t.Fatalf("a direct New with Log on must record too, got=%v", lines())
	}
	if _, ok := got[0]["name"]; ok {
		t.Errorf("a direct New has no instance name and must not write an empty one, got=%v", got[0])
	}
}

func TestLog_OffMeansNoCommandLines(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default", func(c *ClientConfig) { c.Log = false })
	client.Set(context.Background(), "k", "v", 0)
	f.setReply("get", "-ERR boom\r\n")
	client.Get(context.Background(), "k")
	client.Pipelined(context.Background(), func(p redis.Pipeliner) error { p.Incr(context.Background(), "c"); return nil })
	for _, l := range lines() {
		if msg, _ := l["msg"].(string); strings.HasPrefix(msg, "redis ") || strings.HasPrefix(msg, "slow redis") {
			t.Errorf("Log is off by default, so there must be no command log, got=%v", l)
		}
	}
}

func TestLog_ValuesAndOtherArgsNeverLogged(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default")
	ctx := context.Background()

	val, field, hval, argv, pw, pipeVal := secret(t, "val"), secret(t, "field"), secret(t, "hval"),
		secret(t, "argv"), secret(t, "pw"), secret(t, "pipe")
	client.Set(ctx, "k:set", val, time.Minute)
	client.HSet(ctx, "k:hash", field, hval)
	client.MSet(ctx, "k:m1", val, "k:m2", hval)
	client.Eval(ctx, "return ARGV[1]", []string{"k:lua"}, argv)
	client.Eval(ctx, "return ARGV[1]", nil, argv)
	client.Do(ctx, "auth", "someone", pw)
	client.Do(ctx, "hello", "3", "auth", "someone", pw)
	client.Migrate(ctx, "10.0.0.1", "6379", "k:mig", 0, time.Millisecond)
	client.ConfigSet(ctx, "requirepass", pw)
	client.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, "k:p", pipeVal, 0)
		p.HSet(ctx, "k:ph", field, pipeVal)
		return nil
	})

	all := lines()
	for _, l := range all {
		blob := fmt.Sprint(l)
		for _, s := range []string{val, field, hval, argv, pw, pipeVal, "return ARGV", "10.0.0.1"} {
			if strings.Contains(blob, s) {
				t.Errorf("a value or another argument %q reached the log: %v", s, l)
			}
		}
	}
	// Precondition: these commands really were recorded, or the above verified nothing.
	want := map[string]string{"set": "k:set", "hset": "k:hash", "mset": "k:m1", "auth": "", "migrate": "", "config": ""}
	var evalKeys []string
	for _, l := range all {
		if l["cmd"] == "eval" {
			k, _ := l["key"].(string)
			evalKeys = append(evalKeys, k)
		}
	}
	if len(evalKeys) != 2 || evalKeys[0] != "k:lua" || evalKeys[1] != "" {
		t.Errorf("Eval records the key when it has one and none when it does not, got=%v", evalKeys)
	}
	seen := map[string]bool{}
	for _, l := range all {
		cmd, _ := l["cmd"].(string)
		if k, ok := want[cmd]; ok {
			seen[cmd] = true
			if got, _ := l["key"].(string); got != k {
				t.Errorf("%s's key must be %q, got=%v", cmd, k, l)
			}
		}
	}
	for cmd := range want {
		if !seen[cmd] {
			t.Errorf("precondition: %s must have a log line, got=%v", cmd, all)
		}
	}
}

func TestLog_NilIsNotAFailure(t *testing.T) {
	// A missing key is a normal business branch (a cache read-through happens all day) and must not be noise at WARN.
	f := newFakeRedis(t)
	f.setReply("get", "$-1\r\n")
	lines := capture(t)
	client := logClient(t, f, "default")
	if err := client.Get(context.Background(), "user:404").Err(); err != redis.Nil {
		t.Fatalf("precondition: redis.Nil must come back, got=%v", err)
	}
	got := cmdLines(lines(), "redis command", "redis command failed", "slow redis command")
	if len(got) != 1 {
		t.Fatalf("there must be one line, got=%v", lines())
	}
	l := got[0]
	if l["msg"] != "redis command" || l["level"] != "INFO" || l["nil"] != true || l["key"] != "user:404" {
		t.Errorf("redis.Nil must be an INFO redis command carrying nil=true, got=%v", l)
	}
	if _, ok := l["error"]; ok {
		t.Errorf("redis.Nil is not an error and must carry no error, got=%v", l)
	}
}

func TestLog_FailedCommandLogsOnlyErrorCode(t *testing.T) {
	// Measured on Redis 7.0.15: the text of ERR unknown command carries the arguments out;
	// and the error text of return {err=ARGV[1]} inside an EVAL is the value entirely, even in the "error code" position.
	arg := secret(t, "arg")
	for _, c := range []struct {
		name, reply, code string
	}{
		{"the error text carries the arguments", "-ERR unknown command 'foo', with args beginning with: '" + arg + "'\r\n", "ERR"},
		{"WRONGTYPE", "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n", "WRONGTYPE"},
		{"a value in the error code position", "-" + arg + "\r\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRedis(t)
			f.setReply("get", c.reply)
			lines := capture(t)
			client := logClient(t, f, "default")
			err := client.Get(context.Background(), "k:1").Err()
			if err == nil || !strings.Contains(err.Error(), strings.TrimSuffix(c.reply[1:], "\r\n")) {
				t.Fatalf("the error returned to the caller must be the original text, got=%v", err)
			}
			got := cmdLines(lines(), "redis command failed")
			if len(got) != 1 {
				t.Fatalf("a failed command must have one redis command failed line, got=%v", lines())
			}
			l := got[0]
			if l["level"] != "WARN" || l["key"] != "k:1" {
				t.Errorf("it must be a WARN carrying the key, got=%v", l)
			}
			if code, _ := l["error_code"].(string); code != c.code {
				t.Errorf("error_code must be %q, got=%v", c.code, l)
			}
			if e, _ := l["error"].(string); !strings.HasPrefix(e, "redis server error") || strings.Contains(e, arg) {
				t.Errorf("error must only say it is a server error and give the code, without the text, got=%v", l)
			}
			if strings.Contains(fmt.Sprint(lines()), arg) {
				t.Errorf("an argument reached the log through the error text: %v", lines())
			}
		})
	}
}

func TestLog_ClientSideErrorKeepsText(t *testing.T) {
	// A timeout or a failed connection is on the client side and its text holds only the address, which is exactly what an investigation needs.
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default")
	f.setStall("get")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client.Get(ctx, "k").Err()
	got := cmdLines(lines(), "redis command failed")
	if len(got) != 1 || got[0]["error"] != "context deadline exceeded" {
		t.Errorf("a timeout must be recorded verbatim, got=%v", lines())
	}
}

func TestErrorAttrs(t *testing.T) {
	// When go-redis fails to parse a reply it writes the reply content into the error (reader.go's can't parse %q).
	attrs := errorAttrs(fmt.Errorf("redis: can't parse %q", "reply-with-value"))
	if strings.Contains(fmt.Sprint(attrs), "reply-with-value") {
		t.Errorf("an unrecognized client error records no text, got=%v", attrs)
	}
	if got := fmt.Sprint(errorAttrs(redis.ErrClosed)); !strings.Contains(got, "client is closed") {
		t.Errorf("ErrClosed is recorded verbatim, got=%v", got)
	}
}

func TestLog_SlowThreshold(t *testing.T) {
	for _, c := range []struct {
		name      string
		threshold time.Duration
		wantMsg   string
	}{
		{"above the threshold is a WARN", 20 * time.Millisecond, "slow redis command"},
		{"0 records no slow commands", 0, "redis command"},
		{"below the threshold is INFO as usual", time.Second, "redis command"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRedis(t)
			f.setDelay("get", 60*time.Millisecond)
			lines := capture(t)
			client := logClient(t, f, "default", func(cc *ClientConfig) { cc.SlowThreshold = c.threshold })
			client.Get(context.Background(), "k:slow")
			got := cmdLines(lines(), "redis command", "slow redis command")
			if len(got) != 1 || got[0]["msg"] != c.wantMsg {
				t.Fatalf("it must be one %s line, got=%v", c.wantMsg, lines())
			}
			if c.wantMsg == "slow redis command" {
				if got[0]["level"] != "WARN" || got[0]["threshold_ms"] != float64(20) || got[0]["key"] != "k:slow" {
					t.Errorf("a slow command must be a WARN carrying threshold_ms and the key, got=%v", got[0])
				}
			}
		})
	}
}

func TestLog_SlowNilStillMarksNil(t *testing.T) {
	f := newFakeRedis(t)
	f.setReply("get", "$-1\r\n")
	f.setDelay("get", 40*time.Millisecond)
	lines := capture(t)
	client := logClient(t, f, "default", func(c *ClientConfig) { c.SlowThreshold = 10 * time.Millisecond })
	client.Get(context.Background(), "k")
	got := cmdLines(lines(), "slow redis command")
	if len(got) != 1 || got[0]["nil"] != true {
		t.Errorf("a slow nil must carry nil=true too, got=%v", lines())
	}
}

func TestLog_LongKeyTruncated(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default")
	long := strings.Repeat("k", 300)
	client.Get(context.Background(), long)
	got := cmdLines(lines(), "redis command")
	if len(got) != 1 {
		t.Fatalf("got=%v", lines())
	}
	if k, _ := got[0]["key"].(string); len(k) != maxLoggedKey || got[0]["key_truncated"] != true {
		t.Errorf("an overlong key must be cut to %d bytes and carry key_truncated, got=%v", maxLoggedKey, got[0])
	}
}

func TestKeyAttrs_DoesNotSplitUTF8(t *testing.T) {
	key := strings.Repeat("a", maxLoggedKey-1) + "€" + "tail"
	got := keyAttrs(key)[1].(string)
	if !strings.HasSuffix(got, "a") || len(got) != maxLoggedKey-1 {
		t.Errorf("truncation must not cut a UTF-8 character, got tail %q, length %d", got[len(got)-3:], len(got))
	}
}

func TestFirstKey(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		args []any
		want string // an empty string means no key may be given
	}{
		{[]any{"get", "user:1"}, "user:1"},
		{[]any{"set", "user:1", "value", "ex", 10}, "user:1"},
		{[]any{"HSET", "h:1", "f", "v"}, "h:1"},
		{[]any{"mget", "a", "b"}, "a"},
		{[]any{"eval", "return 1", "1", "lua:k", "argv"}, "lua:k"},
		{[]any{"evalsha", "abc", "2", "k1", "k2", "argv"}, "k1"},
		{[]any{"eval", "return 1", "0", "argv-not-a-key"}, ""},
		{[]any{"eval", "return 1", 1, "lua:int"}, "lua:int"}, // go-redis's Eval passes numkeys as an int
		{[]any{"eval", "return 1", 0, "argv-not-a-key"}, ""},
		{[]any{"eval", "return 1", "x", "argv-not-a-key"}, ""},
		{[]any{"fcall", "fn", "1", "fk", "argv"}, "fk"},
		{[]any{"xread", "count", "1", "block", "0", "streams", "s1", "s2", "0", "0"}, "s1"},
		{[]any{"xreadgroup", "group", "streams", "streams", "count", "1", "streams", "s1", ">"}, "s1"},
		{[]any{"xread", "count", "1"}, ""},
		{[]any{"auth", "user", "pw"}, ""},
		{[]any{"hello", "3", "auth", "user", "pw"}, ""},
		{[]any{"migrate", "10.0.0.1", "6379", "k", "0", "5", "auth", "pw"}, ""},
		{[]any{"config", "set", "requirepass", "pw"}, ""},
		{[]any{"ping"}, ""},
		{[]any{"info", "server"}, ""},
		{[]any{"select", 1}, ""},
		{[]any{"client", "setname", "x"}, ""},
		{[]any{"script", "load", "return 1"}, ""},
		{[]any{"keys", "user:*"}, ""},
		{[]any{"get", 42}, ""}, // a non-string argument is not guessed at
		{[]any{"get", []byte("b:1")}, "b:1"},
		{[]any{"get"}, ""},
	} {
		got, ok := firstKey(redis.NewCmd(ctx, c.args...))
		if got != c.want || ok != (c.want != "") {
			t.Errorf("firstKey(%v) = %q, %v; want %q", c.args, got, ok, c.want)
		}
	}
}

func TestLog_PipelineOneLine(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default")
	ctx := context.Background()
	val := secret(t, "pipe")
	_, err := client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i := 0; i < 12; i++ {
			p.Set(ctx, fmt.Sprintf("p:%d", i), val, 0)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all := lines()
	if n := len(cmdLines(all, "redis command")); n != 0 {
		t.Errorf("the commands in a pipeline must not each get a line, got %d lines", n)
	}
	got := cmdLines(all, "redis pipeline")
	if len(got) != 1 {
		t.Fatalf("one line for the whole pipeline, got=%v", all)
	}
	l := got[0]
	cmds, _ := l["cmds"].([]any)
	if l["level"] != "INFO" || l["name"] != "default" || l["count"] != float64(12) || len(cmds) != maxLoggedPipelineCmds || cmds[0] != "set" {
		t.Errorf("it must carry the total of 12 and the first %d command names, got=%v", maxLoggedPipelineCmds, l)
	}
	if _, ok := l["elapsed_ms"].(float64); !ok {
		t.Errorf("it must carry elapsed_ms, got=%v", l)
	}
	if strings.Contains(fmt.Sprint(all), val) || strings.Contains(fmt.Sprint(all), "p:1") {
		t.Errorf("a pipeline log carries no key and no value: %v", all)
	}
}

func TestLog_PipelineNilIsNotFailureButLaterErrorIs(t *testing.T) {
	ctx := context.Background()

	f := newFakeRedis(t)
	f.setReply("get", "$-1\r\n")
	lines := capture(t)
	client := logClient(t, f, "default")
	if _, err := client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Get(ctx, "a"); p.Set(ctx, "b", "v", 0); return nil }); err != redis.Nil {
		t.Fatalf("precondition: Exec returns redis.Nil when one is present, got=%v", err)
	}
	if got := cmdLines(lines(), "redis pipeline"); len(got) != 1 || got[0]["level"] != "INFO" {
		t.Errorf("a pipeline with nothing but a nil is not a failure, got=%v", lines())
	}

	f2 := newFakeRedis(t)
	f2.setReply("get", "$-1\r\n")
	f2.setReply("incr", "-ERR value is not an integer or out of range\r\n")
	lines2 := capture(t)
	client2 := logClient(t, f2, "default")
	client2.Pipelined(ctx, func(p redis.Pipeliner) error { p.Get(ctx, "a"); p.Incr(ctx, "b"); return nil })
	got := cmdLines(lines2(), "redis pipeline failed")
	if len(got) != 1 || got[0]["level"] != "WARN" || got[0]["error_code"] != "ERR" {
		t.Errorf("a real error after a nil must be recognized, got=%v", lines2())
	}
}

func TestLog_SlowPipeline(t *testing.T) {
	f := newFakeRedis(t)
	f.setDelay("set", 40*time.Millisecond)
	lines := capture(t)
	client := logClient(t, f, "default", func(c *ClientConfig) { c.SlowThreshold = 10 * time.Millisecond })
	ctx := context.Background()
	client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, "a", "v", 0); return nil })
	if got := cmdLines(lines(), "slow redis pipeline"); len(got) != 1 || got[0]["threshold_ms"] != float64(10) {
		t.Errorf("a slow pipeline must have one slow redis pipeline line, got=%v", lines())
	}
}

// idTracer gives every Span a new span_id while keeping the parent's trace_id:
// the test uses that to tell whether the log's ctx holds the caller's Span or the redis command's.
type idTracer struct {
	noop.Tracer
	mu    *sync.Mutex
	spans map[string]trace.SpanContext
	next  *byte
}

func (t idTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	t.mu.Lock()
	*t.next++
	sid := trace.SpanID{0, 0, 0, 0, 0, 0, 0, *t.next}
	tid := trace.SpanContextFromContext(ctx).TraceID()
	if !tid.IsValid() {
		tid = trace.TraceID{1, *t.next}
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	t.spans[name] = sc
	t.mu.Unlock()
	return t.Tracer.Start(trace.ContextWithSpanContext(ctx, sc), name, opts...)
}

type idTP struct {
	noop.TracerProvider
	t idTracer
}

func (p idTP) Tracer(string, ...trace.TracerOption) trace.Tracer { return p.t }

func TestLog_CarriesRedisSpanOfCallerTrace(t *testing.T) {
	// The log hook runs inside the tracing hook: the command ctx holds the redis Span, with the same trace_id as the caller.
	var n byte
	tp := idTP{t: idTracer{mu: &sync.Mutex{}, spans: map[string]trace.SpanContext{}, next: &n}}
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	var recs []trace.SpanContext
	var mu sync.Mutex
	oldLog := slog.Default()
	slog.SetDefault(slog.New(scRecorder{mu: &mu, recs: &recs}))
	t.Cleanup(func() { slog.SetDefault(oldLog) })

	f := newFakeRedis(t)
	client := logClient(t, f, "default")
	ctx, _ := tp.t.Start(context.Background(), "caller")
	caller := trace.SpanContextFromContext(ctx)
	if err := client.Get(ctx, "k").Err(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	tp.t.mu.Lock()
	cmdSpan, ok := tp.t.spans["get"]
	tp.t.mu.Unlock()
	if !ok || len(recs) != 1 {
		t.Fatalf("precondition: there must be one get Span and one command log line, spans=%v logs=%v", tp.t.spans, recs)
	}
	if recs[0].TraceID() != caller.TraceID() {
		t.Errorf("the log must carry the caller's trace_id %s, got=%s", caller.TraceID(), recs[0].TraceID())
	}
	if recs[0].SpanID() != cmdSpan.SpanID() {
		t.Errorf("the log's span_id must be this command's redis Span %s (the caller's is %s), got=%s", cmdSpan.SpanID(), caller.SpanID(), recs[0].SpanID())
	}
}

// scRecorder records the SpanContext in the ctx of every redis command log line.
type scRecorder struct {
	mu   *sync.Mutex
	recs *[]trace.SpanContext
}

func (h scRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h scRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h scRecorder) WithGroup(string) slog.Handler            { return h }
func (h scRecorder) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "redis command" {
		h.mu.Lock()
		*h.recs = append(*h.recs, trace.SpanContextFromContext(ctx))
		h.mu.Unlock()
	}
	return nil
}

func TestLog_InvalidCmdNameReplaced(t *testing.T) {
	// The cmd field is go-redis's cmd.Name(): the first argument lowercased as it is, untruncated.
	// Do(ctx, "SET k1 <value>") puts the whole command into the first argument, so the value travels into cmd with it.
	val := secret(t, "val")
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default")
	ctx := context.Background()
	client.Do(ctx, "SET k1 "+val)
	client.Do(ctx, "json.set", "doc:1", "$", "{}")
	client.Do(ctx, strings.Repeat("a", 65))
	client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Do(ctx, "SET k2 "+val); p.Get(ctx, "a"); return nil })

	var cmds []any
	for _, l := range cmdLines(lines(), "redis command") {
		cmds = append(cmds, l["cmd"])
	}
	if fmt.Sprint(cmds) != "[<invalid> json.set <invalid>]" {
		t.Errorf("a cmd that does not look like a command name must become <invalid>, while a module command (json.set) is recorded, got=%v", cmds)
	}
	p := cmdLines(lines(), "redis pipeline")
	if len(p) != 1 || fmt.Sprint(p[0]["cmds"]) != "[<invalid> get]" {
		t.Errorf("a pipeline's cmds likewise become <invalid>, got=%v", p)
	}
	if strings.Contains(strings.ToLower(fmt.Sprint(lines())), strings.ToLower(val)) {
		t.Errorf("a value reached the log through the command name: %v", lines())
	}
}

func TestLog_WatchConflictIsNotAFailure(t *testing.T) {
	// A WATCHed key changed by somebody else makes EXEC answer an empty array and go-redis return redis.TxFailedErr.
	// It is a proto.RedisError (the same type as a server error), but it is the normal outcome of optimistic locking, not a failure.
	f := newFakeRedis(t)
	f.setReply("exec", "*-1\r\n")
	lines := capture(t)
	client := logClient(t, f, "default")
	ctx := context.Background()
	_, err := client.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, "w", "v", 0); return nil })
	if !errors.Is(err, redis.TxFailedErr) {
		t.Fatalf("precondition: an empty array from EXEC returns redis.TxFailedErr, got=%v", err)
	}
	got := cmdLines(lines(), "redis pipeline", "redis pipeline failed", "slow redis pipeline")
	if len(got) != 1 || got[0]["msg"] != "redis pipeline" || got[0]["level"] != "INFO" || got[0]["tx_failed"] != true {
		t.Fatalf("a WATCH conflict must be an INFO redis pipeline carrying tx_failed=true, got=%v", lines())
	}
	if _, ok := got[0]["error"]; ok {
		t.Errorf("a WATCH conflict carries no error, got=%v", got[0])
	}
}

func TestLog_WatchConflictSlowStillMarked(t *testing.T) {
	f := newFakeRedis(t)
	f.setReply("exec", "*-1\r\n")
	f.setDelay("exec", 40*time.Millisecond)
	lines := capture(t)
	client := logClient(t, f, "default", func(c *ClientConfig) { c.SlowThreshold = 10 * time.Millisecond })
	ctx := context.Background()
	client.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, "w", "v", 0); return nil })
	if got := cmdLines(lines(), "slow redis pipeline"); len(got) != 1 || got[0]["tx_failed"] != true {
		t.Errorf("a slow WATCH conflict is a slow redis pipeline and carries tx_failed=true too, got=%v", lines())
	}
}

func TestKeyAttrs_InvalidUTF8NotTruncatedToEmpty(t *testing.T) {
	// A run of 0x80: no byte is a character boundary, and walking back forever would reach 0.
	got := keyAttrs(strings.Repeat("\x80", 300))
	if k := got[1].(string); len(k) != maxLoggedKey || got[3] != true {
		t.Errorf("a non-UTF-8 key is cut to %d bytes and must not become an empty string, got length %d", maxLoggedKey, len(k))
	}
}

func TestLog_WarnLevelHandlerStillGetsProblems(t *testing.T) {
	// With slog accepting WARN only, the shortcut that skips assembling the fields must not skip failures, slow commands and failed pipelines too.
	f := newFakeRedis(t)
	f.setReply("incr", "-ERR value is not an integer or out of range\r\n")
	f.setDelay("get", 40*time.Millisecond)
	lines := captureAt(t, slog.LevelWarn)
	client := logClient(t, f, "default", func(c *ClientConfig) { c.SlowThreshold = 10 * time.Millisecond })
	ctx := context.Background()
	client.Set(ctx, "ok", "v", 0)
	client.Incr(ctx, "n")
	client.Get(ctx, "slow")
	client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Incr(ctx, "n"); return nil })
	client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Get(ctx, "slow"); return nil })
	var msgs []any
	for _, l := range cmdLines(lines(), "redis command", "redis command failed", "slow redis command",
		"redis pipeline", "redis pipeline failed", "slow redis pipeline") {
		msgs = append(msgs, l["msg"])
	}
	if fmt.Sprint(msgs) != "[redis command failed slow redis command redis pipeline failed slow redis pipeline]" {
		t.Errorf("a WARN-only handler must still get the failure, slow and pipeline-failure lines, got=%v", msgs)
	}
}

func TestLog_PipelineReportsFirstError(t *testing.T) {
	// The first one is a nil: Exec returns redis.Nil, so the real error has to be looked for after it, and what is found is the first one.
	f := newFakeRedis(t)
	f.setReply("get", "$-1\r\n")
	f.setReply("incr", "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n")
	f.setReply("decr", "-ERR value is not an integer or out of range\r\n")
	lines := capture(t)
	client := logClient(t, f, "default")
	ctx := context.Background()
	client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Get(ctx, "a"); p.Incr(ctx, "b"); p.Decr(ctx, "c"); return nil })
	got := cmdLines(lines(), "redis pipeline failed")
	if len(got) != 1 || got[0]["error_code"] != "WRONGTYPE" {
		t.Errorf("a pipeline records the first error (WRONGTYPE), not the last, got=%v", lines())
	}
}

func TestLog_SlowAndFailedLogsFailed(t *testing.T) {
	f := newFakeRedis(t)
	f.setReply("get", "-ERR boom\r\n")
	f.setDelay("get", 40*time.Millisecond)
	f.setReply("incr", "-ERR boom\r\n")
	f.setDelay("incr", 40*time.Millisecond)
	lines := capture(t)
	client := logClient(t, f, "default", func(c *ClientConfig) { c.SlowThreshold = 10 * time.Millisecond })
	ctx := context.Background()
	client.Get(ctx, "k")
	client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Incr(ctx, "n"); return nil })
	if got := cmdLines(lines(), "redis command failed"); len(got) != 1 || got[0]["error_code"] != "ERR" {
		t.Errorf("a slow and failed command is a redis command failed carrying the error, got=%v", lines())
	}
	if got := cmdLines(lines(), "redis pipeline failed"); len(got) != 1 || got[0]["error_code"] != "ERR" {
		t.Errorf("a slow and failed pipeline is a redis pipeline failed carrying the error, got=%v", lines())
	}
	if got := cmdLines(lines(), "slow redis command", "slow redis pipeline"); len(got) != 0 {
		t.Errorf("failure wins and nothing is additionally recorded as slow, got=%v", got)
	}
}

func TestLog_ContextCanceledKeepsText(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.Get(ctx, "k")
	got := cmdLines(lines(), "redis command failed")
	if len(got) != 1 || got[0]["error"] != "context canceled" {
		t.Errorf("a cancelled ctx is recorded verbatim, got=%v", lines())
	}
}

func TestMs_KeepsSubMillisecond(t *testing.T) {
	if got := ms(1500 * time.Microsecond); got != 1.5 {
		t.Errorf("ms(1.5ms) = %v, want 1.5", got)
	}
	if got := ms(250 * time.Microsecond); got != 0.25 {
		t.Errorf("ms(250µs) = %v, want 0.25", got)
	}
}
