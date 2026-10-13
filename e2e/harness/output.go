package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Log 一行 JSON 日志
type Log struct {
	// Stream 从哪来：stdout / stderr / file
	Stream string
	// Line 原文
	Line string
	// Fields 解出来的 JSON 对象
	Fields map[string]any
}

// Get 按点分路径取字段，比如 "request_headers.Authorization"。
// 先按整个名字找，找不到才按点拆：字段名里本身带点的也取得到
func (l Log) Get(path string) (any, bool) {
	var cur any = l.Fields
	for path != "" {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok := m[path]; ok {
			return v, true
		}
		head, rest, found := strings.Cut(path, ".")
		if !found {
			return nil, false
		}
		if cur, ok = m[head]; !ok {
			return nil, false
		}
		path = rest
	}
	return cur, true
}

// Str 取字段并转成字符串，取不到时返回空串。数字按 fmt.Sprint 渲染（200 而不是 200.0）
func (l Log) Str(path string) string {
	v, ok := l.Get(path)
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Msg 日志的 msg 字段
func (l Log) Msg() string { return l.Str("msg") }

// Level 日志的 level 字段，比如 INFO
func (l Log) Level() string { return l.Str("level") }

// parseLog 不是 JSON 对象的行返回 false
func parseLog(stream, line string) (Log, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "{") {
		return Log{}, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return Log{}, false
	}
	return Log{Stream: stream, Line: line, Fields: m}, true
}

// ReadLogFile 读一个 JSON 行日志文件，非 JSON 的行跳过。文件不存在时返回空
func ReadLogFile(path string) ([]Log, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Log
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		if l, ok := parseLog("file", sc.Text()); ok {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// output 收一个进程的 stdout / stderr，按行存
type output struct {
	mu     sync.Mutex
	lines  []outLine
	parsed []Log // lines 里是 JSON 的那些，按到达顺序；增量解析
	next   int   // lines 里解析到哪了
	wake   chan struct{}
}

type outLine struct {
	stream  string
	text    string
	partial bool // 进程退出时还没有换行符结尾的最后一截
}

func newOutput() *output { return &output{wake: make(chan struct{})} }

// writer 一个流的写入端。exec 为每个流单开一个协程拷贝，所以 partial 不用加锁
func (o *output) writer(stream string) *lineWriter { return &lineWriter{o: o, stream: stream} }

func (o *output) add(stream, text string, partial bool) {
	o.mu.Lock()
	o.lines = append(o.lines, outLine{stream, text, partial})
	close(o.wake)
	o.wake = make(chan struct{})
	o.mu.Unlock()
}

// changed 有新行时关闭的 channel
func (o *output) changed() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.wake
}

func (o *output) text(stream string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var b strings.Builder
	for _, l := range o.lines {
		if stream == "" || l.stream == stream {
			b.WriteString(l.text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// tail 最后 n 行，两个流混在一起按到达顺序
func (o *output) tail(n int) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	start := max(0, len(o.lines)-n)
	var b strings.Builder
	for _, l := range o.lines[start:] {
		fmt.Fprintf(&b, "[%s] %s\n", l.stream, l.text)
	}
	return b.String()
}

// snapshot 全部行的一份拷贝
func (o *output) snapshot() []outLine {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]outLine(nil), o.lines...)
}

func (o *output) logs() []Log {
	o.mu.Lock()
	defer o.mu.Unlock()
	for ; o.next < len(o.lines); o.next++ {
		if l, ok := parseLog(o.lines[o.next].stream, o.lines[o.next].text); ok {
			o.parsed = append(o.parsed, l)
		}
	}
	return append([]Log(nil), o.parsed...)
}

type lineWriter struct {
	o       *output
	stream  string
	partial []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	n := len(b)
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			w.partial = append(w.partial, b...)
			return n, nil
		}
		line := string(append(w.partial, b[:i]...))
		w.partial = w.partial[:0]
		w.o.add(w.stream, line, false)
		b = b[i+1:]
	}
}

// flush 进程退出后把没有换行符结尾的最后一截也收进来
func (w *lineWriter) flush() {
	if len(w.partial) > 0 {
		w.o.add(w.stream, string(w.partial), true)
		w.partial = nil
	}
}

// 进程退出时对它的输出做的两条检查（checkOutput）：
//
//  1. stderr 里没有数据竞争报告。压测之外二进制都带 -race 编（见 RaceBuild），
//     竞争检测器发现竞争时往 stderr 写 WARNING: DATA RACE，进程照常跑下去
//  2. stdout / stderr 的每一行都是 JSON。使用者的日志平台按行解析 JSON：
//     哪个三方库绕开 slog 往标准输出、标准错误写了一行纯文本，那一行在他们那边就是一条
//     解析失败的垃圾，或者干脆丢了。单元测试只看得到被测包自己，这条只有真进程才查得出来
//
// 第 2 条放过的只有这几种，都是框架自己预期会写的：
//
//   - xlog 装好之前框架用 slog 的默认格式往 stderr 写的文本（2006/01/02 15:04:05 INFO starting ...）
//   - 以 1 退出时 stderr 末尾的那段错误：MustRun 把 Run 返回的错误原样写出去（xone ... failed, err=[...]，
//     YAML 的解码错误会跨好几行）；被测服务在 xone.Run 之前读配置失败时 log.Fatal 的那段同理
//   - Go 运行时的 panic / fatal error 输出，从头一行起到结束（测的就是崩溃的用例要看它）
//   - 被信号杀掉时没写完的最后一截
//
// xlog 关掉之后框架写 stderr 的兜底日志本来就是 JSON，不用放过。
// 输出本来就不是 JSON 的用例（XLog.Format: text、stdout 的 Span 导出）给 Options.NonJSON 写上理由

// raceReport 竞争检测器报告的开头；raceDelim 是它包在每份报告前后的分隔线
const (
	raceReport = "WARNING: DATA RACE"
	raceDelim  = "=================="
)

// preXLogLine xlog 装好之前 slog 默认 logger 的一行：log 包的时间前缀加级别
var preXLogLine = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? (DEBUG|INFO|WARN|ERROR) `)

// exitError 退出前写到 stderr 的那段错误的第一行：MustRun 写的 xerror（xone ...），或者 log.Fatal 带的时间前缀
var exitError = regexp.MustCompile(`^(xone |\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} )`)

// runtimeCrash Go 运行时崩溃输出的第一行
var runtimeCrash = regexp.MustCompile(`^(panic: |fatal error: |runtime: |SIG[A-Z]+: |goroutine \d+ \[)`)

func (p *Process) checkOutput(t testing.TB) {
	t.Helper()
	lines := p.out.snapshot()

	var races []string
	inRace := false
	for _, l := range lines {
		if l.stream != "stderr" {
			continue
		}
		if strings.Contains(l.text, raceReport) {
			inRace = true
		}
		if inRace {
			races = append(races, l.text)
			if len(races) > 200 {
				races = append(races, "...")
				break
			}
		}
	}
	if len(races) > 0 {
		t.Errorf("%s hit a data race (built with -race):\n%s", p, strings.Join(races, "\n"))
	}

	if p.nonJSON != "" {
		return
	}
	fatalFrom := len(lines) // 以 1 退出时，从这一行起 stderr 上的是退出前写的那段错误
	if p.exit.Code == 1 {
		for i := len(lines) - 1; i >= 0; i-- {
			l := lines[i]
			if l.stream != "stderr" {
				continue
			}
			if _, ok := parseLog(l.stream, l.text); ok {
				break
			}
			if exitError.MatchString(l.text) {
				fatalFrom = i
			}
		}
	}
	var bad []string
	crashed := false
	for i, l := range lines {
		if l.stream == "stderr" && (crashed || runtimeCrash.MatchString(l.text) || strings.Contains(l.text, raceReport) || l.text == raceDelim) {
			crashed = true // 运行时崩溃、竞争报告：从这一行起的 stderr 都是它的
			continue
		}
		if _, ok := parseLog(l.stream, l.text); ok {
			continue
		}
		switch {
		case l.stream == "stderr" && preXLogLine.MatchString(l.text):
		case l.stream == "stderr" && i >= fatalFrom:
		case l.partial && p.exit.Signal != nil:
		default:
			bad = append(bad, fmt.Sprintf("[%s] %s", l.stream, l.text))
		}
	}
	if len(bad) > 0 {
		t.Errorf("%s wrote %d non-JSON line(s): every line on stdout / stderr must be a JSON log "+
			"(a third-party library bypassing slog?); set Options.NonJSON with a reason if this process is meant to:\n%s",
			p, len(bad), strings.Join(bad, "\n"))
	}
}

// String 名字加进程号，给错误消息用
func (p *Process) String() string {
	if p.cmd == nil || p.cmd.Process == nil {
		return p.name
	}
	return fmt.Sprintf("%s (pid %d)", p.name, p.cmd.Process.Pid)
}
