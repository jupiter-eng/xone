package harness

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// errRecorder 只记下 Errorf，其余照 testing.TB
type errRecorder struct {
	testing.TB
	errs []string
}

func (r *errRecorder) Helper() {}
func (r *errRecorder) Errorf(format string, a ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, a...))
}

func checked(exit Exit, nonJSON string, lines ...outLine) []string {
	p := &Process{name: "service", nonJSON: nonJSON, exit: exit, out: newOutput()}
	p.out.lines = lines
	r := &errRecorder{}
	p.checkOutput(r)
	return r.errs
}

func out(stream, text string) outLine { return outLine{stream: stream, text: text} }

const jsonLine = `{"time":"2026-09-24T00:00:00Z","level":"INFO","msg":"ready"}`

func TestCheckOutput_AllowsExpectedFrameworkOutput(t *testing.T) {
	cases := map[string]struct {
		exit  Exit
		lines []outLine
	}{
		"全是 JSON":      {Exit{}, []outLine{out("stdout", jsonLine), out("stderr", jsonLine)}},
		"xlog 装好之前的文本": {Exit{}, []outLine{out("stderr", "2026/09/24 14:58:50 INFO starting hook=xapp.loadConfig"), out("stdout", jsonLine)}},
		"以 1 退出时 MustRun 写的多行错误": {Exit{Code: 1}, []outLine{
			out("stdout", jsonLine),
			out("stderr", "xone start failed, err=[xgin.loadConfig: xconfig config failed, err=[invalid config XGin: yaml: unmarshal errors:"),
			out("stderr", "  /tmp/application.yml:2: field Prot not found in type xgin.Config]]"),
		}},
		"log.Fatal 的多行错误": {Exit{Code: 1}, []outLine{
			out("stderr", "2026/09/24 14:58:49 xone xconfig config failed, err=[invalid config Service: yaml: unmarshal errors:"),
			out("stderr", "  /tmp/application.yml:2: field Tabel not found in type conf.Config]"),
		}},
		"运行时 panic": {Exit{Code: 2}, []outLine{
			out("stdout", jsonLine),
			out("stderr", "panic: boom"), out("stderr", ""), out("stderr", "goroutine 1 [running]:"), out("stderr", "main.main()"),
		}},
		"被杀掉时没写完的最后一截": {Exit{Code: -1, Signal: syscall.SIGKILL}, []outLine{
			out("stdout", jsonLine), {stream: "stdout", text: `{"time":"2026-09-24T0`, partial: true},
		}},
	}
	for name, c := range cases {
		if errs := checked(c.exit, "", c.lines...); len(errs) != 0 {
			t.Errorf("%s：不该报错，实际 %v", name, errs)
		}
	}
}

func TestCheckOutput_ReportsThirdPartyPlainTextAndDataRaces(t *testing.T) {
	cases := map[string]struct {
		exit  Exit
		lines []outLine
		want  string
	}{
		"stdout 上的纯文本":          {Exit{}, []outLine{out("stdout", jsonLine), out("stdout", "[GIN-debug] GET /ping")}, "[GIN-debug]"},
		"stderr 上不带级别的 log 包输出": {Exit{}, []outLine{out("stdout", jsonLine), out("stderr", "2026/09/24 14:58:50 [mysql] packets.go:58 unexpected EOF")}, "[mysql]"},
		"以 0 退出时 stderr 上的错误文本": {Exit{}, []outLine{out("stderr", "xone something failed")}, "xone something failed"},
		"以 1 退出时错误之前的纯文本":       {Exit{Code: 1}, []outLine{out("stderr", "WARN RESTY Get"), out("stdout", jsonLine), out("stderr", "xone start failed")}, "WARN RESTY"},
		"没写完的一截但进程是自己退出的":       {Exit{}, []outLine{{stream: "stdout", text: `{"time":`, partial: true}}, `{"time":`},
		"数据竞争": {Exit{Code: 66}, []outLine{out("stdout", jsonLine), out("stderr", "=================="), out("stderr", "WARNING: DATA RACE"), out("stderr", "Write at 0x00c by goroutine 7:")}, "data race"},
	}
	for name, c := range cases {
		errs := checked(c.exit, "", c.lines...)
		if len(errs) != 1 || !strings.Contains(errs[0], c.want) {
			t.Errorf("%s：该报一条带 %q 的错误，实际 %q", name, c.want, errs)
		}
	}
}

func TestCheckOutput_NonJSONOnlyDisablesJSONCheck(t *testing.T) {
	if errs := checked(Exit{}, "XLog.Format: text", out("stdout", "time=... level=INFO msg=ready")); len(errs) != 0 {
		t.Errorf("写了 NonJSON 就不查每一行是不是 JSON，实际 %v", errs)
	}
	errs := checked(Exit{Code: 66}, "XLog.Format: text", out("stderr", "WARNING: DATA RACE"))
	if len(errs) != 1 || !strings.Contains(errs[0], "data race") {
		t.Errorf("数据竞争照查，实际 %v", errs)
	}
}
