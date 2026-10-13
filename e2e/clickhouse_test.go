package e2e

// ClickHouse 端到端测试：TestClickHouse_* 系列，e2e 服务的第三个 xgorm 实例 xgorm.C("ch")，
// 经 xgorm/clickhouse 方言、native 协议连 Docker 里的 ClickHouse 24.8。
//
//	scripts/e2e.sh -run ClickHouse
//	scripts/e2e.sh --load -run ClickHouse_Load   # 压测
//
// ClickHouse 实例是可选的：harness.Options.ClickHouse 激活 service/application-ch.yml 那份 profile 才有。
// ClickHouse 不可用（没有 Docker、容器起不来）时这一组各自跳过（harness.RequireCH），其余 e2e 照跑。
//
// 断言对照 xgorm/clickhouse/README.md「配置」「ClickHouse 的超时与取消」、README 与 xgorm/clickhouse 的包文档；
// 失败信息写成「文档说 X，实际 Y」。文档没写死的数只量、不卡，以「数字：」开头打在 t.Logf 里。
//
// 文件划分：
//
//	clickhouse_test.go             本文件：共用的小工具和按文档默认值推出来的常量
//	clickhouse_functional_test.go  写入 / 查询、SQL 日志、Span、连接池指标、密码不外泄、DSN 规则、版本探测
//	clickhouse_auth_test.go        密码错：native 协议认得出、不重试；HTTP 协议认不出
//	clickhouse_fault_test.go       运行中断开 / 卡住 ClickHouse、启动时不可达、启动期间收到 SIGTERM
//	clickhouse_shutdown_test.go    SIGTERM 时在途的查询、卡住的查询听不听请求的 ctx
//	clickhouse_load_test.go        裸 gin + ClickHouse 对照 xone + ClickHouse（XONE_E2E_LOAD=1）
//	clickhouse_tls_test.go         TLS 块：另起一个开着 TLS 端口的容器，native 与 https 各走一遍

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// xgorm/README.md XGorm 的默认值与 ClickHouse 那两节；service/application-ch.yml 没改这几项
const (
	chDialTimeout = 500 * time.Millisecond // DialTimeout，注入 DSN 的 dial_timeout
	// chAttempt「其余驱动是 2 × DialTimeout」：xgorm/clickhouse 的 resolve 按驱动读出来的 dial_timeout 算
	chAttempt = 2 * chDialTimeout
	// chStartBudget「建连重试」：3 次 × 单次探测预算 + 两次退避的上界 1s、2s
	chStartBudget = faultPingAttempts*chAttempt + faultPingBackoffs // 6s
	// chReadTimeout「read_timeout 没写时是 300s（setDefaults）」
	chReadTimeout = 300 * time.Second
)

// chEvent ClickHouse 那一组接口里的一行
type chEvent struct {
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// chInsert POST /ch/events，要求 201，返回服务发的 id
func chInsert(t *testing.T, p *harness.Process, name string, values ...int64) ([]uint64, harness.Response) {
	t.Helper()
	r := p.PostJSON(t, "/ch/events", map[string]any{"name": name, "values": values})
	if r.Status != http.StatusCreated {
		t.Fatalf("POST /ch/events 应返回 201，实际 %v", r)
	}
	var body struct {
		IDs []uint64 `json:"ids"`
	}
	r.JSON(t, &body)
	if len(body.IDs) != len(values) {
		t.Fatalf("POST /ch/events 写了 %d 行，应回 %d 个 id，实际 %v", len(values), len(values), r)
	}
	return body.IDs, r
}

// chRows 直连 ClickHouse 读这个进程事件表里 id 在 ids 里的行，按 id 排序
func chRows(t *testing.T, p *harness.Process, ids ...uint64) []chEvent {
	t.Helper()
	db := harness.CH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT id, name, value FROM "+p.Table+" WHERE has(?, id) ORDER BY id", ids)
	if err != nil {
		t.Fatalf("直连 ClickHouse 读 %s：%v", p.Table, err)
	}
	defer rows.Close()
	var out []chEvent
	for rows.Next() {
		var e chEvent
		if err := rows.Scan(&e.ID, &e.Name, &e.Value); err != nil {
			t.Fatalf("直连 ClickHouse 读 %s：%v", p.Table, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("直连 ClickHouse 读 %s：%v", p.Table, err)
	}
	return out
}

// chOverlay 只改 ch 这一个实例的配置：body 是 XGorm.Clients.ch 下面的 YAML，每行两格缩进写
func chOverlay(body string) string {
	return "XGorm:\n  Clients:\n    ch:\n" + indent(body, "      ")
}

// chDep GET /dep?target=ch，客户端最多等 wait
func chDep(p *harness.Process, timeout string, wait time.Duration) faultDepResult {
	return faultDep(p, "ch", timeout, wait)
}

// chWaitDep 反复探 ClickHouse 直到回 200，返回用了多久和探了几次；limit 内没恢复就 t.Fatal
func chWaitDep(t *testing.T, p *harness.Process, limit time.Duration) (time.Duration, int) {
	t.Helper()
	return faultWaitRecover(t, p, "ch", limit)
}

// chStart 起一个带 ClickHouse 实例的进程
func chStart(t *testing.T, o harness.Options) *harness.Process {
	t.Helper()
	o.ClickHouse = true
	return harness.Start(t, o)
}
