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

// 这一组钉的是 Redis 出故障时的行为：建连拨几次号、ctx 的取消和截止时间各管什么、
// 认证失败怎么报、go-redis 自己的日志去了哪。数字都写进了 xredis/README.md XRedis 一节

func TestNew_RedisDownDialsOncePerConnect(t *testing.T) {
	// go-redis v9.22.0 默认每次建连内部重拨 5 次、间隔 100ms：拒绝连接时一次建连白等 400ms，
	// 主机宕机时是 5×DialTimeout+400ms。xredis 配成只拨一次，重试只剩 MaxRetries 那一层
	f := newFakeRedis(t)
	c := liveCfg(f)
	c.MinIdleConns, c.MaxRetries, c.Trace = 0, -1, false
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	f.stop()
	// 池里那条连接已经断了：第一条命令可能先撞上它（EOF），再往后才走到建连
	for i := range 3 {
		start := time.Now()
		err := client.Get(context.Background(), "k").Err()
		took := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			continue
		}
		t.Logf("拒绝连接时第 %d 条命令用了 %v：%v", i+1, took, err)
		// 上界取 go-redis 默认行为（400ms）的一半多一点：实测当场返回（1ms 量级），慢机器上也留足余量
		if took > 250*time.Millisecond {
			t.Errorf("一次建连只该拨一次号：拒绝连接时应当场返回，实际 %v（go-redis 默认重拨 5 次、每次间隔 100ms 就是 400ms）", took)
		}
		return
	}
	t.Fatal("3 条命令都没走到建连")
}

func TestNew_CommandsIgnoreCtxCancelOnlyDeadline(t *testing.T) {
	// 钉住 go-redis 的行为，文档（ContextTimeoutEnabled 的注释、xredis/README.md XRedis）照这个写：
	// ctx 被取消叫不醒一个阻塞在读上的命令，它等到 ReadTimeout 才返回。
	// 哪天升级之后这条挂了，是 go-redis 开始听取消了——把文档里那句限制删掉
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
	t.Logf("ReadTimeout=%v，100ms 时取消 ctx：命令 %v 后返回：%v", c.ReadTimeout, took.Round(time.Millisecond), err)
	if took < c.ReadTimeout-100*time.Millisecond {
		t.Errorf("go-redis 开始听 ctx 的取消了（%v 就返回），文档里「取消叫不醒阻塞的命令」该改了", took)
	}
}

func TestNew_ExitSignalAtStartupSkipsReadTimeout(t *testing.T) {
	// 对端收下连接不回话，Ping 卡在读上。go-redis 只认 ctx 的截止时间，
	// 直接在当前协程里 Ping 的话，取消要等这次读撞上 ReadTimeout（这里 5s）才生效。
	// 上界 1.5s：离 100ms 的取消点和 5s 的读超时都远，慢机器上不会误报
	f := newFakeRedis(t)
	f.setStall("ping")
	c := liveCfg(f)
	c.ReadTimeout, c.MinIdleConns = 5*time.Second, 0

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := New(ctx, c)
	took := time.Since(start)
	t.Logf("100ms 时取消，New %v 后返回：%v", took.Round(time.Millisecond), err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消了就该如实报取消，实际 %v", err)
	}
	if took > 1500*time.Millisecond {
		t.Errorf("退出信号到了应当场放弃，实际 %v 才返回（ReadTimeout=%v）", took, c.ReadTimeout)
	}
	// 丢下的那次 Ping 不漏：New 关掉 client，卡着的连接跟着断开
	if n := f.waitConns(0); n != 0 {
		t.Errorf("放弃之后连接应全部关掉，还剩 %d 条", n)
	}
}

func TestNew_AuthFailureReportedClearlyWithoutRetry(t *testing.T) {
	f := newFakeRedis(t)
	f.setFailAuth(true)
	c := liveCfg(f)
	c.Password, c.MinIdleConns = "wrong", 0 // 不预热：预热的连接也会各发一次 AUTH

	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("密码不对时应当报错")
	}
	t.Logf("错误：%v", err)
	if !strings.Contains(err.Error(), "authentication to "+c.Addr+" failed") || strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("认证失败应说清是认证失败，而不是连不上，实际 %v", err)
	}
	if !redis.IsAuthError(err) {
		t.Errorf("原始的认证错误应能用 redis.IsAuthError 认出来，实际 %v", err)
	}
	if n := f.count("auth"); n != 1 {
		t.Errorf("密码不对再试也不对：应只 AUTH 一次，实际 %d 次", n)
	}
	assertOneFrame(t, err, "xredis", "connect")
}

// ctxKey 测 go-redis 的日志把 ctx 传下来了用的 key
type ctxKey struct{}

// recorded 一条日志里测试关心的部分
type recorded struct {
	level  slog.Level
	msg    string
	detail string
	ctxVal any
}

// ctxRecorder 记下每条日志和它收到的 ctx 里的 ctxKey
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
	// go-redis 默认用标准库 log 往 stderr 写纯文本：不是 JSON、不带 trace_id。
	// 让它写一条：过期时间短于 1ms 时它会记一句「truncating to 1ms」，用的是命令的 ctx
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
				t.Errorf("级别应是 WARN，实际 %v", r.level)
			}
			if r.ctxVal != "trace-1" {
				t.Errorf("ctx 应原样传给 slog（trace_id 靠它），实际拿到 %v", r.ctxVal)
			}
			return
		}
	}
	t.Errorf("go-redis 自己的日志应进 slog，实际 slog 收到 %+v", recs)
}
