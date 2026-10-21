package e2e

// The MySQL end-to-end tests: the TestMySQL_* family, against the e2e service's second xgorm
// instance xgorm.C("mysql").
//
//	scripts/e2e.sh -run MySQL
//	scripts/e2e.sh --load -run MySQL_Load   # the load tests
//
// The service's XGorm is written with several instances (service/application.yml): default is
// PostgreSQL and mysql is MySQL.
// The assertions follow xgorm/README.md, "Configuration", and the behavior the README writes down,
// and a failure message reads "the documentation says X, got Y"; a number the documentation does
// not pin down is measured and not asserted, and printed with t.Logf behind a "numbers:" prefix.
//
// How the files divide up:
//
//	mysql_test.go             this file: the shared helpers and the constants that follow from the documented defaults
//	mysql_functional_test.go  create / read / update / delete, the SQL log, spans, the per-instance pool metrics, no password leaking, the timeouts from the DSN / the configuration
//	mysql_fault_test.go       MySQL broken / stuck while running, unreachable / a wrong password at start
//	mysql_shutdown_test.go    a MySQL query in flight on SIGTERM, and whether a stuck query listens to the request's ctx
//	mysql_load_test.go        bare gin + MySQL against xone + MySQL (XONE_E2E_LOAD=1)

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// XGorm's defaults from xgorm/README.md; the mysql instance in service/application.yml changes none of them
const (
	mysqlDialTimeout = 500 * time.Millisecond // DialTimeout, injected into the DSN as timeout
	mysqlReadTimeout = 3 * time.Second        // MySQL.ReadTimeout, injected into the DSN as readTimeout
	// mysqlAttempt: "the budget of one connect probe: for MySQL it is DialTimeout + MySQL.ReadTimeout"
	mysqlAttempt = mysqlDialTimeout + mysqlReadTimeout
	// mysqlStartBudget, "Connect retries": 3 attempts x the budget of one probe + the 1s and 2s upper bounds of the two backoffs
	mysqlStartBudget = faultPingAttempts*mysqlAttempt + faultPingBackoffs // 13.5s
)

// mysqlUser is a user as the MySQL endpoints see it
type mysqlUser struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// mysqlCreate POSTs /mysql/users and wants a 201
func mysqlCreate(t *testing.T, p *harness.Process, name, email string) (mysqlUser, harness.Response) {
	t.Helper()
	r := p.PostJSON(t, "/mysql/users", map[string]string{"name": name, "email": email})
	if r.Status != http.StatusCreated {
		t.Fatalf("POST /mysql/users should answer 201, got %v", r)
	}
	var u mysqlUser
	r.JSON(t, &u)
	if u.ID <= 0 {
		t.Fatalf("POST /mysql/users answered no id: %v", r)
	}
	return u, r
}

// mysqlRow reads one row of this process's user table on MySQL directly; ok is false when there is none
func mysqlRow(t *testing.T, p *harness.Process, id int64) (u mysqlUser, ok bool) {
	t.Helper()
	db := harness.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+p.Table+" WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("reading %s on MySQL directly: %v", p.Table, err)
	}
	if n == 0 {
		return u, false
	}
	if err := db.QueryRowContext(ctx, "SELECT id, name, email FROM "+p.Table+" WHERE id = ?", id).Scan(&u.ID, &u.Name, &u.Email); err != nil {
		t.Fatalf("reading %s on MySQL directly: %v", p.Table, err)
	}
	return u, true
}

// mysqlOverlay configures the mysql instance alone: body is the YAML under XGorm.Clients.mysql, written two spaces in
func mysqlOverlay(body string) string {
	return "XGorm:\n  Clients:\n    mysql:\n" + indent(body, "      ")
}

// indent puts prefix in front of every line
func indent(s, prefix string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(prefix + l + "\n")
	}
	return b.String()
}

// mysqlDep GETs /dep?target=mysql, with the client waiting wait at most
func mysqlDep(p *harness.Process, timeout string, wait time.Duration) faultDepResult {
	return faultDep(p, "mysql", timeout, wait)
}

// mysqlWaitDep probes MySQL until it answers 200 and returns how long that took and how many
// probes it needed; it t.Fatals when it has not recovered within limit
func mysqlWaitDep(t *testing.T, p *harness.Process, limit time.Duration) (time.Duration, int) {
	t.Helper()
	return faultWaitRecover(t, p, "mysql", limit)
}

// mysqlSecretDSN puts secret in a MySQL DSN as the password
func mysqlSecretDSN(addr, secret string) string {
	return fmt.Sprintf("xone:%s@tcp(%s)/xone_e2e", secret, addr)
}
