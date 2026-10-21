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

// maxLoggedKey is how many bytes of a key are recorded at most; anything beyond is cut and key_truncated is added.
// A key is assembled by business code, and someone occasionally uses a whole JSON document as one; a log line should not grow to tens of KB for that.
const maxLoggedKey = 256

// maxLoggedPipelineCmds is how many command names one pipeline log line lists at most; the total is in count.
const maxLoggedPipelineCmds = 10

// logHook records one line per command and per pipeline (ClientConfig.Log).
//
// Only the command name and the first key are recorded, never the value and never the other arguments: a value often
// holds a session, a token or personal information, the same principle that has xgorm record SQL with placeholders
// and keep db.statement off the trace. The key is recorded whole: if business code puts a token or a phone number
// into a key, it reaches the log (see ClientConfig.Log).
// The ctx is the command's own, which is how xlog gives the log a trace_id / span_id. The hook is added after the tracing hook and therefore runs inside it, so span_id is this command's redis Span (see New).
type logHook struct {
	name string        // the instance name, which tells several Redis instances apart; empty for one built by calling New directly
	slow time.Duration // 0 records no slow commands
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

// command records one line per command. redis.Nil (the key does not exist) is not a failure and is recorded as an ordinary line with nil=true.
func (h logHook) command(ctx context.Context, cmd redis.Cmder, err error, elapsed time.Duration) {
	failed := err != nil && !errors.Is(err, redis.Nil)
	if !failed && !h.isSlow(elapsed) && !slog.Default().Enabled(ctx, slog.LevelInfo) {
		return // do not assemble the fields for nothing when slog is not accepting info: this is the path every command takes
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

// pipeline records one line for the whole pipeline: the command count, the first few command names and the first real error.
// It carries no key: a pipeline may hold thousands of commands, and listing every key would make the line endless.
// count is the number of commands go-redis actually sent: a TxPipelined holds MULTI and EXEC inside it.
func (h logHook) pipeline(ctx context.Context, cmds []redis.Cmder, err error, elapsed time.Duration) {
	// A WATCHed key changed by somebody else: EXEC answers an empty array and go-redis sets redis.TxFailedErr on the
	// pipeline and on every command inside it. That is the normal outcome of optimistic locking (the caller retries) and not a failure, as with redis.Nil.
	txFailed := errors.Is(err, redis.TxFailedErr)
	// Exec also returns redis.Nil when one command got it, which is not a failure either; find the first one that is not Nil.
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

// nameAttr is the instance name. One built by calling New directly has no name and the field is not written.
func (h logHook) nameAttr() []any {
	if h.name == "" {
		return nil
	}
	return []any{"name", h.name}
}

func (h logHook) isSlow(elapsed time.Duration) bool { return h.slow > 0 && elapsed > h.slow }

// nilAttr adds nil=true for a miss and passes everything else through.
func nilAttr(err error, extra ...any) []any {
	if errors.Is(err, redis.Nil) {
		return append([]any{"nil", true}, extra...)
	}
	return extra
}

// keyAttrs cuts an overlong key to maxLoggedKey bytes (without cutting a UTF-8 character) and adds key_truncated.
//
// Walking back to a character boundary takes at most utf8.UTFMax-1 bytes: valid UTF-8 is found within those steps.
// A key is an arbitrary byte string, and one that is a run of 0x80 has no boundary at all, so walking back forever would cut it to an empty string.
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

// invalidCmdName is what a command name that does not look like one is recorded as.
const invalidCmdName = "<invalid>"

// validCmdName is a Redis command name, plus the dot of module commands (json.set, ft.search) and the | of the subcommand form, at most 64 bytes.
var validCmdName = regexp.MustCompile(`^[a-z][a-z0-9._|-]{0,63}$`)

// cmdName is the command name in the log.
//
// go-redis's cmd.Name() is the first argument lowercased as it is, neither truncated nor validated: measured,
// Do(ctx, "SET k1 <value>") is recorded as cmd: "set k1 <value>", with the whole command and its value inside the command name.
// Anything that does not look like a command name is replaced by a fixed placeholder rather than guessing which part is the command.
func cmdName(cmd redis.Cmder) string {
	if name := cmd.Name(); validCmdName.MatchString(name) {
		return name
	}
	return invalidCmdName
}

// firstKey takes a command's first key, and gives none when it cannot be sure.
//
// go-redis v9.22.0 knows where the key is itself (Cmder's firstKeyPos), but that is an unexported method;
// and its fallback is "anything not on the no-key list has the key as the first argument": MIGRATE (whose first is a host),
// INFO and KEYS would all be taken to have a key. This does the reverse: only the commands on the list below are recognized, and anything else records no key.
// Guessing wrong costs a value written into the log, while missing a key only costs one field.
//
// AUTH, HELLO (whose arguments are the password when AUTH is given), MIGRATE (which may carry AUTH) and CONFIG SET --
// commands whose arguments may hold credentials -- are not on the list, and nothing but the command name is recorded for them.
func firstKey(cmd redis.Cmder) (string, bool) {
	args := cmd.Args()
	pos := 0
	switch name := cmd.Name(); name {
	case "eval", "evalsha", "eval_ro", "evalsha_ro", "fcall", "fcall_ro":
		// EVAL script numkeys key... arg...: with numkeys 0, the 3rd argument is already ARGV
		if numKeys(args) > 0 {
			pos = 3
		}
	case "xread":
		// XREAD [COUNT n] [BLOCK ms] STREAMS key... id...: what follows COUNT / BLOCK is a number,
		// so the first STREAMS is that keyword
		pos = afterStreams(args, 1)
	case "xreadgroup":
		// XREADGROUP GROUP group consumer [...] STREAMS key...: the group and consumer names may happen to be "streams", so they are skipped
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

// afterStreams looks for the STREAMS keyword from index from and returns the position of the argument after it, or 0 when there is none.
func afterStreams(args []any, from int) int {
	for i := from; i < len(args)-1; i++ {
		if s, ok := args[i].(string); ok && strings.EqualFold(s, "streams") {
			return i + 1
		}
	}
	return 0
}

// numKeys is EVAL / FCALL's numkeys. go-redis's Eval passes an int (len(keys)),
// while one assembled with Do is usually a string; an unrecognized one counts as 0 and no key is recorded.
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

// argString takes the i-th argument when it is a string (or []byte); other types are not guessed at.
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

// keyAtFirstArg are the commands whose first argument is certainly a key. Anything else records no key, see firstKey.
var keyAtFirstArg = setOf(
	// string
	"get", "set", "setnx", "setex", "psetex", "getset", "getdel", "getex", "getrange", "setrange",
	"append", "strlen", "incr", "incrby", "incrbyfloat", "decr", "decrby", "mget", "mset", "msetnx", "lcs",
	// general
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
	// HyperLogLog, geo, bitmap
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

// errorAttrs are the error's fields; the text and the error code are in redactedError.
func errorAttrs(err error) []any {
	text, code := redactedError(err)
	if code != "" {
		return []any{"error", text, "error_code", code}
	}
	return []any{"error", text}
}

// redactedError is the error text that goes into the log and onto the Span: a server-side error keeps its error code alone, never the text.
//
// A server's error text carries the arguments out with it, measured on Redis 7.0.15:
//
//	ERR unknown command 'foo', with args beginning with: 'secretarg1' 'secretarg2'
//	ERR mysecretvalue                     ← redis.error_reply(ARGV[1]) inside an EVAL
//	plainsecret                           ← return {err=ARGV[1]} inside an EVAL, where even the error code position is a value
//
// So the error code is not "the first word" either: only the ones in knownErrorCodes are recognized, and the rest only say that it is a server error.
// On the client side, only the few kinds clientSafe recognizes are recorded verbatim;
// the rest are not either -- when go-redis fails to parse a reply it writes the reply content into the error (reader.go's can't parse %q).
// The error returned to the caller is unchanged.
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

// clientSafe are the client-side errors whose text may be recorded verbatim: addresses and timeouts, which is exactly what an investigation needs.
func clientSafe(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, redis.ErrClosed) || errors.Is(err, redis.ErrPoolTimeout) || errors.Is(err, redis.ErrPoolExhausted)
}

// knownErrorCodes are the error codes Redis itself uses (the prefixes of addReplyError in the Redis 7 source, plus the cluster and ACL ones).
var knownErrorCodes = setOf(
	"ERR", "WRONGTYPE", "NOSCRIPT", "BUSY", "BUSYKEY", "BUSYGROUP", "NOTBUSY", "UNKILLABLE",
	"NOAUTH", "WRONGPASS", "NOPERM", "OOM", "READONLY", "EXECABORT", "LOADING", "MISCONF",
	"MOVED", "ASK", "TRYAGAIN", "CROSSSLOT", "CLUSTERDOWN", "MASTERDOWN", "NOREPLICAS",
	"NOGROUP", "NOPROTO", "UNBLOCKED", "NOQUORUM",
)

// ms converts an elapsed time to milliseconds, keeping microseconds. The field name carries the unit, matching xgorm's elapsed_ms.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
