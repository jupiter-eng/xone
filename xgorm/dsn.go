package xgorm

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 本文件里没有任何「DSN 脱敏」的代码，这是有意的。
//
// 脱敏的前提是把 DSN 放进了日志，然后再想办法把密码抠掉——那是个
// 永远做不干净的活：URL 形式、key=value 形式、密码里带 @ 或空格、
// 同一个 key 写两遍、解析失败的兜底……每一条都是一次可能的泄漏。
//
// 这里的做法是根本不打印 DSN。日志只写从 DSN 里解出来的、确定不含凭证的
// 结构化字段：驱动、地址、库名。没有密码进过日志，也就没有什么需要脱敏。

// errMalformedDSN / errMalformedQuery DSN 解析失败时报的错。
//
// 不回传解析器的原始错误：url.Parse 和驱动的 ParseDSN 都会把 DSN 片段带进
// 错误信息，而错误信息会被记下来。
var (
	errMalformedDSN = errors.New("failed to parse DSN, check the format of " + ConfigKey +
		" (details omitted to keep credentials out of logs)")
	errMalformedQuery = errors.New("failed to parse the query part of the DSN, check the format of " + ConfigKey +
		" (a literal % in a password must be written as %25; details omitted to keep credentials out of logs)")
)

// unknownDriver 报「不认识的驱动」，并把已注册的列出来
//
// 只说「不认识」帮助有限：驱动名写错和忘了 import 对应的模块是两个不同的
// 问题，把实际注册了哪些列出来，两者一眼可分
func unknownDriver(name Driver) error {
	return fmt.Errorf("unknown Driver=%q, registered: %v "+
		"(drivers other than mysql / postgres live in their own module, import it to register)",
		name, Drivers())
}

// openPostgres PostgreSQL 的 Dialector 构造函数。MySQL 的见 mysql.go
func openPostgres(dsn string) gorm.Dialector { return postgres.Open(dsn) }

// postgresAuthFailed SQLSTATE 第 28 类（invalid authorization specification）。
//
// 实测 pgx v5.10.0 连 PG 16，密码错、用户不存在都报 28P01（后者在 scram 认证下
// 也是 "password authentication failed"），错误链上是 *pgconn.PgError。
// 库不存在是 3D000，不在这一类里，仍按连不上处理。
func postgresAuthFailed(err error) bool {
	return strings.HasPrefix(postgresErrorCode(err), "28")
}

// postgresErrorCode 服务端报的 SQLSTATE。
//
// 实测 PG 16：PgError.Error() 是 "ERROR: <Message> (SQLSTATE <Code>)"，
// Message 里可能就有参数值（22P02 是 invalid input syntax for type integer: "notanint"），
// Detail 更是（23505 是 Key (email)=(a@b.com) already exists.），只是 Error() 不带 Detail
func postgresErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// resolvePostgres 把超时和运行时参数作为默认值补进 DSN
//
// 两种 DSN 格式都支持：postgres://... 的 URL 形式和 libpq 的 key=value 形式。
// 使用者在 DSN 里显式写了的 key 一律不覆盖——配置里的值只是默认值。
func resolvePostgres(c ClientConfig) (string, ConnInfo, error) {
	// 空值不算：没配的字段不注入，Params 里写成空的也不盖掉前面的字段
	defaults := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			defaults[k] = v
		}
	}
	put("connect_timeout", seconds(c.DialTimeout))
	put("statement_timeout", millis(c.Postgres.StatementTimeout))
	put("lock_timeout", millis(c.Postgres.LockTimeout))
	put("idle_in_transaction_session_timeout", millis(c.Postgres.IdleInTxTimeout))
	// Params 优先于上面几个字段：同名时以使用者显式写的为准
	for k, v := range c.Postgres.Params {
		put(k, v)
	}

	dsn := c.DSN
	if isPostgresURL(dsn) {
		var err error
		if dsn, err = injectPostgresURL(dsn, defaults); err != nil {
			return "", ConnInfo{}, err
		}
	} else {
		dsn = prependPostgresKV(dsn, defaults)
	}

	// 查的是补完参数之后的 DSN：Postgres.Params 里写 sslmode 同样算在 DSN 里。
	// 先于 parsePostgres：DSN 里写的 sslrootcert 读不到时，该报的是「两处都写了」
	if c.TLS.Enable {
		if err := checkPostgresTLSParams(dsn); err != nil {
			return "", ConnInfo{}, err
		}
	}
	pc, err := parsePostgres(dsn)
	if err != nil {
		return "", ConnInfo{}, err
	}
	if c.TLS.Enable {
		if err := checkPostgresTCP(pc); err != nil {
			return "", ConnInfo{}, err
		}
	}
	// 连接信息交给 pgx 自己解：真正拿这串 DSN 建连的是它，两边读出来的
	// 必然是同一份东西——密码里带着 host=… 这样的片段，也不会被读成
	// 连接信息写进日志。没写端口时这里是 pgx 补上的 5432，多主机时是第一个
	info := ConnInfo{
		Driver:       "postgres",
		Addr:         net.JoinHostPort(pc.Host, strconv.Itoa(int(pc.Port))),
		DB:           pc.Database,
		ProbeTimeout: postgresProbeTimeout(pc.ConnectTimeout, c.DialTimeout),
	}
	return dsn, info, nil
}

// postgresProbeTimeout 单次探测的预算：最终生效的 connect_timeout，再加一份 DialTimeout。
//
// connect 是 pgx 从最终 DSN 里读出来的 connect_timeout：没写时就是注入的那个——
// DialTimeout 向上取整的整秒（默认 500ms 注进去是 1s）。pgx 拿它管的是每个主机的
// 整个建连：TCP 之后的 TLS 握手、startup、认证都在里面（pgconn v5.10.0
// "restricts the whole connection process"；实测 TCP 秒连、startup 不回话的服务端，
// connect_timeout=1 等满 1.0s）。预算只给 DialTimeout 的话，一次慢一点但合法的握手
// 会在 pgx 自己放弃之前就被判超时。再加的那份 DialTimeout 给探测本身那个往返
func postgresProbeTimeout(connect, dial time.Duration) time.Duration {
	return cmp.Or(connect, ceilSeconds(dial)) + dial
}

// parsePostgres 用 pgx 自己的解析器过一遍，解不出来就报一个不带 DSN 的错。
//
// 不这么做的话，同一个错误要等到建连时才由 pgx 报出来，而它的错误原文里就是
// 整串 DSN：pgx 只遮得住 password=x、password='x' 两种规整写法（v5.10.0）。
// 实测 password = hunter2（等号两边有空格）和带转义引号的密码，都连同密码
// 一起进了 New 返回的错误——那是一条会进日志、进告警的错误。
//
// 用的必须是 pgx.ParseConfig 而不是 pgconn.ParseConfig：gorm 的 postgres 驱动
// 建连时调的是前者（gorm.io/driver/postgres v1.6.3），它在 pgconn 之上还要校验
// default_query_exec_mode、statement_cache_capacity、description_cache_capacity。
// 只用 pgconn 预检的话这三项写错照样放行，实测 password = hunter2 配
// default_query_exec_mode=bogus，密码原样进了 New 返回的错误。
//
// 证书文件读不到这类错误照原样报：里面只有文件路径，没有凭证，
// 吞掉的话使用者只剩一句「DSN 解析失败」，查不出是哪个文件。
func parsePostgres(dsn string) (*pgconn.Config, error) {
	cc, err := pgx.ParseConfig(dsn)
	if err == nil {
		return &cc.Config, nil
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return nil, fmt.Errorf("cannot read a file named in the DSN: %w", pathErr)
	}
	return nil, errMalformedDSN
}

func isPostgresURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// injectPostgresURL 往 URL 形式 DSN 的 query 里补参数，已有的 key 保留
//
// URL 形式没法像 key=value 那样把默认值垫在前面：pgx 在 query 里遇到
// 重复的 key 取的是第一个。
//
// 补的参数接在原 query 后面，使用者写的那部分一个字节都不动。从前是解开再
// q.Encode() 整个重写，TimeZone=Europe/Berlin 就成了 Europe%2FBerlin：
// pgx 会解码，但 gorm 的 postgres 驱动用 gormTimeZone 那个正则直接读原串、
// 不解码，于是每条连接都拿 Europe%2FBerlin 去设时区，服务端不认，全部失败
func injectPostgresURL(dsn string, injects map[string]string) (string, error) {
	if len(injects) == 0 {
		return dsn, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		// 同样不回传原始错误：url.Parse 的错误里带着整串 DSN
		return "", errMalformedDSN
	}
	// 显式 ParseQuery 而不是 u.Query()：后者会把错误吞掉，只返回解得出的那部分。
	// 于是密码里带一个字面 % （构成非法的百分号转义）时，那一项会被静默丢掉，
	// 回写之后 DSN 里就没有密码了——服务报「认证失败」，而配置文件里密码明明写着。
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		// 同样不回传原始错误：它带着出问题的那个片段，而那多半就是凭证
		return "", errMalformedQuery
	}

	pairs := []string{}
	if u.RawQuery != "" {
		pairs = append(pairs, u.RawQuery)
	}
	for _, k := range slices.Sorted(maps.Keys(injects)) {
		if !q.Has(k) {
			pairs = append(pairs, queryEscape(k)+"="+queryEscape(injects[k]))
		}
	}
	u.RawQuery = strings.Join(pairs, "&")
	return u.String(), nil
}

// queryEscape 按 query 的规则转义，但 / 原样保留。
//
// / 在 query 里本就合法（RFC 3986），pgx 解不解码都一样；转成 %2F 的话，
// Params 里的 TimeZone: Europe/Berlin 会撞上和上面同一个问题——gorm 读原串
func queryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "%2F", "/")
}

// gormTimeZone gorm 的 postgres 驱动从 DSN 里找时区用的正则，原样照抄
// （gorm.io/driver/postgres v1.6.3 postgres.go 的 timeZoneMatcher）。
// 它不走 pgx 的解析，取的是整串里第一处匹配。
var gormTimeZone = regexp.MustCompile("(time_zone|TimeZone|timezone)=(.*?)($|&| )")

// prependPostgresKV 把默认值垫在 key=value 形式 DSN 的前面。
//
// 同一个 key 写两遍 pgx 取后一个（pgconn v5.10.0 parseKeywordValueSettings，
// 实测 connect_timeout=1 … connect_timeout=9 读出来是 9s），使用者写了的
// 因此自然作数。不必先读懂这串 DSN 才知道他写了哪些：那要照抄一遍 pgx 的
// 引号与转义规则，抄错一处就是「密码里的 connect_timeout= 骗过检测」
// 「connect_timeout = 10 没认出来被默认值盖掉」这类静默失效——都真出过。
//
// 唯一的例外是时区：gorm 用上面那个正则取第一处，垫在前面的反倒赢了。
// 所以 DSN 里已经有它认得的时区时，不再垫时区。
func prependPostgresKV(dsn string, defaults map[string]string) string {
	skipTimeZone := gormTimeZone.MatchString(dsn)
	pairs := []string{}
	for _, k := range slices.Sorted(maps.Keys(defaults)) {
		if skipTimeZone && gormTimeZone.MatchString(k+"=") {
			continue
		}
		pairs = append(pairs, k+"="+quoteKV(defaults[k]))
	}
	if len(pairs) == 0 {
		return dsn
	}
	return strings.Join(pairs, " ") + " " + dsn
}

// quoteKV 值里有空格、单引号或反斜杠时按 libpq 规则加引号转义
func quoteKV(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, ` '\`) {
		return v
	}
	e := strings.ReplaceAll(v, `\`, `\\`)
	e = strings.ReplaceAll(e, `'`, `\'`)
	return "'" + e + "'"
}

// seconds 向上取整为整秒，libpq 的 connect_timeout 只接受整数秒且最小为 1
func seconds(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(ceilSeconds(d)/time.Second), 10)
}

// ceilSeconds 向上取整到整秒，<=0 时为 0。与注入 DSN 的 connect_timeout 同一个值
func ceilSeconds(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(math.Ceil(d.Seconds())) * time.Second
}

// millis 转成毫秒整数，PG 的几个超时 GUC 用毫秒。
// 向上取整，理由同 seconds：这几个 GUC 的 0 是「不限制」，500µs 向下取整就成了 0，超时静默消失
func millis(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(int64((d+time.Millisecond-1)/time.Millisecond), 10)
}
