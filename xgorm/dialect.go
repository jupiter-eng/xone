package xgorm

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// ConnInfo 可以安全写进日志的连接信息
//
// 从 DSN 里解出来的、确定不含凭证的那几项。本包不打印 DSN，
// 所以也就没有什么需要脱敏——理由见 dsn.go 开头。
type ConnInfo struct {
	Driver string // 驱动名，如 mysql
	Addr   string // 主机:端口
	DB     string // 库名

	// ProbeTimeout 建连验证时单次探测的预算：建连（含握手、认证）加一次往返。
	// 0 表示方言推算不出来，xgorm 按 2 × DialTimeout 算，还是 0 就用 1s 兜底。
	//
	// 由方言按驱动从最终 DSN 里读出来的超时推算：使用者在 DSN 里写了比配置
	// 更长的超时，驱动就等那么久，预算要跟着放宽，不能在驱动自己放弃之前
	// 就把一次慢一点但合法的建连判超时、重试。
	ProbeTimeout time.Duration
}

// Dialect 一种数据库方言。
//
// mysql 和 postgres 内置。其余驱动由独立的 module 提供，在自己的 init 里
// 注册进来，使用者匿名 import 即可：
//
//	import _ "github.com/jupiter-eng/xone/xgorm/clickhouse"
//
// 拆成独立 module 是因为驱动很重。实测一个只 import xgorm 的应用模块图是
// 68 个（不含应用自己），加上 ClickHouse 驱动变成 131 个（go list -deps 里的非标准库包 165 → 208，clickhouse-go v2.48.0）——多出来的
// 大头是 Docker 和 testcontainers，因为 clickhouse-go 把集成测试用的它们
// 写在了自己 go.mod 的主 require 块里，而 go.mod 分不出「只测试用」。
// Go 的 MVS 正是按模块图把版本要求强加给使用者的，哪怕他只用 MySQL、
// 一个 ClickHouse 的包都没 import。
type Dialect struct {
	// Name 驱动名，即配置里 Driver 那一项要写的值
	Name Driver

	// Open 造 GORM 的 Dialector
	Open func(dsn string) gorm.Dialector

	// OpenTLS 同 Open，但连接一律走 cfg 给出的 TLS：配置里 TLS.Enable 开着时 xgorm 调它而不是 Open。
	// 可以留空，留空的驱动配了 TLS 块是配置错误（TLS 就写在它自己的 DSN 里）。
	//
	// cfg 由 xtls.Config.Build 装出来，ServerName 没配时是空的，按连接地址补上是这里的事。
	// 不许在 cfg 之外留一条明文的退路：配了 TLS 的人要的是「连不上 TLS 就失败」。
	// DSN 里自己也写了 TLS 参数的，由 Resolve 报配置错误，到不了这里。
	// 返回普通 error，xgorm 包成 op 为 config 的 xerror，同样别回传带 DSN 片段的原始错误。
	OpenTLS func(dsn string, cfg *tls.Config) (gorm.Dialector, error)

	// Resolve 把配置里的超时等参数注入 DSN，并解出可安全记录的连接信息。
	//
	// 留空表示 DSN 原样使用、连接信息里只有驱动名——对一个只想先跑起来的
	// 驱动这是合理的起点，超时写进 DSN 里一样有效。
	// 连接信息里填上 ProbeTimeout 的话，建连验证的预算按它走；
	// 不填就是 2 × DialTimeout。
	//
	// 返回普通 error 即可，由 xgorm 在模块边界包成一层 xerror（op 为 config）。
	// DSN 解不出来时别回传解析器的原始错误：那里面带着 DSN 片段，
	// 而错误信息会被记下来。
	Resolve func(c ClientConfig) (dsn string, info ConnInfo, err error)

	// Ready 建连验证通过之后调用，给驱动一个受 ctx 管的地方做它自己的初始化查询。
	// 可以留空。
	//
	// 有的 Dialector 在 Initialize 里查一次服务端版本，用的是写死的
	// context.Background()：退出信号管不到它，失败了也轮不到重试，
	// 因为它发生在 gorm.Open 里、在 xgorm 的建连验证之前。这类驱动在 Open 里
	// 把那次查询关掉、挪到这里：它和每次 Ping 一起执行、一起重试，受 ctx 约束。
	// 返回普通 error，xgorm 会包成 op 为 connect 的 xerror。
	Ready func(ctx context.Context, db *gorm.DB) error

	// AuthFailed 这个错误是不是服务端拒绝了这组凭证。可以留空，留空就是认不出。
	//
	// 认得出来的，建连验证不再重试，报的是 "authentication to <addr> failed"；
	// 认不出来的照常重试，报 "cannot reach <addr>"。
	AuthFailed func(error) bool

	// ErrorCode 服务端报错时的错误码（PostgreSQL 的 SQLSTATE、MySQL 的错误号），
	// 不是服务端报的错返回空串。可以留空。
	//
	// SQL 日志的 error 字段和 Span 的状态只记这个码，不记错误原文：服务端的错误
	// 原文会把参数值带出来——实测 MySQL 8.0 的 1062 是
	// Duplicate entry 'a@b.com' for key 'm_err.email'，PG 16 的 22P02 是
	// invalid input syntax for type integer: "notanint"。返回给调用方的错误不变。
	// 留空的话认不出服务端的错，日志和 Span 里照原文记。
	ErrorCode func(error) string
}

// dialects 已注册的方言
//
// 加锁而不是裸 map：只在各包 init 里注册的话确实用不着——init 全部跑完、
// 单协程，之后 main 才开始读。但 RegisterDialect 是导出的，没人拦着在运行时调它，
// 而 New 可能正在别的协程里查表；map 的并发读写是会直接崩的那一种错。
var (
	dialectMu sync.RWMutex
	dialects  = map[Driver]Dialect{}
)

// RegisterDialect 注册一个驱动。
//
// 同名重复注册直接 panic：两个 Dialect 抢同一个名字时，选哪个都可能让服务
// 连到一个它以为自己没在连的地方。这是 import 期的问题，不该留到运行时。
func RegisterDialect(d Dialect) {
	if d.Name == "" {
		panic("xgorm: Dialect.Name must not be empty")
	}
	if d.Open == nil {
		panic(fmt.Sprintf("xgorm: Dialect %q has no Open", d.Name))
	}

	dialectMu.Lock()
	defer dialectMu.Unlock()
	if _, dup := dialects[d.Name]; dup {
		panic(fmt.Sprintf("xgorm: Dialect %q is already registered", d.Name))
	}
	dialects[d.Name] = d
}

// dialector 按有没有 TLS 选 Open 还是 OpenTLS。
// 开了 TLS 而方言没有 OpenTLS 的，ClientConfig.Validate 已经拦下了
func (d Dialect) dialector(dsn string, cfg *tls.Config) (gorm.Dialector, error) {
	if cfg == nil {
		return d.Open(dsn), nil
	}
	return d.OpenTLS(dsn, cfg)
}

// resolve 把配置里的超时等参数注入 DSN，并解出可安全记录的连接信息。
// 具体怎么解由方言的 Resolve 决定
func (d Dialect) resolve(c ClientConfig) (string, ConnInfo, error) {
	if d.Resolve == nil {
		// 没提供解析逻辑：DSN 原样用，日志里只写得出驱动名
		return c.DSN, ConnInfo{Driver: string(c.Driver)}, nil
	}
	return d.Resolve(c)
}

// authFailed 方言认不认得出这是认证失败，方言没提供就是认不出
func (d Dialect) authFailed(err error) bool {
	return err != nil && d.AuthFailed != nil && d.AuthFailed(err)
}

// errorCode 服务端的错误码，方言没提供或不是服务端的错时为空
func (d Dialect) errorCode(err error) string {
	if err == nil || d.ErrorCode == nil {
		return ""
	}
	return d.ErrorCode(err)
}

// redactedError 要写进日志和 Span 的错误文本：服务端报的错只留错误码；
// 客户端这一侧的只有 clientSafeError 认得的几类照记，其余一律是 clientErrorOmitted。
//
// 客户端的错也会带参数值，实测：pgx v5.10.0 编码不了的参数用 %#v 整个写进错误
// （unable to encode xgorm.rvSecret{Token:"…"} into text format for text (OID 25)），
// database/sql 扫描失败用 %q 写出读回来的值（converting driver.Value type []uint8 ("…") to a int）。
// 所以和 xredis 一样按白名单放行，不是按黑名单去猜哪些危险。返回给调用方的错误不变
func (d Dialect) redactedError(err error) (text, code string) {
	if code = d.errorCode(err); code != "" {
		return fmt.Sprintf("%s error %s (message omitted, it may contain parameter values)", d.Name, code), code
	}
	if safe := clientSafeError(err); safe != nil {
		return safe.Error(), ""
	}
	return clientErrorOmitted, ""
}

// clientSafeError 错误链上第一个原文可以照记的错误：网络错误（地址、超时，排查正要看这个）、
// ctx 取消 / 超时，以及 database/sql、驱动、GORM 自己的那些固定文案的哨兵错误。没有就是 nil。
//
// 返回的是链上认出来的那一个，不是整条链的原文：外面包的那几层不一定干净——
// GORM 的 AddError 用 "%v; %w" 把前一个错误的原文拼在前面（gorm v1.31.2 gorm.go AddError），
// 后一个是 ErrInvalidData 的话，整条链的原文里照样是前一个带着的参数值
func clientSafeError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne
	}
	for _, safe := range safeErrors {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return nil
}

// safeErrors 文案固定、不带任何值的哨兵错误
var safeErrors = []error{
	context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF,
	sql.ErrConnDone, sql.ErrTxDone, sql.ErrNoRows, driver.ErrBadConn, driver.ErrSkip,
	mysqldriver.ErrInvalidConn, mysqldriver.ErrMalformPkt, mysqldriver.ErrNoTLS, mysqldriver.ErrPktSync,
	mysqldriver.ErrPktSyncMul, mysqldriver.ErrPktTooLarge, mysqldriver.ErrBusyBuffer,
	gorm.ErrRecordNotFound, gorm.ErrInvalidTransaction, gorm.ErrNotImplemented, gorm.ErrMissingWhereClause,
	gorm.ErrUnsupportedRelation, gorm.ErrPrimaryKeyRequired, gorm.ErrModelValueRequired,
	gorm.ErrModelAccessibleFieldsRequired, gorm.ErrSubQueryRequired, gorm.ErrInvalidData, gorm.ErrUnsupportedDriver,
	gorm.ErrRegistered, gorm.ErrInvalidField, gorm.ErrEmptySlice, gorm.ErrDryRunModeUnsupported, gorm.ErrInvalidDB,
	gorm.ErrInvalidValue, gorm.ErrInvalidValueOfLength, gorm.ErrPreloadNotAllowed, gorm.ErrDuplicatedKey,
	gorm.ErrForeignKeyViolated, gorm.ErrCheckConstraintViolated,
}

// clientErrorOmitted 客户端这一侧的错、又不在 clientSafeError 认得的那几类里时，日志和 Span 里写的话
const clientErrorOmitted = "client error (message omitted, it may contain parameter values)"

// lookupDialect 取一个已注册的方言
func lookupDialect(name Driver) (Dialect, bool) {
	dialectMu.RLock()
	defer dialectMu.RUnlock()
	d, ok := dialects[name]
	return d, ok
}

// Drivers 返回当前已注册的驱动名，按字母序。
//
// 用来回答「Driver 写对了吗、对应的 module import 了吗」——
// 这是配错驱动时唯一分得清「名字拼错」和「忘了 import」的办法。
func Drivers() []Driver {
	dialectMu.RLock()
	defer dialectMu.RUnlock()
	return slices.Sorted(maps.Keys(dialects))
}

// 内置两个。直接填进 map 而不是在 init 里注册：
// 这样「内置的」和「外部注册的」在源码上一眼可分。
func init() {
	dialects[DriverMySQL] = Dialect{
		Name: DriverMySQL, Open: openMySQL, OpenTLS: openMySQLTLS, Resolve: resolveMySQL, Ready: probeMySQLVersion,
		AuthFailed: mysqlAuthFailed, ErrorCode: mysqlErrorCode,
	}
	dialects[DriverPostgres] = Dialect{
		Name: DriverPostgres, Open: openPostgres, OpenTLS: openPostgresTLS, Resolve: resolvePostgres,
		AuthFailed: postgresAuthFailed, ErrorCode: postgresErrorCode,
	}
}
