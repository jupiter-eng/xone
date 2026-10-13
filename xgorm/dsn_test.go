package xgorm

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// 这份测试最要紧的一条：凭证不能出现在任何错误或日志里。
// 上一版靠「把 DSN 打出去再脱敏」，那是个永远做不干净的活；
// 这一版根本不打印 DSN，所以这里要证明的是「真的没打」。

const secret = "hunter2"

// resolveDSN 按 c.Driver 查到方言再解 DSN，即 open 里那两步。
// 驱动不认识时照样报错：那是 Validate 的事，这里只是别让测试拿着零值方言往下走
func resolveDSN(c ClientConfig) (string, ConnInfo, error) {
	d, ok := lookupDialect(c.Driver)
	if !ok {
		return "", ConnInfo{}, unknownDriver(c.Driver)
	}
	return d.resolve(c)
}

func mysqlCfg(dsn string) ClientConfig {
	c := DefaultClientConfig()
	c.Driver, c.DSN = DriverMySQL, dsn
	return c
}

func pgCfg(dsn string) ClientConfig {
	c := DefaultClientConfig()
	c.Driver, c.DSN = DriverPostgres, dsn
	return c
}

func TestResolveDSN_MySQLInjectsTimeouts(t *testing.T) {
	dsn, info, err := resolveDSN(mysqlCfg("u:" + secret + "@tcp(db.example.com:3306)/app"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"timeout=500ms", "readTimeout=3s", "writeTimeout=5s"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("应注入 %s，got=%s", want, dsn)
		}
	}
	if info.Addr != "db.example.com:3306" || info.DB != "app" || info.Driver != "mysql" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
}

func TestResolveDSN_MySQLKeepsExistingTimeouts(t *testing.T) {
	// 配置里的值只是默认值，DSN 里显式写了的以 DSN 为准
	dsn, _, err := resolveDSN(mysqlCfg("u:p@tcp(h:3306)/app?timeout=9s&readTimeout=8s&writeTimeout=7s"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"timeout=9s", "readTimeout=8s", "writeTimeout=7s"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("不该覆盖 %s，got=%s", want, dsn)
		}
	}
}

func TestResolveDSN_MySQLDefaultsParseTimeToTrue(t *testing.T) {
	// 驱动默认 parseTime=false：DATETIME 扫不进 time.Time，带 CreatedAt 的模型
	// First 一次就报 unsupported Scan（实测 go-sql-driver v1.10.1、MySQL 8.0.46）。
	// DSN 里写了的，哪怕写的是 false，以 DSN 为准
	for _, c := range []struct {
		name, dsn string
		want      bool
	}{
		{"没写", "u:p@tcp(h:3306)/app", true},
		{"没写但有别的参数", "u:p@tcp(h:3306)/app?charset=utf8mb4", true},
		{"写了 true", "u:p@tcp(h:3306)/app?parseTime=true", true},
		{"写了 false", "u:p@tcp(h:3306)/app?parseTime=false", false},
		{"写了 0", "u:p@tcp(h:3306)/app?charset=utf8mb4&parseTime=0", false},
		// 密码在最后一个 / 之前，里面的 ?parseTime=false 不算数——驱动自己也这么解
		{"密码里写着 parseTime", "u:x?parseTime=false@tcp(h:3306)/app", true},
		{"密码里带 / 又写着 parseTime", "u:a/b?parseTime=false@tcp(h:3306)/app", true},
		// 库名里的 parseTime 同理：它在 ? 之前
		{"库名里写着 parseTime", "u:p@tcp(h:3306)/parseTime=false", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn, _, err := resolveDSN(mysqlCfg(c.dsn))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := mysqldriver.ParseDSN(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ParseTime != c.want {
				t.Errorf("parseTime 应是 %v，最终 DSN=%s", c.want, dsn)
			}
		})
	}
}

func TestResolveDSN_ConnInfoHasNoPassword(t *testing.T) {
	// ConnInfo 是唯一进日志的东西，它必须不含凭证
	for _, c := range []ClientConfig{
		mysqlCfg("u:" + secret + "@tcp(h:3306)/app"),
		pgCfg("postgres://u:" + secret + "@h:5432/app"),
		pgCfg("host=h port=5432 dbname=app user=u password=" + secret),
		pgCfg("host=h dbname=app password='" + secret + " with space'"),
	} {
		_, info, err := resolveDSN(c)
		if err != nil {
			t.Fatal(err)
		}
		blob := info.Driver + info.Addr + info.DB
		if strings.Contains(blob, secret) {
			t.Errorf("连接信息里出现了密码：%+v", info)
		}
		if info.DB != "app" {
			t.Errorf("库名应解出来，got=%+v（DSN=%s）", info, c.DSN)
		}
	}
}

func TestResolveDSN_ParseErrorOmitsDSN(t *testing.T) {
	// 驱动的解析错误会把 DSN 片段带在错误信息里，不能原样往上传
	_, _, err := resolveDSN(mysqlCfg("u:" + secret + "@这不是个合法的DSN"))
	if err == nil {
		t.Fatal("非法 DSN 应当报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息里出现了密码：%v", err)
	}

	_, _, err = resolveDSN(pgCfg("postgres://u:" + secret + "@h:5432/app?x=%zz"))
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息里出现了密码：%v", err)
	}
}

func TestResolveDSN_PG_URLForm(t *testing.T) {
	c := pgCfg("postgres://u:p@h:5432/app")
	c.Postgres.StatementTimeout = 2 * time.Second
	c.Postgres.LockTimeout = 1500 * time.Millisecond
	c.Postgres.IdleInTxTimeout = time.Minute

	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"connect_timeout=1", // 500ms 向上取整为 1 秒，libpq 只收整数秒
		"statement_timeout=2000",
		"lock_timeout=1500",
		"idle_in_transaction_session_timeout=60000",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("应注入 %s，got=%s", want, dsn)
		}
	}
	if info.Addr != "h:5432" || info.DB != "app" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
}

func TestResolveDSN_PG_KVForm(t *testing.T) {
	c := pgCfg("host=h port=5432 dbname=app user=u")
	c.Postgres.StatementTimeout = time.Second

	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "connect_timeout=1") || !strings.Contains(dsn, "statement_timeout=1000") {
		t.Errorf("应注入参数，got=%s", dsn)
	}
	if info.Addr != "h:5432" || info.DB != "app" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
}

func TestResolveDSN_PGKeepsExistingKeys(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@h:5432/app?connect_timeout=9",
		"host=h dbname=app connect_timeout=9",
	} {
		got, _, err := resolveDSN(pgCfg(dsn))
		if err != nil {
			t.Fatal(err)
		}
		// 看 pgx 读出来的是什么，不看串里有没有某个子串：key=value 形式里
		// 默认值照样在串里，只是垫在前面、被后写的盖掉
		cfg, err := pgconn.ParseConfig(got)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConnectTimeout != 9*time.Second {
			t.Errorf("DSN 里写了的不该被覆盖，got=%s → %v", got, cfg.ConnectTimeout)
		}
	}
}

func TestResolveDSN_PG_ParamsTakePrecedence(t *testing.T) {
	// Params 是使用者显式写的，比字段默认值更该作数
	c := pgCfg("host=h dbname=app")
	c.Postgres.StatementTimeout = time.Second
	c.Postgres.Params = map[string]string{"statement_timeout": "5000", "application_name": "svc"}

	dsn, _, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "statement_timeout=5000") || strings.Contains(dsn, "statement_timeout=1000") {
		t.Errorf("Params 应覆盖字段值，got=%s", dsn)
	}
	if !strings.Contains(dsn, "application_name=svc") {
		t.Errorf("Params 里的其它 key 也该注入，got=%s", dsn)
	}
}

func TestQuoteKV(t *testing.T) {
	// libpq 的转义规则：含空格/单引号/反斜杠要加引号
	for _, c := range []struct{ in, want string }{
		{"simple", "simple"},
		{"", "''"},
		{"with space", "'with space'"},
		{"it's", `'it\'s'`},
		{`back\slash`, `'back\\slash'`},
	} {
		if got := quoteKV(c.in); got != c.want {
			t.Errorf("quoteKV(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSecondsMillis(t *testing.T) {
	// connect_timeout 只收整数秒且最小为 1，向下取整会把 500ms 变成 0（不限制）
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, ""}, {-time.Second, ""},
		{time.Millisecond, "1"},
		{500 * time.Millisecond, "1"},
		{1500 * time.Millisecond, "2"},
		{3 * time.Second, "3"},
	} {
		if got := seconds(c.d); got != c.want {
			t.Errorf("seconds(%v)=%q want %q", c.d, got, c.want)
		}
	}
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, ""}, {time.Second, "1000"}, {1500 * time.Millisecond, "1500"},
		// statement_timeout 等的 0 同样是不限制：不满 1ms 的向上取整
		{500 * time.Microsecond, "1"}, {1500 * time.Microsecond, "2"},
	} {
		if got := millis(c.d); got != c.want {
			t.Errorf("millis(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestResolveDSN_QueryUnparsableErrorsInsteadOfDroppingParams(t *testing.T) {
	// 密码里带一个字面 % 就构成非法的百分号转义。u.Query() 会把它吞掉、
	// 只返回解得出的那部分，回写之后 DSN 里就没有密码了——
	// 服务报「认证失败」，而配置文件里密码明明写着。
	const secret = "p%ssw0rd"
	c := DefaultClientConfig()
	c.DSN = "postgres://h:5432/db?password=" + secret + "&sslmode=require"
	c.DialTimeout = time.Second

	_, _, err := resolveDSN(c)
	if err == nil {
		t.Fatal("解不出来的 query 应当报错，而不是悄悄丢掉那个参数")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息里出现了凭证：%v", err)
	}
	if !strings.Contains(err.Error(), "%25") {
		t.Errorf("该告诉使用者怎么改，got=%v", err)
	}
}

func TestResolveDSN_ValidPercentEscapesKept(t *testing.T) {
	c := DefaultClientConfig()
	c.DSN = "postgres://h:5432/db?password=p%25ssw0rd"
	c.DialTimeout = time.Second

	dsn, _, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("password"); got != "p%ssw0rd" {
		t.Errorf("密码该原样留着，got=%q", got)
	}
	if u.Query().Get("connect_timeout") == "" {
		t.Error("超时仍该注进去")
	}
}

func TestResolveDSN_PGPasswordFragmentNotTakenAsConnInfo(t *testing.T) {
	// 密码里带空格是合法的（password='a b'），按空白切的话引号里的内容
	// 会被当成独立的 key=value 读出来，然后写进建连日志。
	// 这个包一开始就决定不打印 DSN，从密码里抠出一段再打出去是同一个洞
	const secret = "SECRET_FRAGMENT"
	for _, c := range []struct{ name, dsn, addr, db string }{
		{"密码里含 host=", "host=real.db port=5432 dbname=mydb user=app password='p " + secret + " host=" + secret + "'", "real.db:5432", "mydb"},
		{"密码里含 dbname=", "user=app password='x dbname=" + secret + "' host=real.db", "real.db:5432", ""},
		{"密码里有空格", "user=app password='a b' dbname=mydb host=real.db port=5433", "real.db:5433", "mydb"},
		{"密码里有转义单引号", `user=app password='it\'s host=` + secret + `' host=real.db dbname=mydb`, "real.db:5432", "mydb"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, info, err := resolveDSN(pgCfg(c.dsn))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(info.Addr, secret) || strings.Contains(info.DB, secret) {
				t.Fatalf("密码片段进了日志字段：%+v", info)
			}
			if info.Addr != c.addr || info.DB != c.db {
				t.Errorf("got=%+v want addr=%q db=%q", info, c.addr, c.db)
			}
		})
	}

	t.Run("引号没闭合就整个放弃", func(t *testing.T) {
		_, info, err := resolveDSN(pgCfg("user=app password='unclosed host=" + secret))
		if err == nil {
			t.Fatalf("解不出来应当报错，got=%+v", info)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("错误里带出了密码：%v", err)
		}
	})
}

func TestResolveDSN_PGSpacesAroundEqualsNotOverriddenByDefaults(t *testing.T) {
	// pgx 接受 connect_timeout = 10。从前自己解 DSN 来判断「写没写过」，
	// 按空白切 token 时这一项被切成三段、一个 key 都认不出来，默认值被追加在后面
	// ——pgx 同一个 key 取最后一次，使用者显式写的 10 就这样被 1 盖掉了
	got, _, err := resolveDSN(pgCfg("host=h dbname=d connect_timeout = 10"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 10*time.Second {
		t.Errorf("pgx 读到的该是使用者写的 10s，got=%q → %v", got, cfg.ConnectTimeout)
	}
}

func TestResolveDSN_PGValueEndingInEscapedSpaceDoesNotBleed(t *testing.T) {
	// password=a\  的最后那个空格属于密码。补进来的参数要是直接接在它后面，
	// 就成了密码的一部分
	got, _, err := resolveDSN(pgCfg(`host=h password=a\ `))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != `a\ ` || cfg.ConnectTimeout != time.Second {
		t.Errorf("补进来的参数和密码串了，got=%q → password=%q connect_timeout=%v", got, cfg.Password, cfg.ConnectTimeout)
	}
}

func TestResolveDSN_PG_DSNTimezoneSkipsDefaultTimezone(t *testing.T) {
	// gorm 的 postgres 驱动不走 pgx，自己用正则取 DSN 里第一处时区。
	// 默认值垫在前面，Params 里的时区就会抢在使用者写的前面被它取走
	for _, c := range []struct{ name, dsn, want string }{
		{"DSN 里写了", "host=h dbname=app timezone=Asia/Shanghai", "Asia/Shanghai"},
		{"写法和 Params 里的不同也算", "host=h dbname=app time_zone=Asia/Shanghai", "Asia/Shanghai"},
		{"DSN 里没写", "host=h dbname=app", "UTC"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := pgCfg(c.dsn)
			cfg.Postgres.Params = map[string]string{"TimeZone": "UTC"}
			got, _, err := resolveDSN(cfg)
			if err != nil {
				t.Fatal(err)
			}
			m := gormTimeZone.FindStringSubmatch(got)
			if len(m) < 3 || m[2] != c.want {
				t.Errorf("gorm 取到的时区=%v want %s，DSN=%s", m, c.want, got)
			}
		})
	}
}

func TestResolvePostgres_UnparsableDSNKeepsPasswordOutOfError(t *testing.T) {
	// pgx 自己的错误原文里就是整串 DSN，它只遮得住 password=x、password='x'
	// 两种规整写法。下面两种实测都把密码带了出去，那是一条会进日志和告警的错误
	for _, dsn := range []string{
		"host=127.0.0.1 port=abc user=u password = hunter2 dbname=d",
		`host=127.0.0.1 port=abc user=u password='it\'s hunter2 x' dbname=d`,
	} {
		c := DefaultClientConfig()
		c.Driver, c.DSN = "postgres", dsn
		_, _, err := resolvePostgres(c)
		if err == nil {
			t.Fatalf("端口写错了应当报错：%s", dsn)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("错误里带出了密码：%v", err)
		}
	}
}

func TestResolvePostgres_UnreadableCertNamesFile(t *testing.T) {
	// 文件路径不是凭证：吞掉它的话，使用者只剩一句「DSN 解析失败」
	c := DefaultClientConfig()
	c.Driver = "postgres"
	c.DSN = "host=127.0.0.1 user=u password=hunter2 sslmode=verify-full sslrootcert=/nonexistent/ca.pem"
	_, _, err := resolvePostgres(c)
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/ca.pem") {
		t.Fatalf("应当报出读不到的那个文件，got=%v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("错误里带出了密码：%v", err)
	}
}

func TestNew_PG_PgxValidationErrorsKeepPasswordOut(t *testing.T) {
	// gorm 的 postgres 驱动建连时调的是 pgx.ParseConfig，它比 pgconn.ParseConfig
	// 多校验三项。只拿 pgconn 预检的话这三项写错会一路放行到 gorm.Open，
	// 那里报出来的错误原文就是整串 DSN——实测 password = hunter2 原样在里面
	for _, bad := range []string{
		"default_query_exec_mode=bogus",
		"statement_cache_capacity=x",
		"description_cache_capacity=x",
	} {
		t.Run(bad, func(t *testing.T) {
			_, _, err := New(context.Background(), pgCfg("host=127.0.0.1 user=u password = hunter2 dbname=d "+bad))
			if err == nil {
				t.Fatal("参数写错了应当报错")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("错误里带出了密码：%v", err)
			}
			if !errors.Is(err, errMalformedDSN) {
				t.Errorf("应当在预检时就拦下，报不带 DSN 的那个错，got=%v", err)
			}
		})
	}
}

func TestResolveDSN_PG_URLAddsParamsWithoutRewritingUserQuery(t *testing.T) {
	// gorm 的 postgres 驱动拿 gormTimeZone 读原串、不解码。
	// 整个 query 重新编码的话 Asia/Shanghai 会变成 Asia%2FShanghai，每条连接都设不上时区
	const user = "postgres://u:p@h:5432/app?TimeZone=Asia/Shanghai&sslmode=disable"
	got, _, err := resolveDSN(pgCfg(user))
	if err != nil {
		t.Fatal(err)
	}
	if want := user + "&connect_timeout=1"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}

	// Params 里的时区补进 URL 时同样不能编码 /
	c := pgCfg("postgres://u:p@h:5432/app")
	c.Postgres.Params = map[string]string{"TimeZone": "Asia/Shanghai", "application_name": "a&b c"}
	got, _, err = resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if m := gormTimeZone.FindStringSubmatch(got); len(m) < 3 || m[2] != "Asia/Shanghai" {
		t.Errorf("gorm 读到的时区=%v，DSN=%s", m, got)
	}
	pc, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if pc.RuntimeParams["TimeZone"] != "Asia/Shanghai" || pc.RuntimeParams["application_name"] != "a&b c" {
		t.Errorf("pgx 读到的参数不对：%v，DSN=%s", pc.RuntimeParams, got)
	}
}
