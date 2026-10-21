package clickhouse

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"gorm.io/driver/clickhouse"
	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

func cfg(dsn string, dial time.Duration) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.DialTimeout = Driver, dsn, dial
	return c
}

func TestRegister_ImportRegisters(t *testing.T) {
	// The caller writes one blank import line and must not have to call any function.
	if !slices.Contains(xgorm.Drivers(), Driver) {
		t.Fatalf("clickhouse must be in the registered list after a blank import, got=%v", xgorm.Drivers())
	}
}

// The database name may be written in the path or as ?database=: the driver accepts both and the one in the query wins
// (clickhouse-go v2.48.0 clickhouse_options.go fromDSN). The db in logs and Spans must be the one actually connected to.
func TestResolve_DBIsTheOneDriverUses(t *testing.T) {
	for dsn, want := range map[string]string{
		"clickhouse://u:p@h:9000/analytics":                 "analytics",
		"clickhouse://u:p@h:9000?database=events":           "events",
		"clickhouse://u:p@h:9000/analytics?database=events": "events",
		"clickhouse://u:p@h:9000/":                          "",
	} {
		_, info, err := resolve(cfg(dsn, 0))
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if info.DB != want {
			t.Errorf("%s: DB want %q, got %q", dsn, want, info.DB)
		}
	}
}

func TestResolve_InjectsDialTimeout(t *testing.T) {
	dsn, info, err := resolve(cfg("clickhouse://u:p@h:9000/analytics", 300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, dsn)
	if q.Get(dialTimeoutKey) != "300ms" {
		t.Errorf("the dial timeout must be injected into the DSN, got=%q", q.Get(dialTimeoutKey))
	}
	if info.Driver != "clickhouse" || info.Addr != "h:9000" || info.DB != "analytics" {
		t.Errorf("wrong connection info, got=%+v", info)
	}
}

func TestResolve_DSNValuesNotOverridden(t *testing.T) {
	// The values in the config are only defaults; whatever the caller wrote explicitly in the DSN always wins.
	dsn, info, err := resolve(cfg("clickhouse://h:9000/db?dial_timeout=5s", time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustQuery(t, dsn).Get(dialTimeoutKey); got != "5s" {
		t.Errorf("what is already in the DSN must not be overridden, got=%q", got)
	}
	// The connect-verification budget is computed from the timeout in info, so a longer value in the DSN must produce the longer budget.
	if info.ProbeTimeout != 10*time.Second {
		t.Errorf("the probe budget must follow the 5s written in the DSN (dial + a round trip of the same order = 10s), got=%v", info.ProbeTimeout)
	}
}

func TestDialect_RecognizesAuthFailure(t *testing.T) {
	// There is no ClickHouse to connect to; the error shape is taken from the driver source: on the native protocol the server
	// replies with an Exception packet and the driver returns *clickhouse.Exception as is; over HTTP it is a *clickhouse.HTTPError
	// wrapping a *clickhouse.Exception (v2.48.0 conn_http_errors.go). The error codes come from ch-go's typed constants.
	wrap := func(code int32) error {
		return fmt.Errorf("dial: %w", &chgo.Exception{Code: code, Name: "DB::Exception", Message: "rejected"})
	}
	for _, c := range []struct {
		name string
		err  error
		auth bool
		code string
	}{
		{"AUTHENTICATION_FAILED", wrap(516), true, "516"},
		{"UNKNOWN_USER", wrap(192), true, "192"},
		{"WRONG_PASSWORD", wrap(193), true, "193"},
		{"REQUIRED_PASSWORD", wrap(194), true, "194"},
		{"UNKNOWN_DATABASE does not count", wrap(81), false, "81"},
		{"HTTP protocol", fmt.Errorf("sendQuery: %w", &chgo.HTTPError{StatusCode: 403, Err: &chgo.Exception{Code: 516}}), true, "516"},
		{"HTTP without a parseable Exception is not recognized", &chgo.HTTPError{StatusCode: 502, Err: errors.New("bad gateway")}, false, ""},
	} {
		if got := dialect.AuthFailed(c.err); got != c.auth {
			t.Errorf("%s: AuthFailed=%v, want %v", c.name, got, c.auth)
		}
		if got := dialect.ErrorCode(c.err); got != c.code {
			t.Errorf("%s: ErrorCode=%q, want %q", c.name, got, c.code)
		}
	}
}

// authProbeSeq numbers the dialects registered by TestNew_NoRetryOnAuthFailure.
var authProbeSeq atomic.Int32

func TestNew_NoRetryOnAuthFailure(t *testing.T) {
	// Aimed at the registered dialect: without AuthFailed, a wrong password would run through all three rounds and be reported as unreachable.
	rejected := &chgo.Exception{Code: 516, Message: "default: Authentication failed"}
	conn := &rejectConnector{err: rejected}
	d := dialect // everything except Open is the registered one
	// Dialect registration is global and cannot be undone (the registry is not reachable from outside the package): a fresh name per run is what makes -count=N work.
	d.Name = xgorm.Driver(fmt.Sprintf("clickhouse-authprobe-%d", authProbeSeq.Add(1)))
	d.Open = func(string) gorm.Dialector {
		return clickhouse.New(clickhouse.Config{Conn: sql.OpenDB(conn), SkipInitializeWithVersion: true})
	}
	xgorm.RegisterDialect(d)

	c := cfg("clickhouse://h:9000/db", 50*time.Millisecond)
	c.Driver = d.Name
	_, _, err := xgorm.New(context.Background(), c)
	if !errors.Is(err, rejected) || !strings.Contains(err.Error(), "authentication to h:9000 failed") {
		t.Errorf("an auth failure must be reported as an auth failure, got=%v", err)
	}
	if n := conn.calls.Load(); n != 1 {
		t.Errorf("an auth failure must not be retried, tried %d times", n)
	}
}

// rejectConnector is rejected by the server on every connection, in the shape of the Exception packet received during a native-protocol handshake.
type rejectConnector struct {
	err   error
	calls atomic.Int32
}

func (c *rejectConnector) Connect(context.Context) (driver.Conn, error) {
	c.calls.Add(1)
	return nil, c.err
}
func (c *rejectConnector) Driver() driver.Driver { return nil }

func TestResolve_UserQueryKeptVerbatim(t *testing.T) {
	// Injecting dial_timeout only appends a segment and does not touch a single byte of the caller's query:
	// parsing and re-running q.Encode() would reorder the parameters by key and turn commas into %2C and slashes into %2F.
	cases := []struct{ dsn, want string }{
		{
			"clickhouse://u:p@h:9000/db?secure=false&compress=lz4&x_tag=a,b/c",
			"clickhouse://u:p@h:9000/db?secure=false&compress=lz4&x_tag=a,b/c&dial_timeout=300ms",
		},
		{"clickhouse://u:p@h:9000/db", "clickhouse://u:p@h:9000/db?dial_timeout=300ms"},
		{"clickhouse://u:p@h:9000/db?", "clickhouse://u:p@h:9000/db?dial_timeout=300ms"},
	}
	for _, tc := range cases {
		dsn, _, err := resolve(cfg(tc.dsn, 300*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if dsn != tc.want {
			t.Errorf("the caller's query must be left as is\n got=%s\nwant=%s", dsn, tc.want)
		}
		// The driver still parses it and reads the injected timeout.
		opts, err := chgo.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if opts.DialTimeout != 300*time.Millisecond {
			t.Errorf("the driver must read the injected dial_timeout, got=%v", opts.DialTimeout)
		}
	}
}

func TestResolve_ZeroTimeoutNotInjected(t *testing.T) {
	dsn, _, err := resolve(cfg("clickhouse://h:9000/db", 0))
	if err != nil {
		t.Fatal(err)
	}
	if mustQuery(t, dsn).Has(dialTimeoutKey) {
		t.Errorf("no timeout configured means none may be invented, got=%s", dsn)
	}
}

func TestResolve_ZeroTimeoutProbesWithDriverDefault(t *testing.T) {
	// Neither the config nor the DSN sets dial_timeout: ParseDSN reads 0 and the driver fills in 30s only when connecting
	// (v2.48.0 clickhouse_options.go setDefaults). A budget computed from 0 would leave xgorm with its 1s fallback,
	// and connect verification would time out a slow handshake the driver was still willing to wait for.
	_, info, err := resolve(cfg("clickhouse://h:9000/db", 0))
	if err != nil {
		t.Fatal(err)
	}
	if info.ProbeTimeout != 60*time.Second {
		t.Errorf("the probe budget must follow the 30s the driver fills in (dial + a round trip of the same order = 60s), got=%v", info.ProbeTimeout)
	}
}

func TestResolve_RejectsNonURLDSNWithoutEcho(t *testing.T) {
	// The driver does not accept a bare host:port (measured: ParseDSN("10.255.255.1:9000") returns an error),
	// so passing it through would put its parse error, together with the whole DSN including the plaintext password, into the log.
	// A misspelled scheme is accepted by the driver, but it would bypass the timeout injection here.
	const secret = "hunter2"
	for _, raw := range []string{
		"10.255.255.1:9000",
		"u:" + secret + "@h:9000/db",
		" clickhouse://u:" + secret + "@h:9000/db", // one leading space
		"clickhous://u:" + secret + "@h:9000/db",   // misspelled scheme
	} {
		_, _, err := resolve(cfg(raw, time.Second))
		if err == nil {
			t.Errorf("%q must be rejected", raw)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a credential appeared in the error message: %v", err)
		}
	}
}

func TestResolve_RejectsDriverUnparsableDSNWithoutEcho(t *testing.T) {
	// Leaving it to be reported at connect time would put the driver's raw error straight into the log --
	// when http_proxy fails to parse it echoes the whole proxy address, which may itself contain credentials.
	const secret = "hunter2"
	for _, raw := range []string{
		"https://u:" + secret + "@h:8443/db",                       // https without secure=true, rejected by the driver
		"clickhouse://u:" + secret + "@h:9000/db?dial_timeout=abc", // malformed duration
		"clickhouse://h:9000/db?http_proxy=" + url.QueryEscape("http://u:"+secret+"@proxy:3128/%zz"),
	} {
		_, _, err := resolve(cfg(raw, time.Second))
		if err == nil {
			t.Errorf("%q must be rejected", raw)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a credential appeared in the error message: %v", err)
		}
	}
}

func TestResolve_MultiHostDSNRecordsFirstHost(t *testing.T) {
	// The URL Host is the whole string "h1:9000,h2:9001", which the driver splits on commas and tries in turn. The connection info records
	// only the first: with the whole string, server.address in the Span would be that whole string and server.port would not parse.
	dsn := "clickhouse://u:p@h1:9000,h2:9001/db"
	got, info, err := resolve(cfg(dsn, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if info.Addr != "h1:9000" {
		t.Errorf("with several hosts the connection info must record the first host h1:9000, got=%q", info.Addr)
	}
	// Both hosts are still present in the DSN handed to the driver.
	opts, err := chgo.ParseDSN(got)
	if err != nil || !slices.Equal(opts.Addr, []string{"h1:9000", "h2:9001"}) {
		t.Errorf("the host list in the DSN must not be touched, got=%v err=%v", opts.Addr, err)
	}
}

func TestResolve_AcceptsAllSchemes(t *testing.T) {
	for _, scheme := range []string{"clickhouse", "tcp", "http", "https"} {
		dsn := scheme + "://h:9000/db"
		if scheme == "https" {
			dsn += "?secure=true" // the driver requires secure to be on together with https
		}
		_, info, err := resolve(cfg(dsn, time.Second))
		if err != nil {
			t.Fatalf("%s: %v", scheme, err)
		}
		if info.Addr != "h:9000" {
			t.Errorf("%s: the address must be resolved, got=%+v", scheme, info)
		}
	}
}

func TestResolve_ParseErrorOmitsDSN(t *testing.T) {
	// The error from url.Parse carries the whole DSN, and the error does get recorded.
	const secret = "hunter2"
	_, _, err := resolve(cfg("clickhouse://u:"+secret+"@h:9000/db\x7f\x00", time.Second))
	if err == nil {
		t.Fatal("an invalid DSN must be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("a credential appeared in the error message: %v", err)
	}
}

func mustQuery(t *testing.T, dsn string) url.Values {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestResolve_QueryUnparsableErrorsInsteadOfDroppingParams(t *testing.T) {
	// Same reason as the postgres branch in xgorm: swallowing the parse error would make the password vanish.
	const secret = "p%ssw0rd"
	_, _, err := resolve(cfg("clickhouse://h:9000/db?password="+secret, time.Second))
	if err == nil {
		t.Fatal("an unparseable query must be an error, not a silently dropped parameter")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("a credential appeared in the error message: %v", err)
	}
}

func TestResolve_ValidPercentEscapesKept(t *testing.T) {
	dsn, _, err := resolve(cfg("clickhouse://h:9000/db?password=p%25ssw0rd", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustQuery(t, dsn).Get("password"); got != "p%ssw0rd" {
		t.Errorf("the password must be left as is, got=%q", got)
	}
}

func TestOpen_DisablesDriverVersionQuery(t *testing.T) {
	// That query hard-codes context.Background() and runs inside gorm.Open, where retries never get a turn.
	d, ok := open("clickhouse://h:9000/db").(*clickhouse.Dialector)
	if !ok {
		t.Fatalf("must be the driver's own Dialector, got %T", d)
	}
	if !d.SkipInitializeWithVersion {
		t.Error("the version query in Initialize must be turned off and moved into Ready")
	}
}

func TestApplyVersion_MatchesDriverRules(t *testing.T) {
	cases := []struct {
		v                   string
		noRename, noPrecise bool
	}{
		{"20.3.1.1", true, true},
		{"21.8.3.44", false, true},
		{"23.8.2.7", false, false},
		{"not-a-version", false, false},
	}
	for _, c := range cases {
		var got clickhouse.Config
		applyVersion(&got, c.v)
		if got.DontSupportRenameColumn != c.noRename || got.DontSupportColumnPrecision != c.noPrecise {
			t.Errorf("%s: got rename=%v precision=%v", c.v, got.DontSupportRenameColumn, got.DontSupportColumnPrecision)
		}
	}
}

func TestProbeVersion_SetsVersionOnDialectAndHonorsCtx(t *testing.T) {
	db := fakeCH(t, "20.3.1.1")
	if err := probeVersion(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	d := db.Dialector.(*clickhouse.Dialector)
	if d.Version != "20.3.1.1" || !d.DontSupportRenameColumn || !d.DontSupportColumnPrecision {
		t.Errorf("the version and both switches must be set, got version=%q config=%+v", d.Version, *d.Config)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeVersion(ctx, fakeCH(t, "23.8")); !errors.Is(err, context.Canceled) {
		t.Errorf("nothing may be queried once the ctx is cancelled, got %v", err)
	}
}

func TestRegister_ReadyIsVersionProbe(t *testing.T) {
	// The call site: Open turned off the driver's own query, so without Ready wired up nothing would ever read the version.
	if dialect.Ready == nil {
		t.Fatal("the clickhouse dialect must carry a Ready")
	}
	db := fakeCH(t, "20.3.1.1")
	if err := dialect.Ready(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !db.Dialector.(*clickhouse.Dialector).DontSupportRenameColumn {
		t.Error("the registered Ready must be probeVersion")
	}
}

func TestNew_FirstConnectHonorsCtxAndRetries(t *testing.T) {
	// The driver queries the version with context.Background() inside gorm.Open: measured against an address that accepts
	// the connection but never answers, it waits out the full dial_timeout even though the ctx was cancelled long before, and then just fails --
	// xgorm's connect retries never got a turn.
	addr, accepts := withSilentServer(t)
	// dial_timeout is 5s: without the ctx wired up it would wait that out, and the 2s upper bound is far from both ends.
	c := cfg("clickhouse://u:p@"+addr+"/db", 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, _, err := xgorm.New(ctx, c); err == nil {
		t.Fatal("connecting must not succeed once the ctx is cancelled")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a cancelled ctx must make it give up immediately, waited %v", elapsed)
	}

	c.DialTimeout = 50 * time.Millisecond
	accepts.Store(0)
	if _, _, err := xgorm.New(context.Background(), c); err == nil {
		t.Fatal("connecting must not succeed when the peer never answers")
	}
	if n := accepts.Load(); n < 3 {
		t.Errorf("a failed first connection must go through xgorm's retries, but it connected only %d times", n)
	}
}

// withSilentServer starts a server that accepts connections and never answers, and returns the address and the total connection count.
func withSilentServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepts atomic.Int64
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				for _, c := range conns {
					c.Close()
				}
				return
			}
			accepts.Add(1)
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	return ln.Addr().String(), &accepts
}

// fakeCH builds a *gorm.DB whose dialect is the real ClickHouse Dialector while the pool behind it is a fake driver
// that only returns a version number. The version-query step measures the switches on the dialect, so no real ClickHouse is needed.
func fakeCH(t *testing.T, v string) *gorm.DB {
	t.Helper()
	pool := sql.OpenDB(verConnector{v})
	t.Cleanup(func() { pool.Close() })
	db, err := gorm.Open(clickhouse.New(clickhouse.Config{Conn: pool, SkipInitializeWithVersion: true}),
		&gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

type verConnector struct{ v string }

func (c verConnector) Connect(context.Context) (driver.Conn, error) { return verConn(c), nil }
func (c verConnector) Driver() driver.Driver                        { return nil }

type verConn struct{ v string }

func (c verConn) Prepare(string) (driver.Stmt, error) { return verStmt(c), nil }
func (verConn) Close() error                          { return nil }
func (verConn) Begin() (driver.Tx, error)             { return nil, errors.New("not supported") }

type verStmt struct{ v string }

func (verStmt) Close() error                                { return nil }
func (verStmt) NumInput() int                               { return 0 }
func (verStmt) Exec([]driver.Value) (driver.Result, error)  { return nil, errors.New("not supported") }
func (s verStmt) Query([]driver.Value) (driver.Rows, error) { return &verRows{v: s.v}, nil }

type verRows struct {
	v    string
	done bool
}

func (*verRows) Columns() []string { return []string{"version()"} }
func (*verRows) Close() error      { return nil }
func (r *verRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done, dest[0] = true, r.v
	return nil
}
