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

// Options are the settings for starting a process. The zero value works: PG / MySQL / Redis
// directly, a random port and a table name generated per test
type Options struct {
	// Env holds further environment variables, overriding the ones the harness sets under the same
	// name (the names are at the top of service/application.yml).
	// It is for changing an injected value alone, a deliberately wrong DSN for instance; use Overlay
	// to turn something on or off
	Env map[string]string

	// Overlay is YAML laid over the configuration file as a profile (--profile=e2e): maps merge
	// recursively and a list is replaced whole, the same rule as a profile in production. Every
	// configuration switch goes through it
	Overlay string

	// Config replaces the configuration file, e2e/service/application.yml by default. Start only
	Config string

	// Args are further command line arguments
	Args []string

	// PGAddr / MySQLAddr / RedisAddr are the addresses the service connects to, direct by default. A
	// fault test passes Proxy.Addr()
	PGAddr    string
	MySQLAddr string
	RedisAddr string

	// ClickHouse gives the service a third xgorm instance, ch: it activates the
	// service/application-ch.yml profile (with an Overlay it is --profile=ch,e2e, where the Overlay
	// wins). Start only.
	// CHAddr is the native address it connects to, direct by default; giving CHAddr means
	// ClickHouse: true.
	// To change the protocol or the password, give E2E_CH_DSN in Env
	ClickHouse bool
	CHAddr     string

	// Kafka gives the service an XKafka: it activates the service/application-kafka.yml profile
	// (after ch and before e2e). Start only. KafkaAddr is the broker address, direct by default;
	// giving KafkaAddr means Kafka: true.
	// With KafkaTopic non-empty the service consumes it (Service.KafkaTopic) in the consumer group
	// KafkaGroup (e2e_<random> when empty, see Process.KafkaGroup)
	Kafka      bool
	KafkaAddr  string
	KafkaTopic string
	KafkaGroup string

	// Downstream is the base URL /proxy calls, Stub.URL or "http://" + proxy.Addr() for instance
	Downstream string

	// Spans writes the spans into Process.SpanFile, to be read with Process.Spans / WaitSpan
	Spans bool

	// LogFile has xlog write to a file as well (in Process.Dir), to be read with Process.FileLogs
	LogFile bool

	// DiscardStdout points standard output at /dev/null instead of having the harness collect it:
	// under load the access log runs to tens of thousands of lines a second, and holding that in
	// memory both takes room and may slow the process under test down. stderr is still collected
	DiscardStdout bool

	// Table / KeyPrefix are generated with NewID by default and deleted when the test ends (the
	// tables of that name on PG and on MySQL alike).
	// Two processes that have to share data are given the same one explicitly
	Table     string
	KeyPrefix string

	// Port is FreePort by default
	Port int

	// NoWait returns without waiting for /ping to be ready: for testing a failing start, or a signal
	// during the start
	NoWait bool

	// ReadyTimeout is how long to wait for /ping, 30s by default
	ReadyTimeout time.Duration

	// NonJSON, when non-empty, stops the check that every output line of the process is JSON after it
	// exits (see checkOutput), and says why: "XLog.Format: text", for instance. A data race is still
	// looked for
	NonJSON string
}

// Process is one service process that is running
type Process struct {
	name string

	// Port is the port it listens on, and Base is http://127.0.0.1:Port
	Port int
	Base string
	// Table / KeyPrefix are the table name this process uses (the order table adds _orders; the user
	// table on MySQL and the event table on ClickHouse carry the same name) and its key prefix
	Table     string
	KeyPrefix string
	// CH says whether this process has a ClickHouse instance (Options.ClickHouse)
	CH bool
	// KafkaGroup is the consumer group this process consumes in (with Options.Kafka)
	KafkaGroup string
	// Dir is this process's temporary directory, deleted when the test ends
	Dir string
	// SpanFile is where the spans go with Options.Spans on
	SpanFile string

	logDir  string
	nonJSON string
	cmd     *exec.Cmd
	out     *output
	flush   []*lineWriter
	client  *http.Client
	started time.Time

	firstSignal atomic.Int64 // UnixNano, 0 meaning none was sent yet
	done        chan struct{}
	exit        Exit
}

// Exit is how the process exited
type Exit struct {
	// Code is the exit code. Killed by a signal it is -1, and Signal says which one
	Code   int
	Signal os.Signal
	// Uptime is from the start to the exit
	Uptime time.Duration
	// SinceSignal is from the first signal to the exit, and 0 when no signal was sent
	SinceSignal time.Duration
}

func (e Exit) String() string {
	if e.Signal != nil {
		return fmt.Sprintf("killed by %v after %v (%v since the first signal)", e.Signal, e.Uptime, e.SinceSignal)
	}
	return fmt.Sprintf("exit code %d after %v (%v since the first signal)", e.Code, e.Uptime, e.SinceSignal)
}

// Start starts the e2e service and waits until GET /ping answers 200 (with Options.NoWait it does
// not wait).
//
// A process still alive when the test ends is SIGKILLed; when the test fails the last 80 lines of
// output go into the test log
func Start(t testing.TB, o Options) *Process {
	t.Helper()
	bin := ServiceBinary(t)
	dir := t.TempDir()

	cfg := o.Config
	if cfg == "" {
		cfg = filepath.Join(ModuleDir(), "service", "application.yml")
	}
	// The child process's working directory is dir, so a relative path would point elsewhere over there
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
		// The profile file names follow from base, so base moves into the temporary directory as well
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

// StartBaseline starts the bare gin service to compare against. It reads no configuration file, so
// Overlay / Config / Spans / LogFile mean nothing to it
func StartBaseline(t testing.TB, o Options) *Process {
	t.Helper()
	if o.CHAddr != "" {
		o.ClickHouse = true
	}
	return launch(t, "baseline", BaselineBinary(t), o.Args, t.TempDir(), o)
}

// StartCovApp starts covapp, hands it the arguments as they are and waits for no readiness (it
// listens on nothing). Of Options only Env means anything to it
func StartCovApp(t testing.TB, o Options, args ...string) *Process {
	t.Helper()
	o.NoWait = true
	return launch(t, "covapp", CovAppBinary(t), args, t.TempDir(), o)
}

// StartArgs starts the e2e service with args as the command line as it is: unlike Start it adds no
// --config of its own.
// It is for testing XONE_CONFIG, the conventional paths and the other ways of finding the
// configuration when no --config was given. The working directory is Process.Dir
func StartArgs(t testing.TB, o Options, args ...string) *Process {
	t.Helper()
	return launch(t, "service", ServiceBinary(t), args, t.TempDir(), o)
}

// WaitReadyWith GETs url with c until it answers 200, and t.Fatals when none has within timeout or
// the process exits first.
// It is for a service Start's own http:// readiness probe cannot reach, HTTPS or h2c for instance
// (started with Options.NoWait)
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
	// The data cleanup is registered before the kill: Cleanup runs backwards, so the process dies first and its tables go after
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
		// The race detector sleeps 1s before the process exits by default (waiting for the other
		// goroutines to finish writing their reports), which would add that second to every case
		// measuring how long an exit takes
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
		defer devnull.Close() // the child process holds an fd of its own, so closing this one does not touch it
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
		// The bare gin service to compare against is not the framework, and these two checks are not about its output
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

// reap waits for the process to exit and records how it did
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
		e.Code = -1 // Wait itself failed (an I/O copy, say); do not let that look like a clean exit
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

// Pid is the process id
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// URL builds the full address: p.URL("/users/1")
func (p *Process) URL(path string) string { return p.Base + path }

// Signal sends one signal and records the moment of the first one in Exit.SinceSignal. On a process
// that has exited it does nothing
func (p *Process) Signal(sig os.Signal) {
	p.firstSignal.CompareAndSwap(0, time.Now().UnixNano())
	_ = p.cmd.Process.Signal(sig)
}

// Kill SIGKILL
func (p *Process) Kill() { _ = p.cmd.Process.Kill() }

// Exited says whether the process has exited
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Done is closed when the process exits
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait waits timeout at most and returns how it exited; the second value is false when it has not
// exited by then, and the process is left alone
func (p *Process) Wait(timeout time.Duration) (Exit, bool) {
	select {
	case <-p.done:
		return p.exit, true
	case <-time.After(timeout):
		return Exit{}, false
	}
}

// Terminate sends SIGTERM and waits for the exit, and t.Fatals when none comes within timeout
func (p *Process) Terminate(t testing.TB, timeout time.Duration) Exit {
	t.Helper()
	p.Signal(syscall.SIGTERM)
	e, ok := p.Wait(timeout)
	if !ok {
		t.Fatalf("%s did not exit within %v after SIGTERM\n%s", p.name, timeout, p.out.tail(80))
	}
	return e
}

// Stdout is standard output as written so far (empty with Options.DiscardStdout)
func (p *Process) Stdout() string { return p.out.text("stdout") }

// Stderr is standard error as written so far. The framework logs from before xlog was installed and
// the error MustRun returned are both here
func (p *Process) Stderr() string { return p.out.text("stderr") }

// Output is stdout and stderr together, in arrival order
func (p *Process) Output() string { return p.out.text("") }

// Logs are all the JSON log lines in stdout and stderr so far, in arrival order.
//
// Before xlog is installed the framework writes text to stderr in slog's default format
// (... INFO starting hook=...), which is not JSON and not here -- see Stderr; after xlog is closed
// the logs are JSON on stderr and are here
func (p *Process) Logs() []Log { return p.out.logs() }

// FindLogs are the lines of Logs matching match
func (p *Process) FindLogs(match func(Log) bool) []Log {
	var out []Log
	for _, l := range p.Logs() {
		if match(l) {
			out = append(out, l)
		}
	}
	return out
}

// WaitLog waits for a log line matching match to appear, and t.Fatals when none has within timeout
func (p *Process) WaitLog(t testing.TB, timeout time.Duration, match func(Log) bool) Log {
	t.Helper()
	l, ok := p.LookForLog(timeout, match)
	if !ok {
		t.Fatalf("no matching log from %s within %v\n%s", p.name, timeout, p.out.tail(80))
	}
	return l
}

// LookForLog is WaitLog that returns false instead of t.Fataling when nothing turns up: the caller
// then says for itself which promise went unkept
func (p *Process) LookForLog(timeout time.Duration, match func(Log) bool) (Log, bool) {
	deadline := time.After(timeout)
	for {
		changed := p.out.changed() // take the channel before looking, so that lines arriving mid-look still wake it
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

// FileLogs are the log lines xlog wrote to the file with Options.LogFile on
func (p *Process) FileLogs(t testing.TB) []Log {
	t.Helper()
	if p.logDir == "" {
		t.Fatal("FileLogs needs Options.LogFile")
	}
	// app.log is a symbolic link to the current file
	logs, err := ReadLogFile(filepath.Join(p.logDir, "app.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return logs
}

// Proc reads this process's /proc: the CPU time and the memory
func (p *Process) Proc(t testing.TB) ProcStat {
	t.Helper()
	s, err := ReadProc(p.Pid())
	if err != nil {
		t.Fatalf("read /proc for %s: %v", p.name, err)
	}
	return s
}

// Request sends one request, takes no t and can be called from any goroutine.
//
// A body that is nil, []byte or string is sent as it is; anything else is encoded as JSON and gets
// a Content-Type: application/json. header is key and value in pairs.
// The client's timeout is 30s; for a shorter one use ctx, and for a longer one (/slow?ms=60000)
// build an http.Client of your own against p.URL(...)
func (p *Process) Request(ctx context.Context, method, path string, body any, header ...string) (Response, error) {
	return do(ctx, p.client, method, p.URL(path), body, header...)
}

// Do is Request that t.Fatals on failure
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

// PostJSON POSTs v as JSON
func (p *Process) PostJSON(t testing.TB, path string, v any, header ...string) Response {
	t.Helper()
	return p.Do(t, http.MethodPost, path, v, header...)
}

// tableName matches the validation in service/conf; the order table adds 7 more characters, and a
// PG identifier stops at 63
var tableName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,55}$`)

// childEnv inherits the test process's environment but drops the kinds that would change how the
// service behaves: XONE_CONFIG / XONE_PROFILE would replace the configuration file, E2E_* are
// switches an outer layer left behind, and OTEL_* would change service.name and the sampling
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

// ResetPeakRSS: see the package-level ResetPeakRSS. The PeakRSS Proc reads afterwards starts over
// from the RSS at this moment
func (p *Process) ResetPeakRSS(t testing.TB) {
	t.Helper()
	if err := ResetPeakRSS(p.Pid()); err != nil {
		t.Fatalf("reset peak RSS of %s: %v", p.name, err)
	}
}
