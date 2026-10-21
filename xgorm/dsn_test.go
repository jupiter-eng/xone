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

// The single most important thing in this file: credentials must not appear in any error or log line.
// The previous version relied on "print the DSN, then redact it", which is a job that is never finished;
// this version never prints the DSN at all, so what has to be proven here is that it really does not.

const secret = "hunter2"

// resolveDSN looks up the dialect by c.Driver and then resolves the DSN -- the two steps inside open.
// An unknown driver is still an error: that is Validate's job, this only keeps the test from carrying a zero-value dialect further.
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
			t.Errorf("%s must be injected, got=%s", want, dsn)
		}
	}
	if info.Addr != "db.example.com:3306" || info.DB != "app" || info.Driver != "mysql" {
		t.Errorf("wrong connection info, got=%+v", info)
	}
}

func TestResolveDSN_MySQLKeepsExistingTimeouts(t *testing.T) {
	// The values in the config are only defaults; what is written explicitly in the DSN wins.
	dsn, _, err := resolveDSN(mysqlCfg("u:p@tcp(h:3306)/app?timeout=9s&readTimeout=8s&writeTimeout=7s"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"timeout=9s", "readTimeout=8s", "writeTimeout=7s"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("%s must not be overridden, got=%s", want, dsn)
		}
	}
}

func TestResolveDSN_MySQLDefaultsParseTimeToTrue(t *testing.T) {
	// The driver defaults to parseTime=false: DATETIME cannot be scanned into time.Time, so a model with
	// CreatedAt fails on the first First with unsupported Scan (measured with go-sql-driver v1.10.1 and MySQL 8.0.46).
	// What the DSN says wins, even when it says false.
	for _, c := range []struct {
		name, dsn string
		want      bool
	}{
		{"not set", "u:p@tcp(h:3306)/app", true},
		{"not set but other parameters present", "u:p@tcp(h:3306)/app?charset=utf8mb4", true},
		{"set to true", "u:p@tcp(h:3306)/app?parseTime=true", true},
		{"set to false", "u:p@tcp(h:3306)/app?parseTime=false", false},
		{"set to 0", "u:p@tcp(h:3306)/app?charset=utf8mb4&parseTime=0", false},
		// The password ends before the last /, so a ?parseTime=false inside it does not count -- the driver parses it the same way.
		{"parseTime inside the password", "u:x?parseTime=false@tcp(h:3306)/app", true},
		{"password with a / and parseTime", "u:a/b?parseTime=false@tcp(h:3306)/app", true},
		// Same for a parseTime inside the database name: it sits before the ?.
		{"parseTime inside the database name", "u:p@tcp(h:3306)/parseTime=false", true},
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
				t.Errorf("parseTime must be %v, final DSN=%s", c.want, dsn)
			}
		})
	}
}

func TestResolveDSN_ConnInfoHasNoPassword(t *testing.T) {
	// ConnInfo is the only thing that reaches the log, so it must carry no credentials.
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
			t.Errorf("the password appeared in the connection info: %+v", info)
		}
		if info.DB != "app" {
			t.Errorf("the database name must be resolved, got=%+v (DSN=%s)", info, c.DSN)
		}
	}
}

func TestResolveDSN_ParseErrorOmitsDSN(t *testing.T) {
	// The driver's parse errors carry DSN fragments in the message; they must not be passed up as they are.
	_, _, err := resolveDSN(mysqlCfg("u:" + secret + "@not a valid DSN"))
	if err == nil {
		t.Fatal("an invalid DSN must be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the password appeared in the error message: %v", err)
	}

	_, _, err = resolveDSN(pgCfg("postgres://u:" + secret + "@h:5432/app?x=%zz"))
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("the password appeared in the error message: %v", err)
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
		"connect_timeout=1", // 500ms rounded up to 1 second; libpq only accepts whole seconds
		"statement_timeout=2000",
		"lock_timeout=1500",
		"idle_in_transaction_session_timeout=60000",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("%s must be injected, got=%s", want, dsn)
		}
	}
	if info.Addr != "h:5432" || info.DB != "app" {
		t.Errorf("wrong connection info, got=%+v", info)
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
		t.Errorf("the parameters must be injected, got=%s", dsn)
	}
	if info.Addr != "h:5432" || info.DB != "app" {
		t.Errorf("wrong connection info, got=%+v", info)
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
		// Check what pgx reads, not whether some substring is present: in key=value form the
		// defaults are in the string too, just placed first and overridden by what comes later.
		cfg, err := pgconn.ParseConfig(got)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConnectTimeout != 9*time.Second {
			t.Errorf("what the DSN sets must not be overridden, got=%s -> %v", got, cfg.ConnectTimeout)
		}
	}
}

func TestResolveDSN_PG_ParamsTakePrecedence(t *testing.T) {
	// Params is what the caller wrote explicitly, so it must count for more than a field default.
	c := pgCfg("host=h dbname=app")
	c.Postgres.StatementTimeout = time.Second
	c.Postgres.Params = map[string]string{"statement_timeout": "5000", "application_name": "svc"}

	dsn, _, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "statement_timeout=5000") || strings.Contains(dsn, "statement_timeout=1000") {
		t.Errorf("Params must override the field value, got=%s", dsn)
	}
	if !strings.Contains(dsn, "application_name=svc") {
		t.Errorf("the other keys in Params must be injected too, got=%s", dsn)
	}
}

func TestQuoteKV(t *testing.T) {
	// libpq's quoting rules: a value containing a space, a single quote or a backslash must be quoted.
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
	// connect_timeout only accepts whole seconds with a minimum of 1; rounding down would turn 500ms into 0 (no limit).
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
		// A 0 for statement_timeout and friends likewise means no limit: anything under 1ms rounds up.
		{500 * time.Microsecond, "1"}, {1500 * time.Microsecond, "2"},
	} {
		if got := millis(c.d); got != c.want {
			t.Errorf("millis(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestResolveDSN_QueryUnparsableErrorsInsteadOfDroppingParams(t *testing.T) {
	// A literal % in the password makes an invalid percent escape. u.Query() swallows it and returns
	// only the part it could parse, so after the value is written back the DSN has no password left --
	// the service reports "authentication failed" while the password sits right there in the config file.
	const secret = "p%ssw0rd"
	c := DefaultClientConfig()
	c.DSN = "postgres://h:5432/db?password=" + secret + "&sslmode=require"
	c.DialTimeout = time.Second

	_, _, err := resolveDSN(c)
	if err == nil {
		t.Fatal("an unparseable query must be an error, not a silently dropped parameter")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("a credential appeared in the error message: %v", err)
	}
	if !strings.Contains(err.Error(), "%25") {
		t.Errorf("the caller must be told how to fix it, got=%v", err)
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
		t.Errorf("the password must be left as is, got=%q", got)
	}
	if u.Query().Get("connect_timeout") == "" {
		t.Error("the timeout must still be injected")
	}
}

func TestResolveDSN_PGPasswordFragmentNotTakenAsConnInfo(t *testing.T) {
	// A space in the password is legal (password='a b'); splitting on whitespace would read the quoted
	// content as a key=value of its own and then write it into the connect log.
	// This package decided from the start not to print the DSN; cutting a fragment out of the password and printing that is the same hole.
	const secret = "SECRET_FRAGMENT"
	for _, c := range []struct{ name, dsn, addr, db string }{
		{"password containing host=", "host=real.db port=5432 dbname=mydb user=app password='p " + secret + " host=" + secret + "'", "real.db:5432", "mydb"},
		{"password containing dbname=", "user=app password='x dbname=" + secret + "' host=real.db", "real.db:5432", ""},
		{"password containing a space", "user=app password='a b' dbname=mydb host=real.db port=5433", "real.db:5433", "mydb"},
		{"password containing an escaped single quote", `user=app password='it\'s host=` + secret + `' host=real.db dbname=mydb`, "real.db:5432", "mydb"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, info, err := resolveDSN(pgCfg(c.dsn))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(info.Addr, secret) || strings.Contains(info.DB, secret) {
				t.Fatalf("a password fragment reached a log field: %+v", info)
			}
			if info.Addr != c.addr || info.DB != c.db {
				t.Errorf("got=%+v want addr=%q db=%q", info, c.addr, c.db)
			}
		})
	}

	t.Run("an unclosed quote gives up on the whole string", func(t *testing.T) {
		_, info, err := resolveDSN(pgCfg("user=app password='unclosed host=" + secret))
		if err == nil {
			t.Fatalf("an unparseable DSN must be an error, got=%+v", info)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error carried the password out: %v", err)
		}
	})
}

func TestResolveDSN_PGSpacesAroundEqualsNotOverriddenByDefaults(t *testing.T) {
	// pgx accepts connect_timeout = 10. The old code parsed the DSN itself to decide whether a key was set,
	// and splitting on whitespace cut this into three tokens with no recognizable key, so the default was appended
	// afterwards -- and since pgx takes the last occurrence of a key, the explicit 10 was overridden by 1.
	got, _, err := resolveDSN(pgCfg("host=h dbname=d connect_timeout = 10"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 10*time.Second {
		t.Errorf("pgx must read the 10s the caller wrote, got=%q -> %v", got, cfg.ConnectTimeout)
	}
}

func TestResolveDSN_PGValueEndingInEscapedSpaceDoesNotBleed(t *testing.T) {
	// The trailing space in password=a\  belongs to the password. An appended parameter placed right after it
	// would become part of that password.
	got, _, err := resolveDSN(pgCfg(`host=h password=a\ `))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != `a\ ` || cfg.ConnectTimeout != time.Second {
		t.Errorf("the appended parameter ran into the password, got=%q -> password=%q connect_timeout=%v", got, cfg.Password, cfg.ConnectTimeout)
	}
}

func TestResolveDSN_PG_DSNTimezoneSkipsDefaultTimezone(t *testing.T) {
	// gorm's postgres driver does not go through pgx; it picks the first time zone in the DSN with a regexp.
	// A default placed first would be taken ahead of the one the caller wrote in Params.
	for _, c := range []struct{ name, dsn, want string }{
		{"set in the DSN", "host=h dbname=app timezone=Asia/Shanghai", "Asia/Shanghai"},
		{"a different spelling than the one in Params still counts", "host=h dbname=app time_zone=Asia/Shanghai", "Asia/Shanghai"},
		{"not set in the DSN", "host=h dbname=app", "UTC"},
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
				t.Errorf("the time zone gorm read=%v want %s, DSN=%s", m, c.want, got)
			}
		})
	}
}

func TestResolvePostgres_UnparsableDSNKeepsPasswordOutOfError(t *testing.T) {
	// pgx's own error text is the whole DSN; it only masks the two tidy spellings password=x and
	// password='x'. Both cases below were measured to carry the password out, in an error that reaches logs and alerts.
	for _, dsn := range []string{
		"host=127.0.0.1 port=abc user=u password = hunter2 dbname=d",
		`host=127.0.0.1 port=abc user=u password='it\'s hunter2 x' dbname=d`,
	} {
		c := DefaultClientConfig()
		c.Driver, c.DSN = "postgres", dsn
		_, _, err := resolvePostgres(c)
		if err == nil {
			t.Fatalf("a malformed port must be rejected: %s", dsn)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error carried the password out: %v", err)
		}
	}
}

func TestResolvePostgres_UnreadableCertNamesFile(t *testing.T) {
	// A file path is not a credential: swallowing it would leave the caller with nothing but "DSN parse failed".
	c := DefaultClientConfig()
	c.Driver = "postgres"
	c.DSN = "host=127.0.0.1 user=u password=hunter2 sslmode=verify-full sslrootcert=/nonexistent/ca.pem"
	_, _, err := resolvePostgres(c)
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/ca.pem") {
		t.Fatalf("the unreadable file must be named, got=%v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error carried the password out: %v", err)
	}
}

func TestNew_PG_PgxValidationErrorsKeepPasswordOut(t *testing.T) {
	// When connecting, gorm's postgres driver calls pgx.ParseConfig, which validates three more items than
	// pgconn.ParseConfig. Pre-checking with pgconn alone would let those three through to gorm.Open,
	// whose error text is the whole DSN -- measured, password = hunter2 appears in it verbatim.
	for _, bad := range []string{
		"default_query_exec_mode=bogus",
		"statement_cache_capacity=x",
		"description_cache_capacity=x",
	} {
		t.Run(bad, func(t *testing.T) {
			_, _, err := New(context.Background(), pgCfg("host=127.0.0.1 user=u password = hunter2 dbname=d "+bad))
			if err == nil {
				t.Fatal("a malformed parameter must be rejected")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error carried the password out: %v", err)
			}
			if !errors.Is(err, errMalformedDSN) {
				t.Errorf("it must be caught in the pre-check and reported with the error that omits the DSN, got=%v", err)
			}
		})
	}
}

func TestResolveDSN_PG_URLAddsParamsWithoutRewritingUserQuery(t *testing.T) {
	// gorm's postgres driver reads the raw string for gormTimeZone without decoding it.
	// Re-encoding the whole query would turn Asia/Shanghai into Asia%2FShanghai and no connection would get the time zone set.
	const user = "postgres://u:p@h:5432/app?TimeZone=Asia/Shanghai&sslmode=disable"
	got, _, err := resolveDSN(pgCfg(user))
	if err != nil {
		t.Fatal(err)
	}
	if want := user + "&connect_timeout=1"; got != want {
		t.Errorf("got=%s\nwant=%s", got, want)
	}

	// A time zone from Params must likewise not have its / encoded when it is appended to a URL.
	c := pgCfg("postgres://u:p@h:5432/app")
	c.Postgres.Params = map[string]string{"TimeZone": "Asia/Shanghai", "application_name": "a&b c"}
	got, _, err = resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if m := gormTimeZone.FindStringSubmatch(got); len(m) < 3 || m[2] != "Asia/Shanghai" {
		t.Errorf("the time zone gorm read=%v, DSN=%s", m, got)
	}
	pc, err := pgconn.ParseConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if pc.RuntimeParams["TimeZone"] != "Asia/Shanghai" || pc.RuntimeParams["application_name"] != "a&b c" {
		t.Errorf("pgx read the wrong parameters: %v, DSN=%s", pc.RuntimeParams, got)
	}
}
