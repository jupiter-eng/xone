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

// recordingTP only records the attributes each Span was started with.
//
// otel's sdk + tracetest is not used: a test dependency is recorded as a direct dependency by go mod tidy
// and travels into every caller's module graph (the same reason as fakeRedis). redisotel gives every attribute
// at Start in one go, so recording that one place is enough.
type recordingTP struct {
	noop.TracerProvider
	mu    sync.Mutex
	attrs []attribute.KeyValue
	names []string
	// errs is the text assembled in call order from each Span's RecordError (exception.message), the attributes
	// SetAttributes added afterwards, and SetStatus's status description.
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

// recordingSpan records the error text landing on a Span: exception.message, the status description and the attributes added later.
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
	// redisotel v9.22.0 defaults to dbStmtEnabled=true and writes the whole command with its arguments into
	// db.statement: a SET's value and an AUTH's password all reach the tracing backend.
	// The same principle as xgorm's "do not record Statement.Vars".
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
			t.Errorf("a command argument reached the Span: %s=%s", a.Key, a.Value.Emit())
		}
		if a.Key == "db.system" {
			instrumented = true
		}
	}
	if !instrumented {
		t.Fatal("precondition: the tracing hook must be attached, or this test verifies nothing")
	}
}

// useTP installs the recording TracerProvider and restores the previous one when the test ends.
func useTP(t *testing.T) *recordingTP {
	tp := &recordingTP{}
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })
	return tp
}

func TestTrace_StartupConnectCheckOpensNoSpan(t *testing.T) {
	// Attached before the connection check, every Ping attempt would be a parentless ping Span,
	// and a failure would make one round of retries a string of failing root Spans -- for a startup, not a business request.
	tp := useTP(t)
	f := newFakeRedis(t)
	f.setFailPing(true)
	if _, _, err := New(context.Background(), liveCfg(f)); err == nil {
		t.Fatal("precondition: New must fail when Ping fails")
	}
	if got := tp.spans(); len(got) != 0 {
		t.Errorf("a failed connection check must leave no Span, got=%v", got)
	}

	f.setFailPing(false)
	client, closer, err := New(context.Background(), liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if got := tp.spans(); slices.Contains(got, "ping") {
		t.Errorf("a successful connection check must leave no ping Span either, got=%v", got)
	}
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	if got := tp.spans(); !slices.Contains(got, "ping") {
		t.Errorf("a business command after it is built must have a Span (the hook has to be attached), got=%v", got)
	}
}

// A server's error text carries the arguments, measured on Redis 7.0.15 (see errorAttrs). redisotel v9.22.0 writes it as it is into
// exception.message and the status description (tracing.go recordError); the rule on a Span has to match the command log's.
// The fake server answers with a real Redis's own words; with XONE_E2E=1 it runs against a real Redis too (the same address as scripts/e2e.sh's XONE_E2E_REDIS_ADDR).
func TestTrace_ServerErrorTextNotInSpan(t *testing.T) {
	const secret = "secretarg1"
	const coded = "status=Error:redis server error ERR (message omitted, it may contain argument values)"
	cases := []struct {
		name  string
		reply string // the fake server's answer to this command, which a real Redis would answer the same way
		do    func(context.Context, *redis.Client) error
		want  string
	}{
		{"an unknown command carrying the arguments out", "-ERR unknown command 'foo', with args beginning with: '" + secret + "' \r\n",
			func(ctx context.Context, c *redis.Client) error { return c.Do(ctx, "foo", secret).Err() }, coded},
		{"error_reply(ARGV[1]) inside an EVAL", "-ERR " + secret + "\r\n",
			func(ctx context.Context, c *redis.Client) error {
				return c.Eval(ctx, "return redis.error_reply('ERR ' .. ARGV[1])", nil, secret).Err()
			}, coded},
		{"{err=ARGV[1]} inside an EVAL, where even the error code position is a value", "-" + secret + "\r\n",
			func(ctx context.Context, c *redis.Client) error {
				return c.Eval(ctx, "return {err=ARGV[1]}", nil, secret).Err()
			}, "status=Error:redis server error (message omitted, it may contain argument values)"},
	}
	backends := map[string]func(t *testing.T, reply string) ClientConfig{
		"the fake server": func(t *testing.T, reply string) ClientConfig {
			f := newFakeRedis(t)
			f.setReply("foo", reply)
			f.setReply("eval", reply)
			return liveCfg(f)
		},
	}
	if os.Getenv("XONE_E2E") == "1" {
		backends["a real Redis"] = func(*testing.T, string) ClientConfig {
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
					t.Fatalf("precondition: the error returned to the caller carries the server's text as it is, got=%v", err)
				}
				got := strings.Join(tp.errors(), "\n")
				if strings.Contains(got, secret) {
					t.Errorf("a parameter value reached the Span:\n%s", got)
				}
				if !strings.Contains(got, c.want) {
					t.Errorf("the Span must carry %q, got:\n%s", c.want, got)
				}
			})
		}
	}
}

// A client-side error that is recognized is recorded verbatim: the address and the timeout are exactly what an investigation needs.
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
		t.Fatalf("precondition: a read timeout, got=%v", err)
	}
	got := strings.Join(tp.errors(), "\n")
	if !strings.Contains(got, "exception.message="+err.Error()) || !strings.Contains(got, "status=Error:"+err.Error()) {
		t.Errorf("a network error must be recorded as it is, got:\n%s", got)
	}
}

// redisotel v9.22.0 names a Span cmd.FullName(): the first argument lowercased as it is, unvalidated (tracing.go ProcessHook);
// a pipeline is "redis.pipeline " plus each command's FullName (rediscmd.CmdsString).
// Measured, Do(ctx, "SET k1 hunter2") produced the Span name "set k1 hunter2", putting the whole command and its value into the tracing backend.
// The rule follows the command log's cmd field (cmdName). With XONE_E2E=1 it runs against a real Redis too.
func TestTrace_SpanNameCarriesNoArgs(t *testing.T) {
	backends := map[string]func(t *testing.T) ClientConfig{
		"the fake server": func(t *testing.T) ClientConfig { return liveCfg(newFakeRedis(t)) },
	}
	if os.Getenv("XONE_E2E") == "1" {
		backends["a real Redis"] = func(*testing.T) ClientConfig {
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

			// The command name holds the whole command: a real Redis answers unknown command, and the error is not what is checked here
			_ = client.Do(ctx, "SET k1 hunter2").Err()
			_, _ = client.Pipelined(ctx, func(p redis.Pipeliner) error {
				p.Do(ctx, "SET k2 pipesecret")
				p.Get(ctx, "k2")
				return nil
			})
			// A conforming command name is unchanged
			_ = client.Set(ctx, "k3", "v", 0).Err()
			_, _ = client.Pipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, "k3", "v", 0)
				p.Get(ctx, "k3")
				return nil
			})
			_ = client.Do(ctx, "command", "count").Err()
			// Occupy the idle connection in the pool so the next command has to dial a new one: redis.dial is unchanged
			held := client.Conn()
			_ = held.Ping(ctx).Err()
			_ = client.Get(ctx, "k3").Err()
			_ = held.Close()

			got := tp.spans()
			for _, name := range got {
				if strings.Contains(name, "hunter2") || strings.Contains(name, "pipesecret") || strings.Contains(name, "k1") {
					t.Errorf("a command argument reached the Span name: %q", name)
				}
			}
			for _, want := range []string{
				"<invalid>", "redis.pipeline <invalid> get",
				"set", "redis.pipeline set get", "command count", "redis.dial",
			} {
				if !slices.Contains(got, want) {
					t.Errorf("there must be a Span %q, got=%q", want, got)
				}
			}
		})
	}
}
