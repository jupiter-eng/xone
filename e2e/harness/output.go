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

// Log is one JSON log line
type Log struct {
	// Stream says where it came from: stdout / stderr / file
	Stream string
	// Line is the line as it was written
	Line string
	// Fields is the JSON object it decodes to
	Fields map[string]any
}

// Get takes a field by a dotted path, "request_headers.Authorization" for instance.
// The whole name is looked up first and only then split on the dots, so a field name holding a dot
// is reachable too
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

// Str takes a field as a string, and returns an empty string when there is none. A number is
// rendered by fmt.Sprint (200 rather than 200.0)
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

// Msg is the log line's msg field
func (l Log) Msg() string { return l.Str("msg") }

// Level is the log line's level field, INFO for instance
func (l Log) Level() string { return l.Str("level") }

// parseLog returns false for a line that is not a JSON object
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

// ReadLogFile reads a file of JSON log lines, skipping the lines that are not JSON. It returns
// nothing when the file does not exist
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

// output collects one process's stdout / stderr and keeps them by line
type output struct {
	mu     sync.Mutex
	lines  []outLine
	parsed []Log // the JSON ones among lines, in arrival order; parsed as they come
	next   int   // how far into lines the parsing has got
	wake   chan struct{}
}

type outLine struct {
	stream  string
	text    string
	partial bool // the last chunk, with no newline at the end, when the process exited
}

func newOutput() *output { return &output{wake: make(chan struct{})} }

// writer is one stream's writing end. exec copies each stream in a goroutine of its own, so
// partial needs no lock
func (o *output) writer(stream string) *lineWriter { return &lineWriter{o: o, stream: stream} }

func (o *output) add(stream, text string, partial bool) {
	o.mu.Lock()
	o.lines = append(o.lines, outLine{stream, text, partial})
	close(o.wake)
	o.wake = make(chan struct{})
	o.mu.Unlock()
}

// changed is a channel that is closed when new lines arrive
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

// tail is the last n lines, both streams together in arrival order
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

// snapshot is a copy of every line
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

// flush takes in the last chunk, which has no newline at the end, after the process has exited
func (w *lineWriter) flush() {
	if len(w.partial) > 0 {
		w.o.add(w.stream, string(w.partial), true)
		w.partial = nil
	}
}

// The two checks made on a process's output when it exits (checkOutput):
//
//  1. no data race report in stderr. Outside the load tests the binaries are built with -race (see
//     RaceBuild), and the race detector writes WARNING: DATA RACE to stderr when it finds one while
//     the process carries on
//  2. every line of stdout / stderr is JSON. A caller's logging platform parses JSON line by line:
//     a third-party library that goes around slog and writes one line of plain text to standard
//     output or standard error is, on their side, one line of garbage that failed to parse or was
//     dropped outright. A unit test only sees the package under test, so this one takes a real
//     process to find
//
// Check 2 lets through these alone, all of them written by the framework on purpose:
//
//   - the text the framework writes to stderr in slog's default format before xlog is installed
//     (2006/01/02 15:04:05 INFO starting ...)
//   - the error at the end of stderr when it exits with 1: MustRun writes the error Run returned as
//     it is (xone ... failed, err=[...], and a YAML decoding error spans several lines); the same
//     goes for the log.Fatal of a service under test that fails to read its configuration before
//     xone.Run
//   - the Go runtime's panic / fatal error output, from its first line to the end (a case testing a
//     crash is there to read exactly that)
//   - the last unfinished chunk when it is killed by a signal
//
// The fallback log the framework writes to stderr after xlog is closed is JSON already and needs no
// exception. A case whose output is not JSON to begin with (XLog.Format: text, span export on
// stdout) gives its reason in Options.NonJSON

// raceReport is the first line of a race detector report; raceDelim is the line it wraps every
// report in
const (
	raceReport = "WARNING: DATA RACE"
	raceDelim  = "=================="
)

// preXLogLine is one line of slog's default logger from before xlog is installed: the log package's
// time prefix and a level
var preXLogLine = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? (DEBUG|INFO|WARN|ERROR) `)

// exitError is the first line of the error written to stderr before exiting: the xerror MustRun
// writes (xone ...), or log.Fatal's time prefix
var exitError = regexp.MustCompile(`^(xone |\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} )`)

// runtimeCrash is the first line of the Go runtime's crash output
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
	fatalFrom := len(lines) // when it exits with 1, stderr from this line on is the error written before exiting
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
			crashed = true // a runtime crash or a race report: stderr from this line on belongs to it
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

// String is the name and the process id, for the error messages
func (p *Process) String() string {
	if p.cmd == nil || p.cmd.Process == nil {
		return p.name
	}
	return fmt.Sprintf("%s (pid %d)", p.name, p.cmd.Process.Pid)
}
