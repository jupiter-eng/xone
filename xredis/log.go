package xredis

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

// maxLoggedKey key 最多记这么多字节，超出的截掉并带上 key_truncated。
// key 是业务拼出来的，偶尔有人把一整段 JSON 当 key，一条日志不该因此撑到几十 KB
const maxLoggedKey = 256

// maxLoggedPipelineCmds 一条 pipeline 日志最多列出这么多个命令名，总数另见 count
const maxLoggedPipelineCmds = 10

// logHook 把每条命令、每个 pipeline 记一条日志（ClientConfig.Log）。
//
// 只记命令名和第一个 key，不记值、不记其余参数：值里常有会话、令牌、个人信息，
// 与 xgorm 只记占位符 SQL、链路不写 db.statement 是同一条原则。key 是整条记的：
// 业务把令牌、手机号拼进 key 的话，它们就进了日志（见 ClientConfig.Log）。
// ctx 用命令自己的：xlog 由此给日志带上 trace_id / span_id。钩子挂在链路钩子之后，
// 在它里面，所以 span_id 是这条命令的 redis Span（见 New）。
type logHook struct {
	name string        // 实例名，配了好几个 Redis 时分得清是哪一个；直接调 New 建的为空
	slow time.Duration // 0 不记慢命令
}

func (h logHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h logHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		h.command(ctx, cmd, err, time.Since(start))
		return err
	}
}

func (h logHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		h.pipeline(ctx, cmds, err, time.Since(start))
		return err
	}
}

// command 一条命令记一行。redis.Nil（key 不存在）不是故障，记成普通的一行、带 nil=true
func (h logHook) command(ctx context.Context, cmd redis.Cmder, err error, elapsed time.Duration) {
	failed := err != nil && !errors.Is(err, redis.Nil)
	if !failed && !h.isSlow(elapsed) && !slog.Default().Enabled(ctx, slog.LevelInfo) {
		return // slog 不收 info 时不白拼字段：这是每条命令都走的路
	}

	attrs := make([]any, 0, 12)
	attrs = append(attrs, h.nameAttr()...)
	attrs = append(attrs, "cmd", cmdName(cmd))
	if key, ok := firstKey(cmd); ok {
		attrs = append(attrs, keyAttrs(key)...)
	}
	attrs = append(attrs, "elapsed_ms", ms(elapsed))

	switch {
	case failed:
		slog.WarnContext(ctx, "redis command failed", append(attrs, errorAttrs(err)...)...)
	case h.isSlow(elapsed):
		slog.WarnContext(ctx, "slow redis command", append(attrs, nilAttr(err, "threshold_ms", ms(h.slow))...)...)
	default:
		slog.InfoContext(ctx, "redis command", append(attrs, nilAttr(err)...)...)
	}
}

// pipeline 整个 pipeline 记一行：命令数、前几个命令名、第一个真正的错误。
// 不带 key：一个 pipeline 可以有上千条命令，每条的 key 都列出来一行日志就没边了。
// count 是 go-redis 实际发出去的命令数：TxPipelined 的里面带着 MULTI 和 EXEC
func (h logHook) pipeline(ctx context.Context, cmds []redis.Cmder, err error, elapsed time.Duration) {
	// WATCH 的 key 被别人改了：EXEC 回空数组，go-redis 给 pipeline 和里面每条命令都设成
	// redis.TxFailedErr。那是乐观锁的正常结果（调用方重试就是了），不是故障，与 redis.Nil 同理
	txFailed := errors.Is(err, redis.TxFailedErr)
	// Exec 在某条命令拿到 redis.Nil 时也返回它，那不算失败；找第一个不是 Nil 的
	if err == nil || errors.Is(err, redis.Nil) || txFailed {
		err = nil
		for _, c := range cmds {
			if e := c.Err(); e != nil && !errors.Is(e, redis.Nil) && !errors.Is(e, redis.TxFailedErr) {
				err = e
				break
			}
		}
	}
	if err == nil && !h.isSlow(elapsed) && !slog.Default().Enabled(ctx, slog.LevelInfo) {
		return
	}

	names := make([]string, 0, min(len(cmds), maxLoggedPipelineCmds))
	for _, c := range cmds[:cap(names)] {
		names = append(names, cmdName(c))
	}
	attrs := append(h.nameAttr(), "count", len(cmds), "cmds", names, "elapsed_ms", ms(elapsed))
	if txFailed {
		attrs = append(attrs, "tx_failed", true)
	}

	switch {
	case err != nil:
		slog.WarnContext(ctx, "redis pipeline failed", append(attrs, errorAttrs(err)...)...)
	case h.isSlow(elapsed):
		slog.WarnContext(ctx, "slow redis pipeline", append(attrs, "threshold_ms", ms(h.slow))...)
	default:
		slog.InfoContext(ctx, "redis pipeline", attrs...)
	}
}

// nameAttr 实例名。直接调 New 建的没有名字，就不写这个字段
func (h logHook) nameAttr() []any {
	if h.name == "" {
		return nil
	}
	return []any{"name", h.name}
}

func (h logHook) isSlow(elapsed time.Duration) bool { return h.slow > 0 && elapsed > h.slow }

// nilAttr 没查到时补一个 nil=true，其余原样
func nilAttr(err error, extra ...any) []any {
	if errors.Is(err, redis.Nil) {
		return append([]any{"nil", true}, extra...)
	}
	return extra
}

// keyAttrs key 超长时截到 maxLoggedKey 字节（不切断 UTF-8 字符），带上 key_truncated。
//
// 往回找字符起点最多退 utf8.UTFMax-1 个字节：合法的 UTF-8 一定在这几步里找到。
// key 是任意字节串，一串 0x80 这样的根本没有起点，一路退下去会截成空串
func keyAttrs(key string) []any {
	if len(key) <= maxLoggedKey {
		return []any{"key", key}
	}
	cut := maxLoggedKey
	for back := 0; back < utf8.UTFMax-1 && !utf8.RuneStart(key[cut]); back++ {
		cut--
	}
	if !utf8.RuneStart(key[cut]) {
		cut = maxLoggedKey
	}
	return []any{"key", key[:cut], "key_truncated", true}
}

// invalidCmdName 命令名不像命令名时记成这个
const invalidCmdName = "<invalid>"

// validCmdName Redis 的命令名，加上模块命令里的点（json.set、ft.search）、子命令记法里的 |，最长 64 字节
var validCmdName = regexp.MustCompile(`^[a-z][a-z0-9._|-]{0,63}$`)

// cmdName 日志里的命令名。
//
// go-redis 的 cmd.Name() 是第 1 个参数原样转小写，不截断、不校验：实测
// Do(ctx, "SET k1 <值>") 记出来是 cmd: "set k1 <值>"，整条命令连同值都在命令名里。
// 不像命令名的一律换成固定的占位，不猜它哪一截是命令
func cmdName(cmd redis.Cmder) string {
	if name := cmd.Name(); validCmdName.MatchString(name) {
		return name
	}
	return invalidCmdName
}

// firstKey 取命令的第一个 key，认不准就不给。
//
// go-redis v9.22.0 自己知道 key 在哪（Cmder 的 firstKeyPos），但那是未导出的方法；
// 它的兜底又是「不在免 key 名单里就当第 1 个参数」：MIGRATE（第 1 个是 host）、
// INFO、KEYS 都会被当成有 key。这里反过来：只认下面名单里的命令，名单外的一律不记 key。
// 猜错的代价是把一个值写进日志，漏记一个 key 只是少一个字段。
//
// AUTH、HELLO（带 AUTH 时参数里就是密码）、MIGRATE（可以带 AUTH）、CONFIG SET 这类
// 参数里可能有凭证的命令不在名单里，除了命令名什么都不记。
func firstKey(cmd redis.Cmder) (string, bool) {
	args := cmd.Args()
	pos := 0
	switch name := cmd.Name(); name {
	case "eval", "evalsha", "eval_ro", "evalsha_ro", "fcall", "fcall_ro":
		// EVAL script numkeys key... arg...：numkeys 为 0 时第 3 个参数已经是 ARGV
		if numKeys(args) > 0 {
			pos = 3
		}
	case "xread":
		// XREAD [COUNT n] [BLOCK ms] STREAMS key... id...：COUNT / BLOCK 后面都是数字，
		// 第一个 STREAMS 就是那个关键字
		pos = afterStreams(args, 1)
	case "xreadgroup":
		// XREADGROUP GROUP group consumer [...] STREAMS key...：组名、消费者名可以恰好叫 streams，跳过它们
		pos = afterStreams(args, 4)
	default:
		if _, ok := keyAtFirstArg[name]; ok {
			pos = 1
		}
	}
	if pos == 0 {
		return "", false
	}
	return argString(args, pos)
}

// afterStreams 从 from 起找 STREAMS 关键字，返回它后面那个参数的位置，找不到返回 0
func afterStreams(args []any, from int) int {
	for i := from; i < len(args)-1; i++ {
		if s, ok := args[i].(string); ok && strings.EqualFold(s, "streams") {
			return i + 1
		}
	}
	return 0
}

// numKeys EVAL / FCALL 的 numkeys。go-redis 的 Eval 传的是 int（len(keys)），
// 自己用 Do 拼的多半是字符串；认不出来当 0，不记 key
func numKeys(args []any) int64 {
	if len(args) < 3 {
		return 0
	}
	switch v := args[2].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// argString 第 i 个参数是字符串（或 []byte）时取出来；别的类型不猜
func argString(args []any, i int) (string, bool) {
	if i >= len(args) {
		return "", false
	}
	switch v := args[i].(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

// keyAtFirstArg 第 1 个参数一定是 key 的命令。名单外的不记 key，理由见 firstKey
var keyAtFirstArg = setOf(
	// string
	"get", "set", "setnx", "setex", "psetex", "getset", "getdel", "getex", "getrange", "setrange",
	"append", "strlen", "incr", "incrby", "incrbyfloat", "decr", "decrby", "mget", "mset", "msetnx", "lcs",
	// 通用
	"del", "unlink", "exists", "expire", "expireat", "pexpire", "pexpireat", "expiretime", "pexpiretime",
	"ttl", "pttl", "persist", "type", "rename", "renamenx", "copy", "dump", "restore", "touch",
	"sort", "sort_ro", "watch",
	// hash
	"hset", "hsetnx", "hget", "hmget", "hmset", "hdel", "hexists", "hgetall", "hkeys", "hvals", "hlen",
	"hincrby", "hincrbyfloat", "hstrlen", "hrandfield", "hscan", "hgetdel", "hgetex", "hsetex",
	"hexpire", "hpexpire", "hexpireat", "hpexpireat", "httl", "hpttl", "hexpiretime", "hpexpiretime", "hpersist",
	// list
	"lpush", "rpush", "lpushx", "rpushx", "lpop", "rpop", "llen", "lrange", "lindex", "lset", "linsert",
	"lrem", "ltrim", "lpos", "lmove", "rpoplpush", "blmove", "brpoplpush", "blpop", "brpop",
	// set
	"sadd", "srem", "smembers", "sismember", "smismember", "scard", "spop", "srandmember",
	"sinter", "sunion", "sdiff", "sinterstore", "sunionstore", "sdiffstore", "smove", "sscan",
	// sorted set
	"zadd", "zrem", "zscore", "zmscore", "zincrby", "zcard", "zcount", "zlexcount",
	"zrange", "zrangebyscore", "zrangebylex", "zrevrange", "zrevrangebyscore", "zrevrangebylex", "zrangestore",
	"zrank", "zrevrank", "zremrangebyrank", "zremrangebyscore", "zremrangebylex",
	"zpopmin", "zpopmax", "bzpopmin", "bzpopmax", "zrandmember", "zscan",
	// stream
	"xadd", "xlen", "xrange", "xrevrange", "xdel", "xtrim", "xack", "xpending", "xclaim", "xautoclaim", "xsetid",
	// HyperLogLog、geo、bitmap
	"pfadd", "pfcount", "pfmerge",
	"geoadd", "geopos", "geodist", "geohash", "georadius", "georadiusbymember", "geosearch", "geosearchstore",
	"setbit", "getbit", "bitcount", "bitpos", "bitfield", "bitfield_ro",
)

func setOf(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// errorAttrs 错误的字段，文本和错误码见 redactedError
func errorAttrs(err error) []any {
	text, code := redactedError(err)
	if code != "" {
		return []any{"error", text, "error_code", code}
	}
	return []any{"error", text}
}

// redactedError 要写进日志和 Span 的错误文本：服务端报的错只记错误码，不记原文。
//
// 服务端的错误原文会把参数带出来，实测 Redis 7.0.15：
//
//	ERR unknown command 'foo', with args beginning with: 'secretarg1' 'secretarg2'
//	ERR mysecretvalue                     ← EVAL 里 redis.error_reply(ARGV[1])
//	plainsecret                           ← EVAL 里 return {err=ARGV[1]}，连错误码的位置都是值
//
// 所以错误码也不是「取第一个词」：只认 knownErrorCodes 里的，其余的只说是服务端的错。
// 客户端这一侧的错误只有 clientSafe 认得的几类照原文记；
// 别的也不记原文——go-redis 解析回复失败时把回复内容写进错误（reader.go 的 can't parse %q）。
// 返回给调用方的错误不变。
func redactedError(err error) (text, code string) {
	var rerr redis.Error
	if errors.As(err, &rerr) {
		code, _, _ := strings.Cut(rerr.Error(), " ")
		if _, ok := knownErrorCodes[code]; ok {
			return "redis server error " + code + " (message omitted, it may contain argument values)", code
		}
		return "redis server error (message omitted, it may contain argument values)", ""
	}
	if clientSafe(err) {
		return err.Error(), ""
	}
	return "redis client error (message omitted, it may contain reply data)", ""
}

// clientSafe 客户端这一侧、原文可以照记的错：地址、超时，排查正要看这个
func clientSafe(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, redis.ErrClosed) || errors.Is(err, redis.ErrPoolTimeout) || errors.Is(err, redis.ErrPoolExhausted)
}

// knownErrorCodes Redis 自己用的错误码（Redis 7 源码里 addReplyError 的前缀，加上集群、ACL 的那几个）
var knownErrorCodes = setOf(
	"ERR", "WRONGTYPE", "NOSCRIPT", "BUSY", "BUSYKEY", "BUSYGROUP", "NOTBUSY", "UNKILLABLE",
	"NOAUTH", "WRONGPASS", "NOPERM", "OOM", "READONLY", "EXECABORT", "LOADING", "MISCONF",
	"MOVED", "ASK", "TRYAGAIN", "CROSSSLOT", "CLUSTERDOWN", "MASTERDOWN", "NOREPLICAS",
	"NOGROUP", "NOPROTO", "UNBLOCKED", "NOQUORUM",
)

// ms 耗时换成毫秒，保留到微秒。字段名带单位，与 xgorm 的 elapsed_ms 一致
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
