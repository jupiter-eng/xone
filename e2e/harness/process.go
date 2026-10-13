package harness

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Options 起一个进程的参数。零值就能用：直连 PG / MySQL / Redis、随机端口、按测试生成表名
type Options struct {
	// Env 额外的环境变量，覆盖 harness 设的同名变量（名字见 service/application.yml 开头）。
	// 只用来换注入的值，比如一条故意写错的 DSN；切开关用 Overlay
	Env map[string]string

	// Overlay 叠加在配置文件上的 YAML，写成一份 profile（--profile=e2e）叠上去：
	// map 递归合并、列表整体替换，和生产上的 profile 一个规矩。配置项的开关都用它
	Overlay string

	// Config 换一份配置文件，默认 e2e/service/application.yml。只对 Start 有效
	Config string

	// Args 追加的命令行参数
	Args []string

	// PGAddr / MySQLAddr / RedisAddr 服务连的地址，默认直连。故障测试传 Proxy.Addr()
	PGAddr    string
	MySQLAddr string
	RedisAddr string

	// ClickHouse 给服务加上第三个 xgorm 实例 ch：激活 service/application-ch.yml 那份 profile
	// （有 Overlay 时是 --profile=ch,e2e，Overlay 压过它）。只对 Start 有效。
	// CHAddr 是它连的 native 地址，默认直连；给了 CHAddr 就等于 ClickHouse: true。
	// 要换协议、换密码，直接在 Env 里给 E2E_CH_DSN
	ClickHouse bool
	CHAddr     string

	// Kafka 给服务配上 XKafka：激活 service/application-kafka.yml 那份 profile（在 ch 之后、e2e 之前）。
	// 只对 Start 有效。KafkaAddr 是 broker 地址，默认直连；给了 KafkaAddr 就等于 Kafka: true。
	// KafkaTopic 非空时服务消费它（Service.KafkaTopic），消费组是 KafkaGroup（空的话是 e2e_<随机>，见 Process.KafkaGroup）
	Kafka      bool
	KafkaAddr  string
	KafkaTopic string
	KafkaGroup string

	// Downstream /proxy 调的下游 base URL，比如 Stub.URL 或 "http://" + proxy.Addr()
	Downstream string

	// Spans 把 Span 写进 Process.SpanFile，用 Process.Spans / WaitSpan 读
	Spans bool

	// LogFile 让 xlog 同时写文件（目录是 Process.Dir），用 Process.FileLogs 读
	LogFile bool

	// DiscardStdout 标准输出直接接 /dev/null，不经 harness 收集：
	// 压测时访问日志一秒几万行，收进内存既占地方又可能拖慢被测进程。stderr 照收
	DiscardStdout bool

	// Table / KeyPrefix 默认按 NewID 生成，测试结束时删掉（PG 和 MySQL 上的同名表都删）。
	// 两个进程要共用数据时显式给同一个
	Table     string
	KeyPrefix string

	// Port 默认 FreePort
	Port int

	// NoWait 不等 /ping 就绪就返回：测启动失败、启动期间收信号用
	NoWait bool

	// ReadyTimeout 等 /ping 的上限，默认 30s
	ReadyTimeout time.Duration

	// NonJSON 非空时，进程退出后不检查它的每一行输出都是 JSON（见 checkOutput），写上为什么：
	// 比如 "XLog.Format: text"。数据竞争照查
	NonJSON string
}

// Process 一个跑着的服务进程
type Process struct {
	name string

	// Port 监听的端口，Base 是 http://127.0.0.1:Port
	Port int
	Base string
	// Table / KeyPrefix 这个进程用的表名（订单表加 _orders；MySQL 上的用户表、ClickHouse 上的事件表同名）和 key 前缀
	Table     string
	KeyPrefix string
	// CH 这个进程有没有 ClickHouse 实例（Options.ClickHouse）
	CH bool
	// KafkaGroup 这个进程消费用的消费组（Options.Kafka 时）
	KafkaGroup string
	// Dir 这个进程的临时目录，测试结束时删掉
	Dir string
	// SpanFile Options.Spans 开着时 Span 写在这里
	SpanFile string

	logDir  string
	nonJSON string
	cmd     *exec.Cmd
	out     *output
	flush   []*lineWriter
	client  *http.Client
	started time.Time

	firstSignal atomic.Int64 // UnixNano，0 表示还没发过
	done        chan struct{}
	exit        Exit
}

// Exit 进程怎么退出的
type Exit struct {
	// Code 退出码。被信号杀掉时是 -1，Signal 说明是哪个
	Code   int
	Signal os.Signal
	// Uptime 从启动到退出
	Uptime time.Duration
	// SinceSignal 从第一次发信号到退出，没发过信号时是 0
	SinceSignal time.Duration
}

func (e Exit) String() string {
	if e.Signal != nil {
		return fmt.Sprintf("killed by %v after %v (%v since the first signal)", e.Signal, e.Uptime, e.SinceSignal)
	}
	return fmt.Sprintf("exit code %d after %v (%v since the first signal)", e.Code, e.Uptime, e.SinceSignal)
}

// Start 起 e2e 服务，等到 GET /ping 返回 200（Options.NoWait 时不等）。
//
// 测试结束时进程还活着就 SIGKILL；测试失败时把最后 80 行输出打进测试日志
func Start(t testing.TB, o Options) *Process {
	t.Helper()
	bin := ServiceBinary(t)
	dir := t.TempDir()

	cfg := o.Config
	if cfg == "" {
		cfg = filepath.Join(ModuleDir(), "service", "application.yml")
	}
	// 子进程的工作目录是 dir，相对路径到了那边就指错了地方
	cfg, err := filepath.Abs(cfg)
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}
	args := []string{"--config=" + cfg}
	if o.CHAddr != "" {
		o.ClickHouse = true
	}
	if o.ClickHouse {
		RequireCH(t)
	}
	if o.KafkaAddr != "" {
		o.Kafka = true
	}
	if o.Kafka {
		RequireKafka(t)
	}
	var profiles []string
	if o.ClickHouse || o.Kafka || o.Overlay != "" {
		// profile 文件名由 base 推出来，所以 base 也挪进临时目录
		base, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		cfg = filepath.Join(dir, "application.yml")
		writeFile(t, cfg, base)
	}
	if o.ClickHouse {
		ch, err := os.ReadFile(filepath.Join(ModuleDir(), "service", "application-ch.yml"))
		if err != nil {
			t.Fatalf("read clickhouse profile: %v", err)
		}
		writeFile(t, filepath.Join(dir, "application-ch.yml"), ch)
		profiles = append(profiles, "ch")
	}
	if o.Kafka {
		k, err := os.ReadFile(filepath.Join(ModuleDir(), "service", "application-kafka.yml"))
		if err != nil {
			t.Fatalf("read kafka profile: %v", err)
		}
		writeFile(t, filepath.Join(dir, "application-kafka.yml"), k)
		profiles = append(profiles, "kafka")
	}
	if o.Overlay != "" {
		writeFile(t, filepath.Join(dir, "application-e2e.yml"), []byte(o.Overlay))
		profiles = append(profiles, "e2e")
	}
	if len(profiles) > 0 {
		args = []string{"--config=" + cfg, "--profile=" + strings.Join(profiles, ",")}
	}
	return launch(t, "service", bin, append(args, o.Args...), dir, o)
}

// StartBaseline 起裸 gin 的对照服务。它不读配置文件，Overlay / Config / Spans / LogFile 对它无效
func StartBaseline(t testing.TB, o Options) *Process {
	t.Helper()
	if o.CHAddr != "" {
		o.ClickHouse = true
	}
	return launch(t, "baseline", BaselineBinary(t), o.Args, t.TempDir(), o)
}

// StartCovApp 起 covapp，参数原样交给它，不等就绪（它不监听）。Options 里只有 Env 对它有意义
func StartCovApp(t testing.TB, o Options, args ...string) *Process {
	t.Helper()
	o.NoWait = true
	return launch(t, "covapp", CovAppBinary(t), args, t.TempDir(), o)
}

// StartArgs 起 e2e 服务，命令行参数原样是 args：不像 Start 那样自己加 --config。
// 测 XONE_CONFIG、约定路径这些「没给 --config」时的找法用它。工作目录是 Process.Dir
func StartArgs(t testing.TB, o Options, args ...string) *Process {
	t.Helper()
	return launch(t, "service", ServiceBinary(t), args, t.TempDir(), o)
}

// WaitReadyWith 用 c 反复 GET url 直到 200，timeout 内没等到、或者进程先退出了就 t.Fatal。
// 给 HTTPS、h2c 这类 Start 自己的 http:// 就绪探测够不着的服务用（启动时传 Options.NoWait）
func (p *Process) WaitReadyWith(t testing.TB, c *http.Client, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := c.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-p.done:
			t.Fatalf("%s exited before it was ready: %v\n%s", p.name, p.exit, p.out.tail(80))
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready at %s within %v (last error: %v)\n%s", p.name, url, timeout, err, p.out.tail(80))
		}
	}
}

func launch(t testing.TB, name, bin string, args []string, dir string, o Options) *Process {
	t.Helper()
	p := &Process{name: name, Dir: dir, Port: o.Port, Table: o.Table, KeyPrefix: o.KeyPrefix, CH: o.ClickHouse, nonJSON: o.NonJSON}
	if p.Port == 0 {
		p.Port = FreePort(t)
	}
	p.Base = "http://127.0.0.1:" + strconv.Itoa(p.Port)
	id := NewID()
	if p.Table == "" {
		p.Table = "e2e_" + id
	}
	if p.KeyPrefix == "" {
		p.KeyPrefix = "e2e:" + id + ":"
	}
	if !tableName.MatchString(p.Table) {
		t.Fatalf("Options.Table %q must match %s: it goes into SQL as is", p.Table, tableName)
	}
	// 先登记删数据、后登记杀进程：Cleanup 倒着跑，进程先死，再删它的表
	t.Cleanup(func() { dropData(t, p.Table, p.KeyPrefix, p.CH) })

	vars := map[string]string{
		"E2E_PORT":       strconv.Itoa(p.Port),
		"E2E_PG_DSN":     PGDSN(or(o.PGAddr, PGAddr())),
		"E2E_MYSQL_DSN":  MySQLDSN(or(o.MySQLAddr, MySQLAddr())),
		"E2E_REDIS_ADDR": or(o.RedisAddr, RedisAddr()),
		"E2E_TABLE":      p.Table,
		"E2E_KEY_PREFIX": p.KeyPrefix,
	}
	if o.ClickHouse {
		vars["E2E_CH_DSN"] = CHDSN(or(o.CHAddr, CHAddr()))
	}
	if o.Kafka || o.KafkaAddr != "" {
		p.KafkaGroup = or(o.KafkaGroup, "e2e_"+id)
		vars["E2E_KAFKA_ADDR"] = or(o.KafkaAddr, KafkaAddr())
		vars["E2E_KAFKA_TOPIC"] = o.KafkaTopic
		vars["E2E_KAFKA_GROUP"] = p.KafkaGroup
	}
	if o.Downstream != "" {
		vars["E2E_DOWNSTREAM_URL"] = o.Downstream
	}
	if o.Spans {
		p.SpanFile = filepath.Join(dir, "spans.jsonl")
		vars["E2E_SPAN_FILE"] = p.SpanFile
	}
	if o.LogFile {
		p.logDir = dir
		vars["E2E_LOG_FILE"], vars["E2E_LOG_DIR"] = "true", dir
	}
	if RaceBuild() {
		// 竞争检测器默认在进程退出前睡 1s（等别的协程把报告写完），
		// 每个测退出时长的用例都会凭空多出这一秒
		vars["GORACE"] = "atexit_sleep_ms=0"
	}
	for k, v := range o.Env {
		vars[k] = v
	}

	p.out = newOutput()
	p.cmd = exec.Command(bin, args...)
	p.cmd.Dir = dir
	p.cmd.Env = childEnv(vars)
	p.cmd.SysProcAttr = sysProcAttr()
	if o.DiscardStdout {
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", os.DevNull, err)
		}
		defer devnull.Close() // 子进程拿到的是自己的那份 fd，这里关掉不影响它
		p.cmd.Stdout = devnull
	} else {
		w := p.out.writer("stdout")
		p.flush = append(p.flush, w)
		p.cmd.Stdout = w
	}
	errw := p.out.writer("stderr")
	p.flush = append(p.flush, errw)
	p.cmd.Stderr = errw

	p.client = &http.Client{Timeout: 30 * time.Second}
	p.done = make(chan struct{})
	p.started = time.Now()
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go p.reap()

	t.Cleanup(func() {
		if !p.Exited() {
			p.Kill()
			p.Wait(10 * time.Second)
		}
		// 裸 gin 的对照服务不是框架，它的输出不归这两条检查管
		if p.Exited() && name != "baseline" {
			p.checkOutput(t)
		}
		p.client.CloseIdleConnections()
		if t.Failed() {
			t.Logf("%s (pid %d) output, last 80 lines:\n%s", p.name, p.Pid(), p.out.tail(80))
		}
	})

	if !o.NoWait {
		p.waitReady(t, or(o.ReadyTimeout, 30*time.Second))
	}
	return p
}

// reap 等进程退出，记下怎么退出的
func (p *Process) reap() {
	err := p.cmd.Wait()
	now := time.Now()
	for _, w := range p.flush {
		w.flush()
	}
	e := Exit{Code: p.cmd.ProcessState.ExitCode(), Uptime: now.Sub(p.started)}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		e.Signal = ws.Signal()
	}
	if s := p.firstSignal.Load(); s != 0 {
		e.SinceSignal = now.Sub(time.Unix(0, s))
	}
	if err != nil && e.Code == 0 && e.Signal == nil {
		e.Code = -1 // Wait 本身出错（比如 I/O 拷贝失败），别让它看起来像正常退出
	}
	p.exit = e
	close(p.done)
}

func (p *Process) waitReady(t testing.TB, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	c := &http.Client{Timeout: time.Second}
	defer c.CloseIdleConnections()
	for {
		resp, err := c.Get(p.Base + "/ping")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-p.done:
			t.Fatalf("%s exited before it was ready: %v\n%s", p.name, p.exit, p.out.tail(80))
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready within %v (last error: %v)\n%s", p.name, timeout, err, p.out.tail(80))
		}
	}
}

// Pid 进程号
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// URL 拼出完整地址：p.URL("/users/1")
func (p *Process) URL(path string) string { return p.Base + path }

// Signal 发一个信号，第一次发的时刻记进 Exit.SinceSignal。进程已经退出时什么都不做
func (p *Process) Signal(sig os.Signal) {
	p.firstSignal.CompareAndSwap(0, time.Now().UnixNano())
	_ = p.cmd.Process.Signal(sig)
}

// Kill SIGKILL
func (p *Process) Kill() { _ = p.cmd.Process.Kill() }

// Exited 进程是否已经退出
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Done 进程退出时关闭
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait 最多等 timeout，返回退出情况；到点还没退出时第二个返回值是 false，进程不动它
func (p *Process) Wait(timeout time.Duration) (Exit, bool) {
	select {
	case <-p.done:
		return p.exit, true
	case <-time.After(timeout):
		return Exit{}, false
	}
}

// Terminate 发 SIGTERM 并等它退出，timeout 内没退出就 t.Fatal
func (p *Process) Terminate(t testing.TB, timeout time.Duration) Exit {
	t.Helper()
	p.Signal(syscall.SIGTERM)
	e, ok := p.Wait(timeout)
	if !ok {
		t.Fatalf("%s did not exit within %v after SIGTERM\n%s", p.name, timeout, p.out.tail(80))
	}
	return e
}

// Stdout 到目前为止标准输出的原文（Options.DiscardStdout 时是空的）
func (p *Process) Stdout() string { return p.out.text("stdout") }

// Stderr 到目前为止标准错误的原文。xlog 装好之前的框架日志、MustRun 返回的错误都在这里
func (p *Process) Stderr() string { return p.out.text("stderr") }

// Output stdout 和 stderr 混在一起，按到达顺序
func (p *Process) Output() string { return p.out.text("") }

// Logs 到目前为止 stdout、stderr 里的全部 JSON 日志，按到达顺序。
//
// xlog 装好之前框架用 slog 的默认格式往 stderr 写文本（... INFO starting hook=...），
// 那些不是 JSON，不在这里，看 Stderr；xlog 关掉之后的日志是写 stderr 的 JSON，在这里
func (p *Process) Logs() []Log { return p.out.logs() }

// FindLogs Logs 里满足 match 的那些
func (p *Process) FindLogs(match func(Log) bool) []Log {
	var out []Log
	for _, l := range p.Logs() {
		if match(l) {
			out = append(out, l)
		}
	}
	return out
}

// WaitLog 等到出现一条满足 match 的日志，timeout 内没等到就 t.Fatal
func (p *Process) WaitLog(t testing.TB, timeout time.Duration, match func(Log) bool) Log {
	t.Helper()
	l, ok := p.LookForLog(timeout, match)
	if !ok {
		t.Fatalf("no matching log from %s within %v\n%s", p.name, timeout, p.out.tail(80))
	}
	return l
}

// LookForLog 同 WaitLog，等不到时返回 false 而不是 t.Fatal：调用方自己说清没等到的是哪一条承诺
func (p *Process) LookForLog(timeout time.Duration, match func(Log) bool) (Log, bool) {
	deadline := time.After(timeout)
	for {
		changed := p.out.changed() // 先取 channel 再查，查完之后到的新行也叫得醒
		for _, l := range p.Logs() {
			if match(l) {
				return l, true
			}
		}
		select {
		case <-changed:
		case <-deadline:
			return Log{}, false
		}
	}
}

// FileLogs Options.LogFile 开着时 xlog 写进文件的日志
func (p *Process) FileLogs(t testing.TB) []Log {
	t.Helper()
	if p.logDir == "" {
		t.Fatal("FileLogs needs Options.LogFile")
	}
	// app.log 是指向当前文件的符号链接
	logs, err := ReadLogFile(filepath.Join(p.logDir, "app.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return logs
}

// Proc 读这个进程的 /proc：CPU 时间和内存
func (p *Process) Proc(t testing.TB) ProcStat {
	t.Helper()
	s, err := ReadProc(p.Pid())
	if err != nil {
		t.Fatalf("read /proc for %s: %v", p.name, err)
	}
	return s
}

// Request 发一个请求，不带 t，哪个协程里都能调。
//
// body 是 nil、[]byte、string 时原样发；其它类型按 JSON 编码，并设上
// Content-Type: application/json。header 是成对的 key、value。
// 客户端的超时是 30s，要更短用 ctx；要更长（/slow?ms=60000）自己建 http.Client 打 p.URL(...)
func (p *Process) Request(ctx context.Context, method, path string, body any, header ...string) (Response, error) {
	return do(ctx, p.client, method, p.URL(path), body, header...)
}

// Do 同 Request，失败时 t.Fatal
func (p *Process) Do(t testing.TB, method, path string, body any, header ...string) Response {
	t.Helper()
	r, err := p.Request(context.Background(), method, path, body, header...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// Get GET path
func (p *Process) Get(t testing.TB, path string, header ...string) Response {
	t.Helper()
	return p.Do(t, http.MethodGet, path, nil, header...)
}

// PostJSON 以 JSON POST v
func (p *Process) PostJSON(t testing.TB, path string, v any, header ...string) Response {
	t.Helper()
	return p.Do(t, http.MethodPost, path, v, header...)
}

// tableName 和 service/conf 的校验一致；订单表还要再加 7 个字符，PG 标识符上限 63
var tableName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,55}$`)

// childEnv 继承测试进程的环境，但去掉会改变服务行为的那几类：
// XONE_CONFIG / XONE_PROFILE 会换掉配置文件，E2E_* 是上一层留下的开关，
// OTEL_* 会改 service.name 和采样
func childEnv(vars map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "XONE_") || strings.HasPrefix(k, "E2E_") || strings.HasPrefix(k, "OTEL_") {
			continue
		}
		env = append(env, kv)
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+vars[k])
	}
	return env
}

func or[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

func writeFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ResetPeakRSS 见包级的 ResetPeakRSS：之后 Proc 读到的 PeakRSS 从此刻的 RSS 重新算起
func (p *Process) ResetPeakRSS(t testing.TB) {
	t.Helper()
	if err := ResetPeakRSS(p.Pid()); err != nil {
		t.Fatalf("reset peak RSS of %s: %v", p.name, err)
	}
}
