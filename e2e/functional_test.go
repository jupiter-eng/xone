package e2e

// 功能端到端测试：TestFunctional_* 系列。
//
//	scripts/e2e.sh -run Functional
//
// 每一条断言都对照 README.md / docs/architecture.md / docs/config.md / 各模块 README 里写下的行为（没写进文档、只在代码里的，
// 以代码为准并在注释里注明出处）。失败信息一律写成「文档说 X，实际 Y」。
//
// 文件划分：
//
//	functional_test.go          本文件：共用的小工具
//	functional_api_test.go      每个接口的返回
//	functional_cache_test.go    本地缓存 → Redis → PG 三级读
//	functional_log_test.go      访问日志字段、trace_id 贯通、脱敏、PG 密码不外泄、SQL 日志
//	functional_metrics_test.go  /metrics
//	functional_trace_test.go    Span 属性与父子关系、traceparent、透传头
//	functional_flow_test.go     /boom 与 xflow 回滚
//	functional_config_test.go   配置写错时启动失败
//
// 数字（耗时、次数）都用 t.Logf 打出来，行首带「数字：」，方便从 -v 的输出里 grep。

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// waitFor 等日志、Span、指标出现的上限。
//
// 本机实测（4 核，全部 e2e 用例并行、-count=3，约 70 次等待）：响应回到客户端之后，
// 访问日志 p50 0.05ms、最慢 4.1ms 就位；服务端 Span p50 0.7ms、最慢 3.9ms 落盘。
// 给到 5s，是比最慢一次多三个数量级的余量，慢机器、CI 上也不抖
const waitFor = 5 * time.Second

var (
	hexTraceID = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hexSpanID  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// randomTraceID / randomSpanID 当作上游传来的链路标识
func randomTraceID() string { return harness.NewID() + harness.NewID() + harness.NewID()[:8] }
func randomSpanID() string  { return harness.NewID() + harness.NewID()[:4] }

// traceIDOf 响应头里的 X-Trace-Id。
// xgin/README.md XGin.Trace：「每个请求一个服务端 Span，响应头回带 X-Trace-Id」
func traceIDOf(t *testing.T, r harness.Response) string {
	t.Helper()
	id := r.Header.Get("X-Trace-Id")
	if !hexTraceID.MatchString(id) {
		t.Fatalf("文档说 XGin.Trace 开着时响应头回带 X-Trace-Id，实际是 %q（%v）", id, r)
	}
	return id
}

// serverSpan 等到这条链路的服务端 Span。
//
// 子 Span（SQL、Redis、出站 HTTP）都在 handler 返回之前结束，服务端 Span 在写完响应之后
// 才结束，导出又是同步的，所以等到它的时候整条链路已经齐了
func serverSpan(t *testing.T, p *harness.Process, traceID string) harness.Span {
	t.Helper()
	return p.WaitSpan(t, waitFor, func(s harness.Span) bool { return s.TraceID == traceID && s.Kind == "server" })
}

// traceSpans 这条链路上已经导出的全部 Span，按开始时间排好
func traceSpans(t *testing.T, p *harness.Process, traceID string) []harness.Span {
	t.Helper()
	var out []harness.Span
	for _, s := range p.Spans(t) {
		if s.TraceID == traceID {
			out = append(out, s)
		}
	}
	for i := 1; i < len(out); i++ { // 插入排序：一条链路只有几个 Span
		for j := i; j > 0 && out[j].Start.Before(out[j-1].Start); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// spansNamed 按名字挑
func spansNamed(spans []harness.Span, name string) []harness.Span {
	var out []harness.Span
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// clientSpanNames 客户端 Span（SQL、Redis、出站 HTTP）的名字，按开始时间，逗号分隔
func clientSpanNames(spans []harness.Span) string {
	var names []string
	for _, s := range spans {
		if s.Kind == "client" {
			names = append(names, s.Name)
		}
	}
	return strings.Join(names, ",")
}

// spanNames 调试输出用
func spanNames(spans []harness.Span) string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name + "(" + s.Kind + ")"
	}
	return strings.Join(names, ", ")
}

// accessLog 等到这条链路的访问日志。
//
// 等不到时分清是哪一种：访问日志根本没打，还是打了、但 trace_id 不是这次请求的——
// 后者就是 README 说的「日志自动带上 trace_id」没兑现，只报一句「没等到日志」的话看不出来
func accessLog(t *testing.T, p *harness.Process, traceID string) harness.Log {
	t.Helper()
	isAccess := func(l harness.Log) bool { return l.Msg() == "request completed" }
	if l, ok := p.LookForLog(waitFor, func(l harness.Log) bool { return isAccess(l) && l.Str("trace_id") == traceID }); ok {
		return l
	}
	var ids []string
	for _, l := range p.FindLogs(isAccess) {
		ids = append(ids, fmt.Sprintf("%q", l.Str("trace_id")))
	}
	t.Fatalf("等了 %v 没有 trace_id=%s 的访问日志。README：日志自动带上 trace_id，和响应头 X-Trace-Id 是同一个；"+
		"进程里已有的 %d 条访问日志的 trace_id 是 [%s]", waitFor, traceID, len(ids), strings.Join(ids, ", "))
	return harness.Log{}
}

// waitMetrics 反复抓 /metrics 直到 cond 成立，返回最后一次的结果。
//
// 请求指标在中间件的 defer 里记，理论上可能晚于响应到达客户端，所以不能发完请求就断言
func waitMetrics(t *testing.T, p *harness.Process, what string, cond func(harness.Metrics) bool) harness.Metrics {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for {
		m := p.Metrics(t)
		if cond(m) {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("等了 %v，/metrics 仍不满足：%s", waitFor, what)
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustNotContain text 里不许出现任何一个 secret
func mustNotContain(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if i := strings.Index(text, s); i >= 0 {
			lo, hi := max(0, i-120), min(len(text), i+len(s)+120)
			t.Errorf("%s 里出现了不该出现的 %q：…%s…", where, s, text[lo:hi])
		}
	}
}

// 开关的 Overlay 片段。顶层块各不相同的片段可以直接拼起来；同一个顶层块里的几项要写在一个片段里
// （YAML 里同一个 key 出现两次是错误）。service/application.yml 没写这些项，不叠就是框架的默认值
const (
	// payloadLogs 访问日志里能记的全打开：查询串、请求头、请求体、响应头、响应体
	payloadLogs = "XGin:\n  LogQuery: true\n  LogRequestHeaders: true\n  LogRequestBody: true\n" +
		"  LogResponseHeaders: true\n  LogResponseBody: true\n"
	// debugLogs 日志级别 debug
	debugLogs = "XLog:\n  Level: debug\n"
)

// logLevel 日志级别
func logLevel(level string) string { return "XLog:\n  Level: " + level + "\n" }

// sampleRatio 链路采样率
func sampleRatio(r string) string { return "XTrace:\n  SampleRatio: " + r + "\n" }

// stopTimeout 整个退出流程的预算（Service.StopTimeout → xone.WithStopTimeout）
func stopTimeout(d time.Duration) string { return fmt.Sprintf("Service:\n  StopTimeout: %v\n", d) }

// sqlLog 打开这几个 xgorm 实例（default 是 PG、mysql、ch）的 SQL 日志
func sqlLog(clients ...string) string {
	s := "XGorm:\n  Clients:\n"
	for _, c := range clients {
		s += "    " + c + ":\n      Log: true\n"
	}
	return s
}

// pgPassword harness 连 PG 用的密码，从它拼出来的 DSN 里取，不在这里再抄一遍默认值
func pgPassword(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(harness.PGDSN(harness.PGAddr()))
	if err != nil {
		t.Fatalf("parse harness DSN: %v", err)
	}
	pw, ok := u.User.Password()
	if !ok || pw == "" {
		t.Fatal("harness DSN has no password")
	}
	return pw
}

// user 接口里的用户
type user struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Source string `json:"source"`
}

// createUser POST /users，要求 201
func createUser(t *testing.T, p *harness.Process, name, email string) user {
	t.Helper()
	r := p.PostJSON(t, "/users", map[string]string{"name": name, "email": email})
	if r.Status != 201 {
		t.Fatalf("POST /users 应返回 201，实际 %v", r)
	}
	var u user
	r.JSON(t, &u)
	if u.ID <= 0 {
		t.Fatalf("POST /users 没回 id：%v", r)
	}
	return u
}

// getUser GET /users/:id，要求 200，返回用户和耗时
func getUser(t *testing.T, p *harness.Process, id int64) (user, harness.Response, time.Duration) {
	t.Helper()
	start := time.Now()
	r := p.Get(t, fmt.Sprintf("/users/%d", id))
	elapsed := time.Since(start)
	if r.Status != 200 {
		t.Fatalf("GET /users/%d 应返回 200，实际 %v", id, r)
	}
	var u user
	r.JSON(t, &u)
	return u, r, elapsed
}

// num 日志里的数字字段（JSON 解出来是 float64）
func num(l harness.Log, path string) (float64, bool) {
	v, ok := l.Get(path)
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}
