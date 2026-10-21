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

// withDialect registers a dialect for the duration of the test and removes it afterwards.
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
		t.Errorf("mysql and postgres must be built in, got=%v", got)
	}
	if !slices.IsSorted(got) {
		t.Errorf("must come back in alphabetical order, otherwise the error message differs every time, got=%v", got)
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
		t.Fatalf("a registered driver must pass validation: %v", err)
	}

	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "demo://h:1/d?tuned=1" {
		t.Errorf("the dialect's own Resolve must be used, got=%q", dsn)
	}
	if info.Driver != "demo" {
		t.Errorf("connection info must come from the dialect, got=%+v", info)
	}
}

func TestRegisterDialect_DSNUsedAsIsWithoutResolve(t *testing.T) {
	// For a driver someone just wants to get running, writing the timeout into the DSN works just as well.
	withDialect(t, Dialect{Name: "bare", Open: func(string) gorm.Dialector { return nil }})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "bare", "bare://h:1/d"
	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != c.DSN {
		t.Errorf("must be used as is, got=%q", dsn)
	}
	if info.Driver != "bare" || info.Addr != "" {
		t.Errorf("when it cannot be parsed, only the driver name is recorded, got=%+v", info)
	}
}

func TestRegisterDialect_PanicsOnDuplicateName(t *testing.T) {
	// When two Dialects claim the same name, either choice may make the service connect to a place
	// it believes it is not connecting to. That is an import-time problem; it must not survive into runtime.
	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate name must panic")
		}
	}()
	RegisterDialect(Dialect{Name: DriverMySQL, Open: func(string) gorm.Dialector { return nil }})
}

func TestRegisterDialect_PanicsOnMissingField(t *testing.T) {
	for _, c := range []struct {
		name string
		d    Dialect
	}{
		{"no name", Dialect{Open: func(string) gorm.Dialector { return nil }}},
		{"no Open", Dialect{Name: "noopen"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("must panic")
				}
			}()
			RegisterDialect(c.d)
		})
	}
}

func TestValidate_UnknownDriverListsRegistered(t *testing.T) {
	// Saying only "unknown" helps little: a typo in the name and a forgotten import of the matching
	// module are two different problems. Listing what is actually registered tells them apart at a glance.
	c := DefaultClientConfig()
	c.Driver, c.DSN = "clickhouse", "clickhouse://h:9000/db"
	err := c.Validate()
	if err == nil {
		t.Fatal("an unregistered driver must be rejected")
	}
	for _, want := range []string{"clickhouse", "mysql", "postgres", "import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must mention %q, got=%v", want, err)
		}
	}
}

// stubDialector is a Dialector that only fails in Initialize,
// used to confirm that New really uses the registered Open.
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
	// Regression case. New once still held an old function with mysql / postgres hard-coded,
	// so the registry looked right (Drivers() listed it, validation passed) while the connection
	// quietly went to postgres -- a place the caller believed it was not connecting to.
	sentinel := errors.New("stub dialector was used")
	withDialect(t, Dialect{
		Name: "stub",
		Open: func(string) gorm.Dialector { return stubDialector{err: sentinel} },
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "stub", "stub://h:1/d"
	_, _, err := New(context.Background(), c)
	if !errors.Is(err, sentinel) {
		t.Fatalf("New must use the registered Open, got=%v", err)
	}
}

func TestNew_ReadyRunsInProbeAndRetries(t *testing.T) {
	// Some drivers query the version in Initialize with context.Background(): the shutdown signal cannot
	// reach it and a failure never gets retried. Ready is where that work moves to, so it must really be
	// called, be retried together with Ping on failure, and be reported as connect.
	var calls int
	boom := errors.New("version query failed")
	withDialect(t, Dialect{
		Name: "readyprobe",
		Open: func(string) gorm.Dialector { return okDialector{} },
		Ready: func(ctx context.Context, db *gorm.DB) error {
			calls++
			if ctx == nil || db == nil {
				t.Error("Ready must receive the ctx and this instance")
			}
			return boom
		},
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "readyprobe", "readyprobe://h/d"
	_, _, err := New(context.Background(), c)
	if !errors.Is(err, boom) {
		t.Fatalf("the Ready error must be propagated, got=%v", err)
	}
	assertOneFrame(t, err, "xgorm", "connect")
	if calls != pingAttempts {
		t.Errorf("a Ready failure must be retried %d times, got %d", pingAttempts, calls)
	}
}

func TestNew_NoRetryOnAuthFailureAndSaysSo(t *testing.T) {
	// A wrong password stays wrong on retry: two more attempts only add two backoff rounds before it is reported.
	// Reporting it as cannot reach would send whoever investigates to the network first.
	// The error shape follows pgx: *pgconn.PgError wrapped in the connection error (measured: PG 16 reports 28P01 for a wrong password).
	for _, c := range []struct {
		name     string
		code     string
		attempts int
		want     string
	}{
		{"wrong password", "28P01", 1, "authentication to "},
		{"authentication method not allowed", "28000", 1, "authentication to "},
		{"missing database is not an auth failure", "3D000", pingAttempts, "cannot reach "},
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
				t.Fatalf("the server's original error must be propagated, got=%v", err)
			}
			assertOneFrame(t, err, "xgorm", "connect")
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error must say %q, got=%v", c.want, err)
			}
			if calls != c.attempts {
				t.Errorf("expected %d attempts, got %d", c.attempts, calls)
			}
		})
	}
}

func TestNew_MySQLNoRetryOnAuthFailureAndSaysSo(t *testing.T) {
	// The error shape follows go-sql-driver: *mysql.MySQLError returned as is. Measured on MySQL 8.0.46:
	// a wrong password or a missing user is 1045; no permission on the database (including an account without global
	// privileges connecting to a database that does not exist) is 1044. 1049 (database missing, visible only to an account with global privileges) is not an auth failure.
	//
	// Both paths must be recognized: the built-in MySQL dialect turns off the version query in Initialize, so the auth failure
	// surfaces in the probe; a registered dialect that connects in Initialize surfaces it in gorm.Open instead.
	for _, c := range []struct {
		name     string
		number   uint16
		attempts int    // how many attempts on the ping path
		want     string // what the error says on the gorm.Open path
	}{
		{"wrong password", 1045, 1, "authentication to "},
		{"no permission on the database", 1044, 1, "authentication to "},
		{"missing database is not an auth failure", 1049, pingAttempts, "open "},
	} {
		rejected := &mysqldriver.MySQLError{Number: c.number, Message: "Access denied"}

		t.Run(c.name+"_in_gorm.Open", func(t *testing.T) {
			withDialect(t, Dialect{Name: "myauthopen", Open: func(string) gorm.Dialector { return stubDialector{err: rejected} }, AuthFailed: mysqlAuthFailed})
			cfg := DefaultClientConfig()
			cfg.Driver, cfg.DSN = "myauthopen", "myauthopen://h/d"
			_, _, err := New(context.Background(), cfg)
			if !errors.Is(err, rejected) {
				t.Fatalf("the server's original error must be propagated, got=%v", err)
			}
			assertOneFrame(t, err, "xgorm", "connect")
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error must say %q, got=%v", c.want, err)
			}
		})

		t.Run(c.name+"_in_the_probe", func(t *testing.T) {
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
				t.Fatalf("the server's original error must be propagated, got=%v", err)
			}
			if calls != c.attempts {
				t.Errorf("expected %d attempts, got %d", c.attempts, calls)
			}
		})
	}
}

// pgDialect / mysqlDialect expose the two built-in dialects for use in tests.
func pgDialect() Dialect {
	d, _ := lookupDialect(DriverPostgres)
	return d
}

func mysqlDialect() Dialect {
	d, _ := lookupDialect(DriverMySQL)
	return d
}

func TestDialect_BuiltInsRecognizeAuthFailureAndErrorCode(t *testing.T) {
	// Measured: PG 16 reports 28P01 for a wrong password, MySQL 8.0.46 reports 1045 for a wrong password
	// and 1044 for no permission on the database. A missing database (3D000 / 1049) is not an auth failure.
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
		{"PG wrong password", pgDialect(), pgErr("28P01"), true, "28P01"},
		{"PG auth method not allowed", pgDialect(), pgErr("28000"), true, "28000"},
		{"PG missing database", pgDialect(), pgErr("3D000"), false, "3D000"},
		{"MySQL wrong password", mysqlDialect(), myErr(1045), true, "1045"},
		{"MySQL no permission on the database", mysqlDialect(), myErr(1044), true, "1044"},
		{"MySQL missing database", mysqlDialect(), myErr(1049), false, "1049"},
		{"PG dialect does not claim a MySQL error", pgDialect(), myErr(1045), false, ""},
		{"not a server error", mysqlDialect(), errors.New("i/o timeout"), false, ""},
	} {
		if got := c.d.authFailed(c.err); got != c.auth {
			t.Errorf("%s: authFailed=%v, want %v", c.name, got, c.auth)
		}
		if got := c.d.errorCode(c.err); got != c.code {
			t.Errorf("%s: errorCode=%q, want %q", c.name, got, c.code)
		}
	}
}
