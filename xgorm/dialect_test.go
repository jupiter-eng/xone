package xgorm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// withDialect 临时注册一个方言，测试结束后摘掉
func withDialect(t *testing.T, d Dialect) {
	t.Helper()
	RegisterDialect(d)
	t.Cleanup(func() {
		dialectMu.Lock()
		delete(dialects, d.Name)
		dialectMu.Unlock()
	})
}

func TestDrivers_TwoBuiltIn(t *testing.T) {
	got := Drivers()
	if !slices.Contains(got, DriverMySQL) || !slices.Contains(got, DriverPostgres) {
		t.Errorf("mysql 和 postgres 应当内置，got=%v", got)
	}
	if !slices.IsSorted(got) {
		t.Errorf("应按字母序返回，否则错误信息每次都不一样，got=%v", got)
	}
}

func TestRegisterDialect_DriverUsableAfterRegister(t *testing.T) {
	withDialect(t, Dialect{
		Name: "demo",
		Open: func(string) gorm.Dialector { return nil },
		Resolve: func(c ClientConfig) (string, ConnInfo, error) {
			return c.DSN + "?tuned=1", ConnInfo{Driver: "demo", Addr: "h:1", DB: "d"}, nil
		},
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "demo", "demo://h:1/d"
	if err := c.Validate(); err != nil {
		t.Fatalf("注册过的驱动应当通过校验：%v", err)
	}

	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "demo://h:1/d?tuned=1" {
		t.Errorf("该走方言自己的 Resolve，got=%q", dsn)
	}
	if info.Driver != "demo" {
		t.Errorf("连接信息该来自方言，got=%+v", info)
	}
}

func TestRegisterDialect_DSNUsedAsIsWithoutResolve(t *testing.T) {
	// 对一个只想先跑起来的驱动，超时写进 DSN 里一样有效
	withDialect(t, Dialect{Name: "bare", Open: func(string) gorm.Dialector { return nil }})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "bare", "bare://h:1/d"
	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != c.DSN {
		t.Errorf("该原样用，got=%q", dsn)
	}
	if info.Driver != "bare" || info.Addr != "" {
		t.Errorf("解不出来就只写驱动名，got=%+v", info)
	}
}

func TestRegisterDialect_PanicsOnDuplicateName(t *testing.T) {
	// 两个 Dialect 抢同一个名字时，选哪个都可能让服务连到一个
	// 它以为自己没在连的地方。这是 import 期的问题，不该留到运行时
	defer func() {
		if recover() == nil {
			t.Error("重名注册应当 panic")
		}
	}()
	RegisterDialect(Dialect{Name: DriverMySQL, Open: func(string) gorm.Dialector { return nil }})
}

func TestRegisterDialect_PanicsOnMissingField(t *testing.T) {
	for _, c := range []struct {
		name string
		d    Dialect
	}{
		{"没名字", Dialect{Open: func(string) gorm.Dialector { return nil }}},
		{"没 Open", Dialect{Name: "noopen"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("应当 panic")
				}
			}()
			RegisterDialect(c.d)
		})
	}
}

func TestValidate_UnknownDriverListsRegistered(t *testing.T) {
	// 只说「不认识」帮助有限：名字拼错和忘了 import 对应的 module
	// 是两个不同的问题，把实际注册了哪些列出来，两者一眼可分
	c := DefaultClientConfig()
	c.Driver, c.DSN = "clickhouse", "clickhouse://h:9000/db"
	err := c.Validate()
	if err == nil {
		t.Fatal("没注册的驱动应当报错")
	}
	for _, want := range []string{"clickhouse", "mysql", "postgres", "import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里该出现 %q，got=%v", want, err)
		}
	}
}

// stubDialector 一个只会在 Initialize 里失败的 Dialector，
// 用来确认 New 真的用了注册进来的那个 Open
type stubDialector struct{ err error }

func (d stubDialector) Name() string                    { return "stub" }
func (d stubDialector) Initialize(*gorm.DB) error       { return d.err }
func (d stubDialector) Migrator(*gorm.DB) gorm.Migrator { return nil }
func (d stubDialector) DataTypeOf(*schema.Field) string { return "" }
func (d stubDialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{}
}
func (d stubDialector) BindVarTo(clause.Writer, *gorm.Statement, any) {}
func (d stubDialector) QuoteTo(clause.Writer, string)                 {}
func (d stubDialector) Explain(sql string, _ ...any) string           { return sql }

func TestNew_UsesRegisteredOpen(t *testing.T) {
	// 回归用例。曾经 New 里还留着一个写死 mysql / postgres 的旧函数，
	// 于是注册表看着是对的（Drivers() 里有它、校验也过了），
	// 建连却悄悄走了 postgres —— 连到了一个使用者以为自己没在连的地方
	sentinel := errors.New("stub dialector was used")
	withDialect(t, Dialect{
		Name: "stub",
		Open: func(string) gorm.Dialector { return stubDialector{err: sentinel} },
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "stub", "stub://h:1/d"
	_, _, err := New(context.Background(), c)
	if !errors.Is(err, sentinel) {
		t.Fatalf("New 该用注册进来的 Open，got=%v", err)
	}
}

func TestNew_ReadyRunsInProbeAndRetries(t *testing.T) {
	// 有的驱动在 Initialize 里用 context.Background() 查版本：退出信号管不到，
	// 失败了也轮不到重试。Ready 是给它们挪过来的地方，所以必须真的被调到、
	// 失败时跟 Ping 一起重试，并且报成 connect
	var calls int
	boom := errors.New("version query failed")
	withDialect(t, Dialect{
		Name: "readyprobe",
		Open: func(string) gorm.Dialector { return okDialector{} },
		Ready: func(ctx context.Context, db *gorm.DB) error {
			calls++
			if ctx == nil || db == nil {
				t.Error("Ready 要拿到 ctx 和这个实例")
			}
			return boom
		},
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "readyprobe", "readyprobe://h/d"
	_, _, err := New(context.Background(), c)
	if !errors.Is(err, boom) {
		t.Fatalf("Ready 的错误要传出来，got=%v", err)
	}
	assertOneFrame(t, err, "xgorm", "connect")
	if calls != pingAttempts {
		t.Errorf("Ready 失败要跟着重试 %d 次，实际 %d 次", pingAttempts, calls)
	}
}

func TestNew_NoRetryOnAuthFailureAndSaysSo(t *testing.T) {
	// 密码错了重试也是错：再试两次只是多等两轮退避才报出来。
	// 报成 cannot reach 的话，排查的人会先去查网络。
	// 错误的形状照 pgx 的来：*pgconn.PgError 包在建连错误里（实测 PG 16 密码错是 28P01）
	for _, c := range []struct {
		name     string
		code     string
		attempts int
		want     string
	}{
		{"密码错误", "28P01", 1, "authentication to "},
		{"认证方式不允许", "28000", 1, "authentication to "},
		{"库不存在不算认证失败", "3D000", pingAttempts, "cannot reach "},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			rejected := fmt.Errorf("failed to connect to `user=u database=d`: %w",
				&pgconn.PgError{Severity: "FATAL", Code: c.code, Message: "rejected by server"})
			withDialect(t, Dialect{
				Name:       "authprobe",
				Open:       func(string) gorm.Dialector { return okDialector{} },
				AuthFailed: postgresAuthFailed,
				Ready: func(context.Context, *gorm.DB) error {
					calls++
					return rejected
				},
			})

			cfg := DefaultClientConfig()
			cfg.Driver, cfg.DSN = "authprobe", "authprobe://h/d"
			_, _, err := New(context.Background(), cfg)
			if !errors.Is(err, rejected) {
				t.Fatalf("服务端的原始错误要传出来，got=%v", err)
			}
			assertOneFrame(t, err, "xgorm", "connect")
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误应说 %q，got=%v", c.want, err)
			}
			if calls != c.attempts {
				t.Errorf("应试 %d 次，实际 %d 次", c.attempts, calls)
			}
		})
	}
}

func TestNew_MySQLNoRetryOnAuthFailureAndSaysSo(t *testing.T) {
	// 错误的形状照 go-sql-driver 的来：*mysql.MySQLError 原样返回。实测 MySQL 8.0.46：
	// 密码错、用户不存在是 1045；没有这个库的权限（包括没有全局权限的账号连一个不存在的库）是 1044。
	// 1049（库不存在，有全局权限的账号才看得到）不算认证失败。
	//
	// 两条路都要认得出来：内置的 MySQL 方言关掉了 Initialize 里的查版本，认证失败出在探测里；
	// 注册进来的方言要是在 Initialize 里建连，就出在 gorm.Open 里
	for _, c := range []struct {
		name     string
		number   uint16
		attempts int    // 走 ping 那条路时试几次
		want     string // 走 gorm.Open 那条路时错误里说什么
	}{
		{"密码错误", 1045, 1, "authentication to "},
		{"没有库的权限", 1044, 1, "authentication to "},
		{"库不存在不算认证失败", 1049, pingAttempts, "open "},
	} {
		rejected := &mysqldriver.MySQLError{Number: c.number, Message: "Access denied"}

		t.Run(c.name+"_在gorm.Open里", func(t *testing.T) {
			withDialect(t, Dialect{Name: "myauthopen", Open: func(string) gorm.Dialector { return stubDialector{err: rejected} }, AuthFailed: mysqlAuthFailed})
			cfg := DefaultClientConfig()
			cfg.Driver, cfg.DSN = "myauthopen", "myauthopen://h/d"
			_, _, err := New(context.Background(), cfg)
			if !errors.Is(err, rejected) {
				t.Fatalf("服务端的原始错误要传出来，got=%v", err)
			}
			assertOneFrame(t, err, "xgorm", "connect")
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误应说 %q，got=%v", c.want, err)
			}
		})

		t.Run(c.name+"_在探测里", func(t *testing.T) {
			var calls int
			withDialect(t, Dialect{
				Name:       "myauthping",
				Open:       func(string) gorm.Dialector { return okDialector{} },
				AuthFailed: mysqlAuthFailed,
				Ready: func(context.Context, *gorm.DB) error {
					calls++
					return fmt.Errorf("select version: %w", rejected)
				},
			})
			cfg := DefaultClientConfig()
			cfg.Driver, cfg.DSN = "myauthping", "myauthping://h/d"
			_, _, err := New(context.Background(), cfg)
			if !errors.Is(err, rejected) {
				t.Fatalf("服务端的原始错误要传出来，got=%v", err)
			}
			if calls != c.attempts {
				t.Errorf("应试 %d 次，实际 %d 次", c.attempts, calls)
			}
		})
	}
}

// pgDialect / mysqlDialect 内置的两个方言，测试里要单独拿出来用
func pgDialect() Dialect {
	d, _ := lookupDialect(DriverPostgres)
	return d
}

func mysqlDialect() Dialect {
	d, _ := lookupDialect(DriverMySQL)
	return d
}

func TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode(t *testing.T) {
	// 实测 PG 16 密码错是 28P01、MySQL 8.0.46 密码错是 1045、没有库权限是 1044。
	// 库不存在（3D000 / 1049）不算认证失败
	pgErr := func(code string) error {
		return fmt.Errorf("failed to connect: %w", &pgconn.PgError{Code: code})
	}
	myErr := func(n uint16) error { return &mysqldriver.MySQLError{Number: n} }
	for _, c := range []struct {
		name string
		d    Dialect
		err  error
		auth bool
		code string
	}{
		{"PG 密码错", pgDialect(), pgErr("28P01"), true, "28P01"},
		{"PG 认证方式不允许", pgDialect(), pgErr("28000"), true, "28000"},
		{"PG 库不存在", pgDialect(), pgErr("3D000"), false, "3D000"},
		{"MySQL 密码错", mysqlDialect(), myErr(1045), true, "1045"},
		{"MySQL 没有库权限", mysqlDialect(), myErr(1044), true, "1044"},
		{"MySQL 库不存在", mysqlDialect(), myErr(1049), false, "1049"},
		{"PG 方言不认 MySQL 的错", pgDialect(), myErr(1045), false, ""},
		{"不是服务端的错", mysqlDialect(), errors.New("i/o timeout"), false, ""},
	} {
		if got := c.d.authFailed(c.err); got != c.auth {
			t.Errorf("%s：authFailed=%v，want %v", c.name, got, c.auth)
		}
		if got := c.d.errorCode(c.err); got != c.code {
			t.Errorf("%s：errorCode=%q，want %q", c.name, got, c.code)
		}
	}
}
