package e2e

// MySQL 端到端测试：TestMySQL_* 系列，e2e 服务的第二个 xgorm 实例 xgorm.C("mysql")。
//
//	scripts/e2e.sh -run MySQL
//	scripts/e2e.sh --load -run MySQL_Load   # 压测
//
// 服务的 XGorm 是多实例写法（service/application.yml）：default 是 PostgreSQL，mysql 是 MySQL。
// 断言对照 xgorm/README.md「配置」与 README 写下的行为，失败信息写成「文档说 X，实际 Y」；
// 文档没写死的数只量、不卡，以「数字：」开头打在 t.Logf 里。
//
// 文件划分：
//
//	mysql_test.go             本文件：共用的小工具和按文档默认值推出来的常量
//	mysql_functional_test.go  增删改查、SQL 日志、Span、按实例的连接池指标、密码不外泄、超时按 DSN / 配置
//	mysql_fault_test.go       运行中断开 / 卡住 MySQL、启动时不可达 / 密码错
//	mysql_shutdown_test.go    SIGTERM 时在途的 MySQL 查询、卡住的查询听不听请求的 ctx
//	mysql_load_test.go        裸 gin + MySQL 对照 xone + MySQL（XONE_E2E_LOAD=1）

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// xgorm/README.md XGorm 的默认值；service/application.yml 的 mysql 实例没改这几项
const (
	mysqlDialTimeout = 500 * time.Millisecond // DialTimeout，注入 DSN 的 timeout
	mysqlReadTimeout = 3 * time.Second        // MySQL.ReadTimeout，注入 DSN 的 readTimeout
	// mysqlAttempt「单次建连探测的预算：MySQL 是 DialTimeout + MySQL.ReadTimeout」
	mysqlAttempt = mysqlDialTimeout + mysqlReadTimeout
	// mysqlStartBudget「建连重试」：3 次 × 单次探测预算 + 两次退避的上界 1s、2s
	mysqlStartBudget = faultPingAttempts*mysqlAttempt + faultPingBackoffs // 13.5s
)

// mysqlUser MySQL 那一组接口里的用户
type mysqlUser struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// mysqlCreate POST /mysql/users，要求 201
func mysqlCreate(t *testing.T, p *harness.Process, name, email string) (mysqlUser, harness.Response) {
	t.Helper()
	r := p.PostJSON(t, "/mysql/users", map[string]string{"name": name, "email": email})
	if r.Status != http.StatusCreated {
		t.Fatalf("POST /mysql/users 应返回 201，实际 %v", r)
	}
	var u mysqlUser
	r.JSON(t, &u)
	if u.ID <= 0 {
		t.Fatalf("POST /mysql/users 没回 id：%v", r)
	}
	return u, r
}

// mysqlRow 直连 MySQL 读这个进程用户表里的一行；没有时 ok 为 false
func mysqlRow(t *testing.T, p *harness.Process, id int64) (u mysqlUser, ok bool) {
	t.Helper()
	db := harness.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+p.Table+" WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("直连 MySQL 读 %s：%v", p.Table, err)
	}
	if n == 0 {
		return u, false
	}
	if err := db.QueryRowContext(ctx, "SELECT id, name, email FROM "+p.Table+" WHERE id = ?", id).Scan(&u.ID, &u.Name, &u.Email); err != nil {
		t.Fatalf("直连 MySQL 读 %s：%v", p.Table, err)
	}
	return u, true
}

// mysqlOverlay 只改 mysql 这一个实例的配置：body 是 XGorm.Clients.mysql 下面的 YAML，每行两格缩进写
func mysqlOverlay(body string) string {
	return "XGorm:\n  Clients:\n    mysql:\n" + indent(body, "      ")
}

// indent 每一行前面加上 prefix
func indent(s, prefix string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(prefix + l + "\n")
	}
	return b.String()
}

// mysqlDep GET /dep?target=mysql，客户端最多等 wait
func mysqlDep(p *harness.Process, timeout string, wait time.Duration) faultDepResult {
	return faultDep(p, "mysql", timeout, wait)
}

// mysqlWaitDep 反复探 MySQL 直到回 200，返回用了多久和探了几次；limit 内没恢复就 t.Fatal
func mysqlWaitDep(t *testing.T, p *harness.Process, limit time.Duration) (time.Duration, int) {
	t.Helper()
	return faultWaitRecover(t, p, "mysql", limit)
}

// mysqlSecretDSN 把 MySQL DSN 里的密码换成 secret
func mysqlSecretDSN(addr, secret string) string {
	return fmt.Sprintf("xone:%s@tcp(%s)/xone_e2e", secret, addr)
}
