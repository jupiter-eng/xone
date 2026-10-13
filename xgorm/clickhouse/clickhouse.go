// Package clickhouse 给 xgorm 加上 ClickHouse 驱动。
//
// 匿名 import 即可，不需要写任何代码：
//
//	import (
//		"github.com/jupiter-eng/xone/xgorm"
//		_ "github.com/jupiter-eng/xone/xgorm/clickhouse"
//	)
//
// 然后配置里写 Driver: clickhouse，拿到的还是原生的 *gorm.DB：
//
//	XGorm:
//	  Driver: clickhouse
//	  DSN: "${CH_DSN}"     # clickhouse://user:pass@host:9000/db
//	  TLS: {Enable: true, CAFile: /etc/ssl/ch-ca.pem}   # 可选：native 与 https:// 都走它，见 tls.go
//
// 为什么是独立的 module：实测一个只 import xgorm 的应用模块图是 68 个（不含应用自己），
// 加上这个包变成 131 个（go list -deps 里的非标准库包 165 → 208；clickhouse-go v2.48.0）。
// 多出来的大头是 Docker（moby）和 testcontainers —— clickhouse-go 用它们跑集成测试，
// 而 go.mod 分不出「只测试用」，所以它们落在主 require 块里，一路传给每个使用者。
// Go 的 MVS 按模块图强加版本要求，不用 ClickHouse 的人不该为它付这个钱。
//
// 驱动版本要盯着：gorm.io/driver/clickhouse v0.6.1 的 go.mod 里还积着
// 126 个 cloud.google.com/* 的陈年 indirect 项，用它模块图是 733 个；
// 升到 v0.7.0 直接降到 146（那时 clickhouse-go 是 v2.30.0，升到 v2.48.0 又降到 128；xgorm 带上 xtrace 之后是 132）。
package clickhouse

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	chproto "github.com/ClickHouse/ch-go/proto"
	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/hashicorp/go-version"
	"gorm.io/driver/clickhouse"
	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

// Driver 配置里 Driver 那一项要写的值
const Driver xgorm.Driver = "clickhouse"

// dialTimeoutKey ClickHouse DSN 里建连超时对应的 query 参数
const dialTimeoutKey = "dial_timeout"

// driverDialTimeout dial_timeout 为 0 时驱动用的值（clickhouse-go v2.48.0 clickhouse_options.go setDefaults，
// Open / OpenDB / Connector 都先过它）。升级驱动时对一下
const driverDialTimeout = 30 * time.Second

// schemes 驱动认的 URL scheme。http / https 走 HTTP 协议，其余走 native
var schemes = []string{"clickhouse://", "tcp://", "http://", "https://"}

// DSN 解不出来时报的错。一律不回显 DSN：url.Parse 和驱动的解析错误里
// 都带着 DSN 片段，而 DSN 多半带着密码
var (
	errNotURL = errors.New("DSN must be a URL starting with clickhouse://, tcp://, http:// or https:// " +
		"(details omitted to keep credentials out of logs)")
	errMalformedDSN = errors.New("failed to parse DSN, check the format of " + xgorm.ConfigKey +
		" (details omitted to keep credentials out of logs)")
	errMalformedQuery = errors.New("failed to parse the query part of the DSN, check the format of " + xgorm.ConfigKey +
		" (a literal % in a password must be written as %25; details omitted to keep credentials out of logs)")
)

// dialect 注册进 xgorm 的那一份。单独成变量，测试才看得到它接的是哪几个函数
var dialect = xgorm.Dialect{
	Name:       Driver,
	Open:       open,
	OpenTLS:    openTLS,
	Resolve:    resolve,
	Ready:      probeVersion,
	AuthFailed: authFailed,
	ErrorCode:  errorCode,
}

// init 只注册，不初始化。真正建连由 xgorm 在框架的 StageClient 里做。
func init() { xgorm.RegisterDialect(dialect) }

// open 造 Dialector，并关掉它在 Initialize 里那次查版本。
//
// 那次 SELECT version() 用的是写死的 context.Background()
// （gorm.io/driver/clickhouse v0.7.0 的 Initialize），而且就在 gorm.Open 里：
// 实测对一个收下连接却不回话的地址，ctx 早已取消也要等满 dial_timeout
// 才返回（300ms 配 300ms），失败了直接报错——xgorm 的三次建连重试一次都
// 没轮上，一次抖动就让服务起不来。
//
// 版本号本身还要：驱动靠它决定老版本上改不改得了列名（< 20.4）、
// 列类型带不带精度（< 21.11），不查的话迁移会在老集群上生成它不认的 DDL。
// 所以这次查询挪到 probeVersion，由 xgorm 在建连验证里做：受 ctx 管，跟着重试。
func open(dsn string) gorm.Dialector {
	return clickhouse.New(clickhouse.Config{DSN: dsn, SkipInitializeWithVersion: true})
}

// probeVersion 查服务端版本，按驱动自己的规则设好那两个开关。
//
// 规则抄自 v0.7.0 的 Initialize，升级驱动时要对一遍。
func probeVersion(ctx context.Context, db *gorm.DB) error {
	d, ok := db.Dialector.(*clickhouse.Dialector)
	if !ok {
		return nil // 不是我们造的 Dialector，没有开关可设
	}
	var v string
	if err := db.ConnPool.QueryRowContext(ctx, "SELECT version()").Scan(&v); err != nil {
		return err
	}
	d.Version = v
	applyVersion(d.Config, v)
	return nil
}

// applyVersion 按版本号设老版本不支持的两项
func applyVersion(c *clickhouse.Config, v string) {
	parsed, err := version.NewVersion(v)
	if err != nil {
		return // 解不出来就按新版本处理，与驱动一致
	}
	noRename, _ := version.NewConstraint("< 20.4")
	noPrecision, _ := version.NewConstraint("< 21.11")
	if noRename.Check(parsed) {
		c.DontSupportRenameColumn = true
	}
	if noPrecision.Check(parsed) {
		c.DontSupportColumnPrecision = true
	}
}

// resolve 把配置里的建连超时注入 DSN，并解出可安全记录的连接信息
//
// 只认 URL 形式（clickhouse://user:pass@host:9000/db?k=v），别的一律拒绝。
// 驱动并不接受裸的 host:port（实测 clickhouse.ParseDSN("10.255.255.1:9000")
// 报 first path segment in URL cannot contain colon），而原样透传的话，
// 驱动建连时的解析错误会连同整串 DSN、包括明文密码一起进日志。
// scheme 拼错（clickhous://）驱动倒是认，但会跳过这里的超时注入，
// 前面多一个空格则又是一次带着整串 DSN 的解析错误——都在这里挡掉。
func resolve(c xgorm.ClientConfig) (string, xgorm.ConnInfo, error) {
	if !isURL(c.DSN) {
		return "", xgorm.ConnInfo{}, errNotURL
	}

	u, err := url.Parse(c.DSN)
	if err != nil {
		// 不回传原始错误：url.Parse 的错误里带着整串 DSN，而错误会被记下来
		return "", xgorm.ConnInfo{}, errMalformedDSN
	}

	// 显式 ParseQuery 而不是 u.Query()：后者会把错误吞掉，只返回解得出的那部分。
	// 于是密码里带一个字面 % （构成非法的百分号转义）时，那一项会被静默丢掉，
	// 回写之后 DSN 里就没有密码了——服务报「认证失败」，而配置文件里密码明明写着。
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", xgorm.ConnInfo{}, errMalformedQuery
	}
	if c.TLS.Enable {
		if err := checkTLS(u, q); err != nil {
			return "", xgorm.ConnInfo{}, err
		}
	}

	// 使用者在 DSN 里显式写了的，一律不覆盖——配置里的值只是默认值。
	// DialTimeout 为 0 不注入；写成 ClickHouse 认的时长字符串（300ms、5s）。
	// 接在原 query 后面，使用者写的那部分一个字节都不动（同 xgorm 的 injectPostgresURL）：
	// 解开再 q.Encode() 的话参数按 key 重排，a,b/c 成了 a%2Cb%2Fc
	if c.DialTimeout > 0 && !q.Has(dialTimeoutKey) {
		u.RawQuery = appendQuery(u.RawQuery, dialTimeoutKey, c.DialTimeout.String())
	}
	dsn := u.String()

	// 用驱动自己的解析器再过一遍：它的错误同样可能带着凭证（http_proxy 解析失败时
	// 回显的是整个代理地址），留到建连时才报就是原样进日志。
	// 这里报出来的只有一句不带内容的话；常见的是 https:// 没配 secure=true（开了 TLS 块时不必配，见 parseDSN）
	opts, err := parseDSN(dsn, c.TLS.Enable)
	if err != nil {
		return "", xgorm.ConnInfo{}, errMalformedDSN
	}

	// 预算按驱动读出来的 dial_timeout 算：DSN 里写了更长的，xgorm 建连验证的预算
	// 才会跟着放宽。驱动拿它管 TCP 建连，握手阶段又拿它设整条连接的 deadline
	// （v2.48.0 conn_handshake.go），往返没有单独可依的配置，按同一量级再给一份。
	// 两处都没写时 ParseDSN 读出来是 0，驱动建连时才补成 driverDialTimeout：预算也按补完的算，
	// 按 0 算的话 xgorm 只剩 1s 兜底，驱动还愿意等的慢握手被建连验证先判了超时
	//
	// 地址取驱动解出来的第一个：多主机写法 clickhouse://u:p@h1:9000,h2:9000/db 里
	// URL 的 Host 是整串 "h1:9000,h2:9000"，驱动按逗号切开、依次去连（默认 in_order）。
	// 整串当地址的话 Span 的 server.address 是整串、没有 server.port（SplitHostPort 解不开），
	// 建连日志里也不是一个「主机:端口」。和 PostgreSQL 的多主机一样记第一个。
	// ParseDSN 在 Host 为空时报错，走到这里 Addr 至少有一项。
	//
	// 库名也取驱动解出来的：路径和 ?database= 两处都能写，query 里的盖掉路径
	// （v2.48.0 clickhouse_options.go fromDSN），只看路径的话日志里的 db 不是真正连上的那个
	return dsn, xgorm.ConnInfo{
		Driver:       string(Driver),
		Addr:         opts.Addr[0],
		DB:           opts.Auth.Database,
		ProbeTimeout: 2 * cmp.Or(opts.DialTimeout, driverDialTimeout),
	}, nil
}

// appendQuery 在 raw query 后面接一个 key=value，raw 原样保留
func appendQuery(raw, key, value string) string {
	pair := url.QueryEscape(key) + "=" + url.QueryEscape(value)
	if raw == "" {
		return pair
	}
	return raw + "&" + pair
}

// authCodes 服务端拒绝凭证时的错误码，取自 ch-go 的类型化常量（与服务端
// src/Common/ErrorCodes.cpp 同名）：新版本的服务端密码错、用户不存在一律报
// 516 AUTHENTICATION_FAILED，192 / 193 / 194 是老版本分开报的三种。
//
// native 协议握手时服务端回 Exception 包，驱动原样返回 *clickhouse.Exception（v2.48.0 conn.go 的
// exception()），错误链上 errors.As 得到。HTTP 协议下驱动把非 200 的响应解析成 *clickhouse.HTTPError，
// 里面包着同一个 *clickhouse.Exception（v2.48.0 conn_http_errors.go），同样认得出。
// 实测（e2e，ClickHouse 24.8.14）两种协议下密码错、用户不存在都是 code: 516，只试 1 次
// （HTTP 的状态码是 403）；库不存在是 81，不在这里。v2.30.0 的 HTTP 错误只是一段拼好的文本，认不出
var authCodes = []chproto.Error{
	chproto.ErrAuthenticationFailed, // 516
	chproto.ErrUnknownUser,          // 192
	chproto.ErrWrongPassword,        // 193
	chproto.ErrRequiredPassword,     // 194
}

// authFailed 服务端是否拒绝了这组凭证，见 authCodes
func authFailed(err error) bool {
	var ex *chgo.Exception
	return errors.As(err, &ex) && slices.Contains(authCodes, chproto.Error(ex.Code))
}

// errorCode 服务端报的错误码。原文（Exception.Message）里可能就有参数值，
// 比如解析不了的输入会被原样引出来，所以日志和 Span 只记码。
// HTTP 协议下错误链上同样有 *clickhouse.Exception（见 authCodes）；解析不出来的（比如代理回的 502）
// 交给 xgorm 按客户端的错处理：网络错误、ctx 取消这类照原文记，其余只说原文收掉了
func errorCode(err error) string {
	var ex *chgo.Exception
	if errors.As(err, &ex) {
		return strconv.Itoa(int(ex.Code))
	}
	return ""
}

// isURL 判断是不是驱动认的 URL 形式
func isURL(dsn string) bool {
	return slices.ContainsFunc(schemes, func(p string) bool { return strings.HasPrefix(dsn, p) })
}
