// Package xgorm 按配置装好 GORM，使用者拿到的是原生的 *gorm.DB。
//
//	err := xgorm.CWithCtx(ctx).First(&u, id).Error
//
// 支持 MySQL 与 PostgreSQL，连接池、超时、慢查询日志、链路、连接池指标
// 全部由配置决定，业务代码里没有任何初始化。
package xgorm

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XGorm"

// DefaultName C() 不带参数时取的那个实例的名字
const DefaultName = xconfig.DefaultClientName

// Driver 数据库驱动
type Driver string

const (
	DriverMySQL    Driver = "mysql"
	DriverPostgres Driver = "postgres"
)

// Config 本模块的配置。两种写法：
//
//	XGorm:                  # 单实例，直接写字段，名字就是 default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # 多实例，按名字写
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}", MaxOpenConns: 5}
type Config struct {
	// Clients 按名字组织的实例。单实例写法会被规整成一个名为 default 的实例。
	Clients map[string]ClientConfig
}

// ClientConfig 一个数据库实例的配置
type ClientConfig struct {
	// Driver 驱动：mysql / postgres。默认 postgres。
	//
	// 其余驱动住在自己的 module 里，匿名 import 即可注册：
	//
	//	import _ "github.com/jupiter-eng/xone/xgorm/clickhouse"
	Driver Driver `yaml:"Driver"`

	// DSN 连接串，必填。
	//
	// 建议写成 "${DB_DSN}"：凭证不该进版本库，漏配时启动就失败。
	//
	// MySQL 的 DSN 里没写 parseTime 的，补成 parseTime=true：驱动默认 false，
	// DATETIME 扫不进 time.Time（实测 go-sql-driver v1.10.1，带 CreatedAt 的模型
	// First 一次就报 unsupported Scan）。写了的以 DSN 为准。
	DSN string `yaml:"DSN"`

	// DialTimeout 建连超时。默认 500ms。
	//
	// MySQL 注入 DSN 的 timeout，PostgreSQL 注入 connect_timeout（向上取整为秒）。
	// 配 0 就不注入、用驱动自己的（两个驱动都是不限时）。不能为负，下面的时长都是。
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// MySQL 仅在 Driver 为 mysql 时生效
	MySQL MySQLConfig `yaml:"MySQL"`

	// Postgres 仅在 Driver 为 postgres 时生效
	Postgres PostgresConfig `yaml:"Postgres"`

	// TLS 连库时走不走 TLS。默认不走（DSN 里自己写的 TLS 参数照旧生效）。
	// 字段和规则各模块共用，见 xtls.Config。
	//
	// 开着时 TLS 全由这一块决定：证书一律校验，**不会退回明文**。DSN 里再写 TLS 参数
	// （PostgreSQL 的 sslmode、sslrootcert 等 ssl 开头的那几个，MySQL 的 tls）
	// 是配置错误——两处说法不一，哪处作数都会让另一处白写。
	// 驱动怎么接、量出来的行为见 xtls/README.md「行为与实测」。
	TLS xtls.Config `yaml:"TLS"`

	// MaxOpenConns 最大连接数。默认 50。
	MaxOpenConns int `yaml:"MaxOpenConns"`

	// MaxIdleConns 最大空闲连接数。默认 50。
	//
	// 配 0 是「一条空闲连接都不留」，每次查询都要重新建连，通常不是你想要的。
	// 比 MaxOpenConns 大也没关系，database/sql 会自己压到 MaxOpenConns。
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxLifetime 连接最长存活时间。默认 5m，配 0 不限。
	MaxLifetime time.Duration `yaml:"MaxLifetime"`

	// MaxIdleTime 空闲连接最长存活时间。默认 5m，配 0 不限。
	MaxIdleTime time.Duration `yaml:"MaxIdleTime"`

	// Log 是否把 GORM 的 SQL 日志接到 slog 上。默认关闭。
	//
	// SQL 执行失败时 error 字段只记服务端的错误码（另有 error_code 字段），
	// 不记原文：实测 MySQL 8.0 的 1062 原文是 Duplicate entry 'a@b.com' for key …，
	// 参数值就在里面。返回给调用方的错误不变。
	Log bool `yaml:"Log"`

	// SlowThreshold 超过这个耗时的 SQL 记一条 warn 日志。默认 3s，需 Log 开启，配 0 不记。
	SlowThreshold time.Duration `yaml:"SlowThreshold"`

	// IgnoreNotFound 是否不把「没查到记录」当错误记日志。默认 false。
	IgnoreNotFound bool `yaml:"IgnoreNotFound"`

	// DisableForeignKeyConstraintWhenMigrating AutoMigrate 建表时不建外键约束。默认 false，即 GORM 的默认：按模型里的关联建外键。
	//
	// 只影响迁移建表，查询和关联照常：实测 GORM v1.31.2 + PostgreSQL 16，Owner has many Order 的两张表，
	// 默认建出 fk_owners_orders，插一条 owner_id 不存在的 order 被拒；开着时不建外键，这条照样插进去，
	// Preload("Orders") 照常查得出来。引用完整性从此由你的代码保证。已经建好的外键不会被删。
	DisableForeignKeyConstraintWhenMigrating bool `yaml:"DisableForeignKeyConstraintWhenMigrating"`

	// SkipDefaultTransaction 单条的 Create / Update / Delete 不再自动包一层事务。默认 false（GORM 的默认：包）。
	//
	// 省掉的是 BEGIN / COMMIT 两个来回：实测本机回环、500 次单条 Create，PostgreSQL 16 每次约 500µs → 230µs，
	// MySQL 8.0 约 1.0ms → 0.9ms。代价是 BeforeCreate 这类钩子里的写和主语句不再同进退；显式的 Transaction 不受影响。
	SkipDefaultTransaction bool `yaml:"SkipDefaultTransaction"`

	// PrepareStmt 把执行过的 SQL 缓存成预编译语句，后面同样的 SQL 直接复用。默认 false。
	//
	// 实测本机回环、2000 次同一条带参数的查询：MySQL 8.0 每次约 180µs → 94µs（不开时驱动每条都要
	// prepare / execute / close 三个来回），PostgreSQL 16 约 100µs → 85µs（pgx 本来就缓存语句，差别不大）。
	// 缓存上限和过期用 GORM 的默认值（不限条数、1h）。经 PgBouncer 这类连接池中间件时（尤其 transaction 模式），先在那套环境里量过再开。
	PrepareStmt bool `yaml:"PrepareStmt"`

	// CreateBatchSize Create 一个切片时每条 INSERT 最多带几行。默认 0：整个切片一条 INSERT。
	//
	// 实测 10 行、配 3：PostgreSQL 和 MySQL 都发 4 条 INSERT（3+3+3+1），10 行都写进去。
	// 切片很大时用它避开单条 SQL 的参数上限（PostgreSQL 65535 个）和包大小上限（MySQL max_allowed_packet）。
	CreateBatchSize int `yaml:"CreateBatchSize"`

	// Trace 是否挂 OpenTelemetry 插件。默认开启。
	//
	// 没装链路时它产出的是 noop Span，代价可以忽略，所以默认就开着。
	// 属性按 OTel 数据库语义约定 v1.43.0 写（db.system.name、db.namespace、
	// db.query.text、db.operation.name、server.address / server.port），
	// db.query.text 带占位符、不带参数值；服务端报错时只记错误码（db.response.status_code）。
	Trace bool `yaml:"Trace"`

	// Metric 是否导出连接池指标 db_pool_*（连接数、等待次数、等待时长等），标签 name 是实例名。默认开启。
	//
	// 指标在被抓取时才读 sql.DB.Stats()，不额外占协程。
	// 按实例生效：配了 Metric: false 的实例不出现在 /metrics 里。
	Metric bool `yaml:"Metric"`
}

// MySQLConfig MySQL 特有的配置
type MySQLConfig struct {
	// ReadTimeout 读超时，对应 DSN 的 readTimeout。默认 3s，配 0 不注入（驱动不限时）。
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout 写超时，对应 DSN 的 writeTimeout。默认 5s，配 0 不注入（驱动不限时）。
	WriteTimeout time.Duration `yaml:"WriteTimeout"`
}

// PostgresConfig PostgreSQL 特有的配置
//
// 这些值在建连时随 startup message 发给服务端，成为会话级 GUC。
// DSN 里已经显式写了同名 key 的话，以 DSN 为准。
type PostgresConfig struct {
	// StatementTimeout 单条 SQL 最长执行时间。默认不限制。
	//
	// 它是服务端的计时，管的是服务端收到语句之后执行了多久。PG 客户端这一侧
	// 没有 MySQL 那样的读超时：连接池里的连接遇上不回话的对端（卡死的进程、
	// 半路断掉的网络），调用方的 ctx 没有截止时间的话，查询一直等到调用方放弃。
	// 实测把这里配成 1s 照样等：30s 后仍没返回——语句根本没到服务端，它无从计时。
	// 管住这一段的只有调用方的截止时间：用 CWithCtx 传一个带 deadline 的 ctx，
	// 实测给 200ms 就在 200ms 返回。
	StatementTimeout time.Duration `yaml:"StatementTimeout"`

	// LockTimeout 等锁的最长时间。默认不限制。
	LockTimeout time.Duration `yaml:"LockTimeout"`

	// IdleInTxTimeout 事务中空闲多久就断连。默认不限制。
	IdleInTxTimeout time.Duration `yaml:"IdleInTxTimeout"`

	// Params 其它任意 PG 运行时参数，原样拼进 DSN。
	//
	// pgx 自己的连接参数也写在这里，比如经 PgBouncer 的事务池连库时要写
	// default_query_exec_mode: exec——pgx 默认的 cache_statement 用具名预备语句，
	// 实测 PgBouncer 1.22 事务池（max_prepared_statements 为 0，1.24 之前的默认值）
	// 20 个协程并发 1000 条查询，701 条报 prepared statement "stmtcache_…" already exists
	// （42P05）。细节和各模式的代价见 xgorm/README.md「PostgreSQL」。
	Params map[string]string `yaml:"Params"`
}

// DefaultClientConfig 单个实例的全部默认值集中在这里
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Driver:        DriverPostgres,
		DialTimeout:   500 * time.Millisecond,
		MySQL:         MySQLConfig{ReadTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second},
		MaxOpenConns:  50,
		MaxIdleConns:  50,
		MaxLifetime:   5 * time.Minute,
		MaxIdleTime:   5 * time.Minute,
		SlowThreshold: 3 * time.Second,
		Trace:         true,
		Metric:        true,
	}
}

// DefaultConfig 默认没有任何实例——没配 XGorm 就不该连任何数据库
func DefaultConfig() Config { return Config{} }

// loadConfig 读配置文件里的这一块。单实例和多实例两种写法都收，
// 每个实例先铺上 DefaultClientConfig 再解，规则见 xconfig.UnmarshalClients
func loadConfig() (Config, error) {
	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
	return Config{Clients: clients}, err
}

// Validate 检查整块配置：每个实例各查一遍，报错时说清是哪个实例。
//
// 按配置文件读的时候用不着它：xconfig.UnmarshalClients 每解完一个实例就调一次
// ClientConfig.Validate，报错带着文件和行号。自己拼出一份 Config 的，可以用它先查一遍。
func (c Config) Validate() error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(c.Clients)) {
		if err := c.Clients[name].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("Clients.%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Validate 检查一个实例的配置本身说不通的地方，在建连之前就失败。
//
// 读配置文件时 xconfig.UnmarshalClients 每解完一个实例就调它，配错的值在读配置时
// 就失败、带着文件和行号，一个实例都还没连；直接调 New 的，New 也会调一次。
// 返回普通 error，由调它的那一层包一次 xerror。
func (c ClientConfig) Validate() error {
	if c.DSN == "" {
		return fmt.Errorf("DSN must not be empty")
	}
	d, ok := lookupDialect(c.Driver)
	if !ok {
		return unknownDriver(c.Driver)
	}
	if err := c.TLS.Validate(); err != nil {
		return err
	}
	if c.TLS.Enable && d.OpenTLS == nil {
		return fmt.Errorf("Driver=%q does not support the TLS block, configure TLS in its DSN instead", c.Driver)
	}
	if c.MaxOpenConns <= 0 {
		return fmt.Errorf("MaxOpenConns must be > 0, got=%d", c.MaxOpenConns)
	}
	if c.MaxIdleConns < 0 {
		return fmt.Errorf("MaxIdleConns must not be negative, got=%d", c.MaxIdleConns)
	}
	if c.CreateBatchSize < 0 {
		return fmt.Errorf("CreateBatchSize must not be negative, got=%d", c.CreateBatchSize)
	}
	// 负的时长没有一个说得通的含义，而且底下每一处都把它静默变成「不限」：
	// database/sql 把负的 MaxLifetime / MaxIdleTime 当成与 0 相同的「不按时长关连接」；
	// go-sql-driver v1.10.1 的 FormatDSN 只写 > 0 的 timeout / readTimeout / writeTimeout，
	// 负数注进去就从 DSN 里消失了，对端不回话时查询一直挂着；PG 的 connect_timeout
	// 与几个 GUC 在注入时同样被跳过。一个减号换来一个静默消失的超时，配置文件看上去
	// 毫无问题。0 各有文档写明的含义（不注入 / 不限），不在此列
	for _, f := range []struct {
		name string
		val  time.Duration
	}{
		{"DialTimeout", c.DialTimeout},
		{"MaxLifetime", c.MaxLifetime},
		{"MaxIdleTime", c.MaxIdleTime},
		{"SlowThreshold", c.SlowThreshold},
		{"MySQL.ReadTimeout", c.MySQL.ReadTimeout},
		{"MySQL.WriteTimeout", c.MySQL.WriteTimeout},
		{"Postgres.StatementTimeout", c.Postgres.StatementTimeout},
		{"Postgres.LockTimeout", c.Postgres.LockTimeout},
		{"Postgres.IdleInTxTimeout", c.Postgres.IdleInTxTimeout},
	} {
		if f.val < 0 {
			return fmt.Errorf("%s must not be negative, got=%v", f.name, f.val)
		}
	}
	return nil
}
