package xgorm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
)

// openMySQL 造 MySQL 的 Dialector，并关掉它在 Initialize 里那次查版本。
//
// 那次 SELECT VERSION() 用的是写死的 context.Background()
// （gorm.io/driver/mysql v1.6.0 mysql.go:130），而且就在 gorm.Open 里、
// 在 xgorm 带重试的建连验证之前——它才是第一次建连。实测（MySQL 8.0.46，
// 对端收下连接却不回话）：只建了 1 个连接，3.0s（= 握手那一读等满 ReadTimeout）
// 后 gorm.Open 直接报 invalid connection，三次重试一次都没轮上；启动期间的
// 退出信号也管不到它，信号之后 2.95s 进程才退。
//
// 版本号本身还要：驱动靠它决定 MariaDB / MySQL 5.x 上改不改得了索引名和列名、
// 支不支持 FOR SHARE、DROP CONSTRAINT，以及 MariaDB 10.5+ 上用不用 RETURNING。
// 所以这次查询挪到 probeMySQLVersion，由建连验证来做：受 ctx 管，跟着重试。
// 关掉之后 gorm.Open 只做 sql.Open 和装配，不碰网络。
func openMySQL(dsn string) gorm.Dialector {
	return mysql.New(mysql.Config{DSN: dsn, SkipInitializeWithVersion: true})
}

// resolveMySQL 用驱动自己的解析器处理 DSN，DSN 里已写的超时不会被覆盖
func resolveMySQL(c ClientConfig) (string, ConnInfo, error) {
	cfg, err := mysqldriver.ParseDSN(c.DSN)
	if err != nil {
		// 不回传驱动的错误：它会把 DSN 片段带在错误信息里，而错误信息会被记下来
		return "", ConnInfo{}, errMalformedDSN
	}
	if c.TLS.Enable {
		if err := checkMySQLTLS(c.DSN, cfg); err != nil {
			return "", ConnInfo{}, err
		}
	}

	if cfg.Timeout == 0 {
		cfg.Timeout = c.DialTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = c.MySQL.ReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = c.MySQL.WriteTimeout
	}

	// parseTime 驱动默认 false：DATETIME / TIMESTAMP 读出来是 []byte，
	// 扫不进 time.Time——实测 go-sql-driver v1.10.1 连 MySQL 8.0.46，
	// 带 CreatedAt 的模型 First 一次就是 unsupported Scan, storing driver.Value
	// type []uint8 into type *time.Time，写进去倒是没问题。GORM 的模型几乎都有
	// 时间字段，所以 DSN 里没写 parseTime 的，这里补成 true；写了（哪怕是 false）
	// 以 DSN 为准。时区跟着驱动的 loc（默认 UTC）：写入时驱动先转成 loc 再格式化，
	// 读出来按 loc 解释，同一个时刻来回不变（实测写 03:04:05+08:00，库里存的是
	// 19:04:05，读回来是 19:04:05 UTC，Equal 为 true）
	if !mysqlParamSet(c.DSN, "parseTime") {
		cfg.ParseTime = true
	}

	// 预算读的是注入之后的值：DSN 里写了的以 DSN 为准。
	// timeout 管建连（拨号），readTimeout 管之后的每一读：握手、认证、探测的那个往返
	// （go-sql-driver v1.10.1 connector.go / connection.go）
	return cfg.FormatDSN(), ConnInfo{
		Driver: "mysql", Addr: cfg.Addr, DB: cfg.DBName,
		ProbeTimeout: mysqlProbeTimeout(cfg.Timeout, cfg.ReadTimeout),
	}, nil
}

// mysqlProbeTimeout 建连加一个往返的预算。
//
// 哪一段配成 0（驱动不限时）就按另一段的量级给：0 说的是「驱动不管」，不是「这一段不花时间」，
// 只拿建连超时当整体预算的话，ReadTimeout: 0 时连接刚建成、登录包还没回来就被判超时。
// 两段都是 0 返回 0，由 probeTimeout 用兜底值
func mysqlProbeTimeout(dial, read time.Duration) time.Duration {
	return cmp.Or(dial, read) + cmp.Or(read, dial)
}

// mysqlParamSet DSN 里有没有写 key 这个参数，写成什么值都算。
//
// 驱动解析完的 Config 分不清「没写」和「写成了默认值」，所以直接看原串，
// 找法照抄驱动的 ParseDSN（go-sql-driver v1.10.1 dsn.go）：最后一个 / 之后、
// 第一个 ? 之后的那段按 & 切开，每项按第一个 = 切成 key 和值。
// 密码里带着 ?parseTime=false 也骗不过它：密码在最后一个 / 之前
func mysqlParamSet(dsn, key string) bool {
	i := strings.LastIndexByte(dsn, '/')
	if i < 0 {
		return false
	}
	_, params, ok := strings.Cut(dsn[i+1:], "?")
	if !ok {
		return false
	}
	for _, kv := range strings.Split(params, "&") {
		if k, _, found := strings.Cut(kv, "="); found && k == key {
			return true
		}
	}
	return false
}

// mysqlAuthFailed 错误号 1045 和 1044，错误链上是 *mysql.MySQLError。
//
// 实测 go-sql-driver v1.10.1 连 MySQL 8.0.46：密码错、用户不存在都是 1045（Access denied for user …）；
// 账号对、但没有这个库的权限是 1044（Access denied … to database …）——
// 库不存在时，没有全局权限的账号拿到的也是 1044 而不是 1049，服务端不告诉它库在不在。
// 不看 SQLSTATE：1045 的是 28000，1044 的却是 42000（语法错误、权限错误共用的那一类）。
func mysqlAuthFailed(err error) bool {
	var myErr *mysqldriver.MySQLError
	return errors.As(err, &myErr) && (myErr.Number == mysqlAccessDenied || myErr.Number == mysqlDBAccessDenied)
}

// MySQL 的两个认证错误号（ER_ACCESS_DENIED_ERROR、ER_DBACCESS_DENIED_ERROR）
const (
	mysqlAccessDenied   = 1045
	mysqlDBAccessDenied = 1044
)

// mysqlErrorCode 服务端报的错误号。
//
// 实测 MySQL 8.0.46：错误原文把参数值原样带出来——1062 是
// Duplicate entry 'a@b.com' for key 'm_err.email'，1366 是
// Incorrect integer value: 'notanint' for column 'n' at row 1，1292 同理。
func mysqlErrorCode(err error) string {
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) {
		return strconv.Itoa(int(myErr.Number))
	}
	return ""
}

// probeMySQLVersion 查服务端版本，按驱动自己的规则设好版本相关的开关。
//
// 作为 MySQL 方言的 Ready，在每次 Ping 成功之后执行，见 Dialect.Ready。
func probeMySQLVersion(ctx context.Context, db *gorm.DB) error {
	d, ok := db.Dialector.(*mysql.Dialector)
	if !ok {
		return nil // 不是我们造的 Dialector，没有开关可设
	}
	var v string
	if err := db.ConnPool.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return err
	}
	return applyMySQLVersion(db, d.Config, v)
}

// applyMySQLVersion 按版本号设老版本不支持的那几项。
//
// 规则抄自 gorm.io/driver/mysql v1.6.0 的 Initialize，升级驱动时要对一遍。
func applyMySQLVersion(db *gorm.DB, c *mysql.Config, v string) error {
	c.ServerVersion = v
	withReturning := false
	switch {
	case strings.Contains(v, "MariaDB"):
		c.DontSupportRenameIndex = true
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportNullAsDefaultValue = true
		withReturning = versionAtLeast(v, "10.5")
	case strings.HasPrefix(v, "5.6."):
		c.DontSupportRenameIndex = true
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportDropConstraint = true
	case strings.HasPrefix(v, "5.7."):
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportDropConstraint = true
	case strings.HasPrefix(v, "5."):
		c.DisableDatetimePrecision = true
		c.DontSupportRenameIndex = true
		c.DontSupportRenameColumn = true
		c.DontSupportForShareClause = true
		c.DontSupportDropConstraint = true
	}
	if strings.Contains(v, "TiDB") {
		c.DontSupportRenameColumnUnique = true
	}
	if withReturning && !c.DisableWithReturning {
		return enableReturning(db)
	}
	return nil
}

// enableReturning 让增删改带上 RETURNING。
//
// 驱动在 Initialize 里注册回调时就把「支不支持 RETURNING」闭包进了
// gorm:create / gorm:update / gorm:delete（gorm v1.31.2 callbacks/create.go:38），
// 那时版本还没查，注册的是不支持的那一份。这里按支持的配置换掉那三个回调，
// 子句列表与驱动在 Initialize 里拼的一致。
func enableReturning(db *gorm.DB) error {
	cfg := &callbacks.Config{
		CreateClauses: append(slices.Clone(mysql.CreateClauses), "RETURNING"),
		UpdateClauses: append(slices.Clone(mysql.UpdateClauses), "RETURNING"),
		DeleteClauses: append(slices.Clone(mysql.DeleteClauses), "RETURNING"),
	}
	cb := db.Callback()
	if err := cb.Create().Replace("gorm:create", callbacks.Create(cfg)); err != nil {
		return err
	}
	if err := cb.Update().Replace("gorm:update", callbacks.Update(cfg)); err != nil {
		return err
	}
	if err := cb.Delete().Replace("gorm:delete", callbacks.Delete(cfg)); err != nil {
		return err
	}
	cb.Create().Clauses = cfg.CreateClauses
	cb.Update().Clauses = cfg.UpdateClauses
	cb.Delete().Clauses = cfg.DeleteClauses
	return nil
}

// versionAtLeast v 是否不低于 floor，逐段比较数字部分。抄自驱动的 checkVersion
func versionAtLeast(v, floor string) bool {
	if v == floor {
		return true
	}
	vs, ms := strings.Split(v, "."), strings.Split(floor, ".")
	for i, s := range vs {
		if i >= len(ms) {
			return true
		}
		a, b := leadingInt(s), leadingInt(ms[i])
		if a != b {
			return a > b
		}
	}
	return false
}

var leadingDigits = regexp.MustCompile(`^\d+`)

func leadingInt(s string) int {
	n, _ := strconv.Atoi(leadingDigits.FindString(s))
	return n
}

// mysqlDriverLogger 把 go-sql-driver 自己的日志接到 slog 上。
//
// go-sql-driver v1.10.1 的默认 logger 是 log.New(os.Stderr, "[mysql] ", …)，
// 往 stderr 写纯文本（`[mysql] 2026/09/24 10:00:00 packets.go:58 read tcp …: i/o timeout`），
// 不进 slog 就不是 JSON，进不了日志平台。它的日志没有级别，写出来的都是出错时的补充
// （读包失败、关掉坏连接、认证插件回退），调用方同时也拿到了错误，所以一律记成 WARN。
// 驱动不给 ctx，这些日志不带 trace_id。
type mysqlDriverLogger struct{}

func (mysqlDriverLogger) Print(v ...any) {
	slog.Warn("xgorm go-sql-driver log", "detail", strings.TrimSpace(fmt.Sprint(v...)))
}
