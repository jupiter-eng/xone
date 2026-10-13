package xredis

import (
	"cmp"
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/redis/go-redis/v9"
)

// recordingTP 只记下每个 Span 开始时带的属性。
//
// 不用 otel 的 sdk + tracetest：测试依赖会被 go mod tidy 记成直接依赖，
// 跟着进每个使用者的模块图（理由同 fakeRedis）。redisotel 的属性都是在
// Start 时一次给齐的，记这一处就够了。
type recordingTP struct {
	noop.TracerProvider
	mu    sync.Mutex
	attrs []attribute.KeyValue
	names []string
	// errs 每个 Span 上 RecordError 记下的原文（exception.message）、SetAttributes 补上的属性、
	// SetStatus 的状态描述，按调用顺序拼成的文本
	errs []string
}

func (p *recordingTP) Tracer(string, ...trace.TracerOption) trace.Tracer {
	return recordingTracer{p: p}
}

func (p *recordingTP) spans() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.names...)
}

func (p *recordingTP) all() []attribute.KeyValue {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]attribute.KeyValue(nil), p.attrs...)
}

type recordingTracer struct {
	noop.Tracer
	p *recordingTP
}

func (t recordingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	attrs := cfg.Attributes()
	t.p.mu.Lock()
	t.p.attrs = append(t.p.attrs, attrs...)
	t.p.names = append(t.p.names, name)
	t.p.mu.Unlock()
	ctx, span := t.Tracer.Start(ctx, name, opts...)
	return ctx, recordingSpan{Span: span, p: t.p}
}

// recordingSpan 记下落在 Span 上的错误文本：exception.message、状态描述和后补的属性
type recordingSpan struct {
	trace.Span
	p *recordingTP
}

func (s recordingSpan) record(text string) {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	s.p.errs = append(s.p.errs, text)
}

func (s recordingSpan) RecordError(err error, _ ...trace.EventOption) {
	s.record("exception.message=" + err.Error())
}

func (s recordingSpan) SetStatus(code codes.Code, desc string) {
	s.record("status=" + code.String() + ":" + desc)
}

func (s recordingSpan) SetAttributes(kv ...attribute.KeyValue) {
	for _, a := range kv {
		s.record(string(a.Key) + "=" + a.Value.Emit())
	}
}

func (p *recordingTP) errors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.errs...)
}

func TestTrace_CommandArgsNotInSpan(t *testing.T) {
	// redisotel v9.22.0 默认 dbStmtEnabled=true，把整条命令连同参数写进
	// db.statement：SET 的值、AUTH 的密码，统统进了链路后端。
	// 与 xgorm 的「不记 Statement.Vars」是同一条原则
	tp := &recordingTP{}
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	f := newFakeRedis(t)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	const secret = "s3cr3t-value"
	if err := client.Set(context.Background(), "session:1", secret, 0).Err(); err != nil {
		t.Fatal(err)
	}

	var instrumented bool
	for _, a := range tp.all() {
		if strings.Contains(a.Value.Emit(), secret) {
			t.Errorf("命令参数进了 Span：%s=%s", a.Key, a.Value.Emit())
		}
		if a.Key == "db.system" {
			instrumented = true
		}
	}
	if !instrumented {
		t.Fatal("前提：链路钩子该挂上，否则这条测试什么都没验")
	}
}

// useTP 换上记录用的 TracerProvider，测试结束还原
func useTP(t *testing.T) *recordingTP {
	tp := &recordingTP{}
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	return tp
}

func TestTrace_StartupConnectCheckOpensNoSpan(t *testing.T) {
	// 钩子挂在建连验证之前的话，每一次 Ping 尝试都是一个没有父 Span 的 ping，
	// 连不上时一轮重试就是一串报错的根 Span——那是一次启动，不是业务请求
	tp := useTP(t)
	f := newFakeRedis(t)
	f.setFailPing(true)
	if _, _, err := New(context.Background(), liveCfg(f)); err == nil {
		t.Fatal("前提：Ping 失败时 New 该失败")
	}
	if got := tp.spans(); len(got) != 0 {
		t.Errorf("连不上时的建连验证不该留下 Span，got=%v", got)
	}

	f.setFailPing(false)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if got := tp.spans(); slices.Contains(got, "ping") {
		t.Errorf("连得上时的那次建连验证也不该留下 ping Span，got=%v", got)
	}
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	if got := tp.spans(); !slices.Contains(got, "ping") {
		t.Errorf("建好之后业务发的命令该有 Span（钩子得挂上），got=%v", got)
	}
}

// 服务端的错误原文带着参数，实测 Redis 7.0.15（见 errorAttrs）。redisotel v9.22.0 把它原样写进
// exception.message 和状态描述（tracing.go recordError）；Span 上的规则要和命令日志的一样。
// 假服务端回的是真 Redis 的原话；XONE_E2E=1 时另对真的 Redis 跑一遍（地址同 scripts/e2e.sh 的 XONE_E2E_REDIS_ADDR）
func TestTrace_ServerErrorTextNotInSpan(t *testing.T) {
	const secret = "secretarg1"
	const coded = "status=Error:redis server error ERR (message omitted, it may contain argument values)"
	cases := []struct {
		name  string
		reply string // 假服务端对这条命令的回复，真 Redis 自己会回同样的话
		do    func(context.Context, *redis.Client) error
		want  string
	}{
		{"未知命令引出参数", "-ERR unknown command 'foo', with args beginning with: '" + secret + "' \r\n",
			func(ctx context.Context, c *redis.Client) error { return c.Do(ctx, "foo", secret).Err() }, coded},
		{"EVAL 里 error_reply(ARGV[1])", "-ERR " + secret + "\r\n",
			func(ctx context.Context, c *redis.Client) error {
				return c.Eval(ctx, "return redis.error_reply('ERR ' .. ARGV[1])", nil, secret).Err()
			}, coded},
		{"EVAL 里 {err=ARGV[1]}，错误码的位置都是值", "-" + secret + "\r\n",
			func(ctx context.Context, c *redis.Client) error {
				return c.Eval(ctx, "return {err=ARGV[1]}", nil, secret).Err()
			}, "status=Error:redis server error (message omitted, it may contain argument values)"},
	}
	backends := map[string]func(t *testing.T, reply string) ClientConfig{
		"假服务端": func(t *testing.T, reply string) ClientConfig {
			f := newFakeRedis(t)
			f.setReply("foo", reply)
			f.setReply("eval", reply)
			return liveCfg(f)
		},
	}
	if os.Getenv("XONE_E2E") == "1" {
		backends["真 Redis"] = func(*testing.T, string) ClientConfig {
			c := DefaultClientConfig()
			c.Addr = cmp.Or(os.Getenv("XONE_E2E_REDIS_ADDR"), "127.0.0.1:6379")
			return c
		}
	}
	for backend, cfgOf := range backends {
		for _, c := range cases {
			t.Run(backend+"/"+c.name, func(t *testing.T) {
				tp := useTP(t)
				client, closer, err := New(context.Background(), cfgOf(t, c.reply))
				if err != nil {
					t.Fatal(err)
				}
				defer closer.Close()

				err = c.do(context.Background(), client)
				if err == nil || !strings.Contains(err.Error(), secret) {
					t.Fatalf("前提：返回给调用方的错误原样带着服务端的原文，got=%v", err)
				}
				got := strings.Join(tp.errors(), "\n")
				if strings.Contains(got, secret) {
					t.Errorf("参数值进了 Span：\n%s", got)
				}
				if !strings.Contains(got, c.want) {
					t.Errorf("Span 上该有 %q，got：\n%s", c.want, got)
				}
			})
		}
	}
}

// 客户端这一侧认得出的错照原文记：地址、超时正是排查要看的
func TestTrace_ClientSafeErrorKeptInSpan(t *testing.T) {
	tp := useTP(t)
	f := newFakeRedis(t)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	f.setStall("get")
	err = client.Get(context.Background(), "k").Err()
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("前提：读超时，got=%v", err)
	}
	got := strings.Join(tp.errors(), "\n")
	if !strings.Contains(got, "exception.message="+err.Error()) || !strings.Contains(got, "status=Error:"+err.Error()) {
		t.Errorf("网络错误该原样记下，got：\n%s", got)
	}
}

// redisotel v9.22.0 的 Span 名是 cmd.FullName()：第 1 个参数原样转小写，不校验（tracing.go ProcessHook）；
// pipeline 是 "redis.pipeline " 加各条命令的 FullName（rediscmd.CmdsString）。
// 实测 Do(ctx, "SET k1 hunter2") 的 Span 名是 "set k1 hunter2"，整条命令连同值进了链路后端。
// 规则同命令日志的 cmd 字段（cmdName）。XONE_E2E=1 时另对真的 Redis 跑一遍
func TestTrace_SpanNameCarriesNoArgs(t *testing.T) {
	backends := map[string]func(t *testing.T) ClientConfig{
		"假服务端": func(t *testing.T) ClientConfig { return liveCfg(newFakeRedis(t)) },
	}
	if os.Getenv("XONE_E2E") == "1" {
		backends["真 Redis"] = func(*testing.T) ClientConfig {
			c := DefaultClientConfig()
			c.Addr = cmp.Or(os.Getenv("XONE_E2E_REDIS_ADDR"), "127.0.0.1:6379")
			return c
		}
	}
	for backend, cfgOf := range backends {
		t.Run(backend, func(t *testing.T) {
			tp := useTP(t)
			cfg := cfgOf(t)
			client, closer, err := New(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			ctx := context.Background()

			// 命令名里带着整条命令：真 Redis 回 unknown command，错误不是这里要验的
			_ = client.Do(ctx, "SET k1 hunter2").Err()
			_, _ = client.Pipelined(ctx, func(p redis.Pipeliner) error {
				p.Do(ctx, "SET k2 pipesecret")
				p.Get(ctx, "k2")
				return nil
			})
			// 合规的命令名照旧
			_ = client.Set(ctx, "k3", "v", 0).Err()
			_, _ = client.Pipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, "k3", "v", 0)
				p.Get(ctx, "k3")
				return nil
			})
			_ = client.Do(ctx, "command", "count").Err()
			// 占住池里那条空闲连接，下一条命令就得新拨一条：redis.dial 照旧
			held := client.Conn()
			_ = held.Ping(ctx).Err()
			_ = client.Get(ctx, "k3").Err()
			_ = held.Close()

			got := tp.spans()
			for _, name := range got {
				if strings.Contains(name, "hunter2") || strings.Contains(name, "pipesecret") || strings.Contains(name, "k1") {
					t.Errorf("命令参数进了 Span 名：%q", name)
				}
			}
			for _, want := range []string{
				"<invalid>", "redis.pipeline <invalid> get",
				"set", "redis.pipeline set get", "command count", "redis.dial",
			} {
				if !slices.Contains(got, want) {
					t.Errorf("该有 Span %q，got=%q", want, got)
				}
			}
		})
	}
}
