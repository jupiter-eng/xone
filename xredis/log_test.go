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

// secret 一个长随机串：「日志里不该出现」的检查用它，短的数字会和时间戳、耗时撞上
func secret(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// logClient 建一个 Log 开着的实例，实例名 name
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

// cmdLines 取 msg 为这几种之一的日志，去掉每条新连接上的 HELLO（假 Redis 不认它，每条连接回一次错）
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
		t.Fatalf("一条命令一行 redis command，got=%v", lines())
	}
	l := got[0]
	if l["level"] != "INFO" || l["name"] != "session" || l["cmd"] != "set" || l["key"] != "session:42" {
		t.Errorf("该带实例名、命令名、第一个 key，级别 INFO，got=%v", l)
	}
	if _, ok := l["elapsed_ms"].(float64); !ok {
		t.Errorf("该带 elapsed_ms（毫秒，数字），got=%v", l)
	}
	for _, k := range []string{"error", "nil", "key_truncated", "threshold_ms"} {
		if _, ok := l[k]; ok {
			t.Errorf("成功、非 nil、短 key、不慢的命令不该带 %s，got=%v", k, l)
		}
	}
}

func TestLog_InstanceNameFromConfigReachesLog(t *testing.T) {
	// 打在调用点上：框架建实例时要把配置里的名字交下去，否则多实例时分不出是哪个
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
		t.Errorf("日志的 name 该是配置里的实例名 cache，got=%v", got)
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
		t.Fatalf("直接调 New、Log 开着也该记，got=%v", lines())
	}
	if _, ok := got[0]["name"]; ok {
		t.Errorf("直接调 New 没有实例名，不该写一个空的 name，got=%v", got[0])
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
			t.Errorf("Log 默认关着，不该有命令日志，got=%v", l)
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
				t.Errorf("日志里出现了值或其余参数 %q：%v", s, l)
			}
		}
	}
	// 前提：这些命令确实都记了，否则上面什么都没验
	want := map[string]string{"set": "k:set", "hset": "k:hash", "mset": "k:m1", "auth": "", "migrate": "", "config": ""}
	var evalKeys []string
	for _, l := range all {
		if l["cmd"] == "eval" {
			k, _ := l["key"].(string)
			evalKeys = append(evalKeys, k)
		}
	}
	if len(evalKeys) != 2 || evalKeys[0] != "k:lua" || evalKeys[1] != "" {
		t.Errorf("Eval 带 key 时记 key、不带时不记，got=%v", evalKeys)
	}
	seen := map[string]bool{}
	for _, l := range all {
		cmd, _ := l["cmd"].(string)
		if k, ok := want[cmd]; ok {
			seen[cmd] = true
			if got, _ := l["key"].(string); got != k {
				t.Errorf("%s 的 key 该是 %q，got=%v", cmd, k, l)
			}
		}
	}
	for cmd := range want {
		if !seen[cmd] {
			t.Errorf("前提：%s 该有一行日志，got=%v", cmd, all)
		}
	}
}

func TestLog_NilIsNotAFailure(t *testing.T) {
	// key 不存在是正常的业务分支（读穿缓存天天走），不该刷成 WARN
	f := newFakeRedis(t)
	f.setReply("get", "$-1\r\n")
	lines := capture(t)
	client := logClient(t, f, "default")
	if err := client.Get(context.Background(), "user:404").Err(); err != redis.Nil {
		t.Fatalf("前提：该拿到 redis.Nil，got=%v", err)
	}
	got := cmdLines(lines(), "redis command", "redis command failed", "slow redis command")
	if len(got) != 1 {
		t.Fatalf("该有一行，got=%v", lines())
	}
	l := got[0]
	if l["msg"] != "redis command" || l["level"] != "INFO" || l["nil"] != true || l["key"] != "user:404" {
		t.Errorf("redis.Nil 该记成 INFO 的 redis command、带 nil=true，got=%v", l)
	}
	if _, ok := l["error"]; ok {
		t.Errorf("redis.Nil 不是错误，不该带 error，got=%v", l)
	}
}

func TestLog_FailedCommandLogsOnlyErrorCode(t *testing.T) {
	// 实测 Redis 7.0.15：ERR unknown command 的原文把参数带出来；
	// EVAL 里 return {err=ARGV[1]} 的错误原文整个就是值，连「错误码」的位置都是
	arg := secret(t, "arg")
	for _, c := range []struct {
		name, reply, code string
	}{
		{"错误原文带着参数", "-ERR unknown command 'foo', with args beginning with: '" + arg + "'\r\n", "ERR"},
		{"WRONGTYPE", "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n", "WRONGTYPE"},
		{"错误码位置上是值", "-" + arg + "\r\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRedis(t)
			f.setReply("get", c.reply)
			lines := capture(t)
			client := logClient(t, f, "default")
			err := client.Get(context.Background(), "k:1").Err()
			if err == nil || !strings.Contains(err.Error(), strings.TrimSuffix(c.reply[1:], "\r\n")) {
				t.Fatalf("返回给调用方的错误该是原文，got=%v", err)
			}
			got := cmdLines(lines(), "redis command failed")
			if len(got) != 1 {
				t.Fatalf("失败的命令该有一行 redis command failed，got=%v", lines())
			}
			l := got[0]
			if l["level"] != "WARN" || l["key"] != "k:1" {
				t.Errorf("该是 WARN、带 key，got=%v", l)
			}
			if code, _ := l["error_code"].(string); code != c.code {
				t.Errorf("error_code 该是 %q，got=%v", c.code, l)
			}
			if e, _ := l["error"].(string); !strings.HasPrefix(e, "redis server error") || strings.Contains(e, arg) {
				t.Errorf("error 只该说是服务端的错和错误码，不带原文，got=%v", l)
			}
			if strings.Contains(fmt.Sprint(lines()), arg) {
				t.Errorf("参数从错误原文进了日志：%v", lines())
			}
		})
	}
}

func TestLog_ClientSideErrorKeepsText(t *testing.T) {
	// 超时、连不上这类是客户端这一侧的，原文里只有地址，排查正要看它
	f := newFakeRedis(t)
	lines := capture(t)
	client := logClient(t, f, "default")
	f.setStall("get")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client.Get(ctx, "k").Err()
	got := cmdLines(lines(), "redis command failed")
	if len(got) != 1 || got[0]["error"] != "context deadline exceeded" {
		t.Errorf("超时该照原文记，got=%v", lines())
	}
}

func TestErrorAttrs(t *testing.T) {
	// go-redis 解析回复失败时把回复内容写进错误（reader.go 的 can't parse %q）
	attrs := errorAttrs(fmt.Errorf("redis: can't parse %q", "reply-with-value"))
	if strings.Contains(fmt.Sprint(attrs), "reply-with-value") {
		t.Errorf("不认得的客户端错误不记原文，got=%v", attrs)
	}
	if got := fmt.Sprint(errorAttrs(redis.ErrClosed)); !strings.Contains(got, "client is closed") {
		t.Errorf("ErrClosed 照原文，got=%v", got)
	}
}

func TestLog_SlowThreshold(t *testing.T) {
	for _, c := range []struct {
		name      string
		threshold time.Duration
		wantMsg   string
	}{
		{"超过阈值记 WARN", 20 * time.Millisecond, "slow redis command"},
		{"配 0 不记慢命令", 0, "redis command"},
		{"没超过阈值照常 INFO", time.Second, "redis command"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRedis(t)
			f.setDelay("get", 60*time.Millisecond)
			lines := capture(t)
			client := logClient(t, f, "default", func(cc *ClientConfig) { cc.SlowThreshold = c.threshold })
			client.Get(context.Background(), "k:slow")
			got := cmdLines(lines(), "redis command", "slow redis command")
			if len(got) != 1 || got[0]["msg"] != c.wantMsg {
				t.Fatalf("该是一行 %s，got=%v", c.wantMsg, lines())
			}
			if c.wantMsg == "slow redis command" {
				if got[0]["level"] != "WARN" || got[0]["threshold_ms"] != float64(20) || got[0]["key"] != "k:slow" {
					t.Errorf("慢命令该是 WARN、带 threshold_ms 和 key，got=%v", got[0])
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
		t.Errorf("慢的 nil 也该带 nil=true，got=%v", lines())
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
		t.Errorf("超长的 key 该截到 %d 字节并带 key_truncated，got=%v", maxLoggedKey, got[0])
	}
}

func TestKeyAttrs_DoesNotSplitUTF8(t *testing.T) {
	key := strings.Repeat("a", maxLoggedKey-1) + "键" + "tail"
	got := keyAttrs(key)[1].(string)
	if !strings.HasSuffix(got, "a") || len(got) != maxLoggedKey-1 {
		t.Errorf("截断不该切开一个 UTF-8 字符，got 尾部 %q、长度 %d", got[len(got)-3:], len(got))
	}
}

func TestFirstKey(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		args []any
		want string // 空串表示不该给 key
	}{
		{[]any{"get", "user:1"}, "user:1"},
		{[]any{"set", "user:1", "value", "ex", 10}, "user:1"},
		{[]any{"HSET", "h:1", "f", "v"}, "h:1"},
		{[]any{"mget", "a", "b"}, "a"},
		{[]any{"eval", "return 1", "1", "lua:k", "argv"}, "lua:k"},
		{[]any{"evalsha", "abc", "2", "k1", "k2", "argv"}, "k1"},
		{[]any{"eval", "return 1", "0", "argv-not-a-key"}, ""},
		{[]any{"eval", "return 1", 1, "lua:int"}, "lua:int"}, // go-redis 的 Eval 传的 numkeys 是 int
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
		{[]any{"get", 42}, ""}, // 不是字符串的参数不猜
		{[]any{"get", []byte("b:1")}, "b:1"},
		{[]any{"get"}, ""},
	} {
		got, ok := firstKey(redis.NewCmd(ctx, c.args...))
		if got != c.want || ok != (c.want != "") {
			t.Errorf("firstKey(%v) = %q, %v；want %q", c.args, got, ok, c.want)
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
		t.Errorf("pipeline 里的命令不该各记一行，got %d 行", n)
	}
	got := cmdLines(all, "redis pipeline")
	if len(got) != 1 {
		t.Fatalf("整个 pipeline 一行，got=%v", all)
	}
	l := got[0]
	cmds, _ := l["cmds"].([]any)
	if l["level"] != "INFO" || l["name"] != "default" || l["count"] != float64(12) || len(cmds) != maxLoggedPipelineCmds || cmds[0] != "set" {
		t.Errorf("该带总数 12、前 %d 个命令名，got=%v", maxLoggedPipelineCmds, l)
	}
	if _, ok := l["elapsed_ms"].(float64); !ok {
		t.Errorf("该带 elapsed_ms，got=%v", l)
	}
	if strings.Contains(fmt.Sprint(all), val) || strings.Contains(fmt.Sprint(all), "p:1") {
		t.Errorf("pipeline 的日志不带 key 和值：%v", all)
	}
}

func TestLog_PipelineNilIsNotFailureButLaterErrorIs(t *testing.T) {
	ctx := context.Background()

	f := newFakeRedis(t)
	f.setReply("get", "$-1\r\n")
	lines := capture(t)
	client := logClient(t, f, "default")
	if _, err := client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Get(ctx, "a"); p.Set(ctx, "b", "v", 0); return nil }); err != redis.Nil {
		t.Fatalf("前提：Exec 在有 nil 时返回 redis.Nil，got=%v", err)
	}
	if got := cmdLines(lines(), "redis pipeline"); len(got) != 1 || got[0]["level"] != "INFO" {
		t.Errorf("只有 nil 的 pipeline 不算失败，got=%v", lines())
	}

	f2 := newFakeRedis(t)
	f2.setReply("get", "$-1\r\n")
	f2.setReply("incr", "-ERR value is not an integer or out of range\r\n")
	lines2 := capture(t)
	client2 := logClient(t, f2, "default")
	client2.Pipelined(ctx, func(p redis.Pipeliner) error { p.Get(ctx, "a"); p.Incr(ctx, "b"); return nil })
	got := cmdLines(lines2(), "redis pipeline failed")
	if len(got) != 1 || got[0]["level"] != "WARN" || got[0]["error_code"] != "ERR" {
		t.Errorf("nil 之后的真错误要认出来，got=%v", lines2())
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
		t.Errorf("慢 pipeline 该有一行 slow redis pipeline，got=%v", lines())
	}
}

// idTracer 给每个 Span 发一个新的 span_id，trace_id 沿用父的：
// 测试据此分得清日志的 ctx 里是调用方的 Span 还是 redis 命令的 Span
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
	// 日志钩子挂在链路钩子里面：命令 ctx 里是 redis Span，trace_id 与调用方相同
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
		t.Fatalf("前提：该有一个 get Span 和一行命令日志，spans=%v logs=%v", tp.t.spans, recs)
	}
	if recs[0].TraceID() != caller.TraceID() {
		t.Errorf("日志该带调用方的 trace_id %s，got=%s", caller.TraceID(), recs[0].TraceID())
	}
	if recs[0].SpanID() != cmdSpan.SpanID() {
		t.Errorf("日志的 span_id 该是这条命令的 redis Span %s（调用方是 %s），got=%s", cmdSpan.SpanID(), caller.SpanID(), recs[0].SpanID())
	}
}

// scRecorder 记下每条 redis command 日志的 ctx 里的 SpanContext
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
	// cmd 字段是 go-redis 的 cmd.Name()：第 1 个参数原样小写，不截断。
	// Do(ctx, "SET k1 <值>") 把整条命令塞进了第 1 个参数，值就跟着进了 cmd
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
		t.Errorf("不像命令名的 cmd 该换成 <invalid>，模块命令（json.set）照记，got=%v", cmds)
	}
	p := cmdLines(lines(), "redis pipeline")
	if len(p) != 1 || fmt.Sprint(p[0]["cmds"]) != "[<invalid> get]" {
		t.Errorf("pipeline 的 cmds 同样换成 <invalid>，got=%v", p)
	}
	if strings.Contains(strings.ToLower(fmt.Sprint(lines())), strings.ToLower(val)) {
		t.Errorf("值从命令名进了日志：%v", lines())
	}
}

func TestLog_WatchConflictIsNotAFailure(t *testing.T) {
	// WATCH 的 key 被别人改了，EXEC 回空数组，go-redis 返回 redis.TxFailedErr。
	// 它是 proto.RedisError（和服务端的错误同一个类型），但它是乐观锁的正常结果，不是故障
	f := newFakeRedis(t)
	f.setReply("exec", "*-1\r\n")
	lines := capture(t)
	client := logClient(t, f, "default")
	ctx := context.Background()
	_, err := client.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, "w", "v", 0); return nil })
	if !errors.Is(err, redis.TxFailedErr) {
		t.Fatalf("前提：EXEC 回空数组时返回 redis.TxFailedErr，got=%v", err)
	}
	got := cmdLines(lines(), "redis pipeline", "redis pipeline failed", "slow redis pipeline")
	if len(got) != 1 || got[0]["msg"] != "redis pipeline" || got[0]["level"] != "INFO" || got[0]["tx_failed"] != true {
		t.Fatalf("WATCH 冲突该记 INFO 的 redis pipeline、带 tx_failed=true，got=%v", lines())
	}
	if _, ok := got[0]["error"]; ok {
		t.Errorf("WATCH 冲突不带 error，got=%v", got[0])
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
		t.Errorf("慢的 WATCH 冲突记 slow redis pipeline、也带 tx_failed=true，got=%v", lines())
	}
}

func TestKeyAttrs_InvalidUTF8NotTruncatedToEmpty(t *testing.T) {
	// 一串 0x80：每个字节都不是字符的起点，一路往回找会退到 0
	got := keyAttrs(strings.Repeat("\x80", 300))
	if k := got[1].(string); len(k) != maxLoggedKey || got[3] != true {
		t.Errorf("不是 UTF-8 的 key 截到 %d 字节，不该截成空串，got 长度 %d", maxLoggedKey, len(k))
	}
}

func TestLog_WarnLevelHandlerStillGetsProblems(t *testing.T) {
	// slog 只收 WARN 时跳过拼字段的捷径，不能把失败、慢、pipeline 失败也跳过
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
		t.Errorf("只收 WARN 的 handler 照样该拿到失败、慢、pipeline 失败的行，got=%v", msgs)
	}
}

func TestLog_PipelineReportsFirstError(t *testing.T) {
	// 头一条是 nil：Exec 返回 redis.Nil，真错误要自己往后找，找到的是第一个
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
		t.Errorf("pipeline 记第一个错误（WRONGTYPE），不是最后一个，got=%v", lines())
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
		t.Errorf("又慢又失败的命令记 redis command failed、带错误，got=%v", lines())
	}
	if got := cmdLines(lines(), "redis pipeline failed"); len(got) != 1 || got[0]["error_code"] != "ERR" {
		t.Errorf("又慢又失败的 pipeline 记 redis pipeline failed、带错误，got=%v", lines())
	}
	if got := cmdLines(lines(), "slow redis command", "slow redis pipeline"); len(got) != 0 {
		t.Errorf("失败优先，不再另记慢，got=%v", got)
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
		t.Errorf("ctx 取消照原文记，got=%v", lines())
	}
}

func TestMs_KeepsSubMillisecond(t *testing.T) {
	if got := ms(1500 * time.Microsecond); got != 1.5 {
		t.Errorf("ms(1.5ms) = %v，want 1.5", got)
	}
	if got := ms(250 * time.Microsecond); got != 0.25 {
		t.Errorf("ms(250µs) = %v，want 0.25", got)
	}
}
