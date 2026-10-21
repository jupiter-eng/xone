package harness

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// errRecorder records Errorf alone and is a testing.TB for the rest
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
		"all JSON":                            {Exit{}, []outLine{out("stdout", jsonLine), out("stderr", jsonLine)}},
		"text from before xlog was installed": {Exit{}, []outLine{out("stderr", "2026/09/24 14:58:50 INFO starting hook=xapp.loadConfig"), out("stdout", jsonLine)}},
		"the multi-line error MustRun writes when exiting with 1": {Exit{Code: 1}, []outLine{
			out("stdout", jsonLine),
			out("stderr", "xone start failed, err=[xgin.loadConfig: xconfig config failed, err=[invalid config XGin: yaml: unmarshal errors:"),
			out("stderr", "  /tmp/application.yml:2: field Prot not found in type xgin.Config]]"),
		}},
		"a multi-line error from log.Fatal": {Exit{Code: 1}, []outLine{
			out("stderr", "2026/09/24 14:58:49 xone xconfig config failed, err=[invalid config Service: yaml: unmarshal errors:"),
			out("stderr", "  /tmp/application.yml:2: field Tabel not found in type conf.Config]"),
		}},
		"a panic at run time": {Exit{Code: 2}, []outLine{
			out("stdout", jsonLine),
			out("stderr", "panic: boom"), out("stderr", ""), out("stderr", "goroutine 1 [running]:"), out("stderr", "main.main()"),
		}},
		"the unfinished last chunk of a process that was killed": {Exit{Code: -1, Signal: syscall.SIGKILL}, []outLine{
			out("stdout", jsonLine), {stream: "stdout", text: `{"time":"2026-09-24T0`, partial: true},
		}},
	}
	for name, c := range cases {
		if errs := checked(c.exit, "", c.lines...); len(errs) != 0 {
			t.Errorf("%s: nothing should be reported, got %v", name, errs)
		}
	}
}

func TestCheckOutput_ReportsThirdPartyPlainTextAndDataRaces(t *testing.T) {
	cases := map[string]struct {
		exit  Exit
		lines []outLine
		want  string
	}{
		"plain text on stdout":                                      {Exit{}, []outLine{out("stdout", jsonLine), out("stdout", "[GIN-debug] GET /ping")}, "[GIN-debug]"},
		"log package output without a level on stderr":              {Exit{}, []outLine{out("stdout", jsonLine), out("stderr", "2026/09/24 14:58:50 [mysql] packets.go:58 unexpected EOF")}, "[mysql]"},
		"error text on stderr while exiting with 0":                 {Exit{}, []outLine{out("stderr", "xone something failed")}, "xone something failed"},
		"plain text before the error while exiting with 1":          {Exit{Code: 1}, []outLine{out("stderr", "WARN RESTY Get"), out("stdout", jsonLine), out("stderr", "xone start failed")}, "WARN RESTY"},
		"an unfinished chunk from a process that exited on its own": {Exit{}, []outLine{{stream: "stdout", text: `{"time":`, partial: true}}, `{"time":`},
		"a data race": {Exit{Code: 66}, []outLine{out("stdout", jsonLine), out("stderr", "=================="), out("stderr", "WARNING: DATA RACE"), out("stderr", "Write at 0x00c by goroutine 7:")}, "data race"},
	}
	for name, c := range cases {
		errs := checked(c.exit, "", c.lines...)
		if len(errs) != 1 || !strings.Contains(errs[0], c.want) {
			t.Errorf("%s: one error holding %q should be reported, got %q", name, c.want, errs)
		}
	}
}

func TestCheckOutput_NonJSONOnlyDisablesJSONCheck(t *testing.T) {
	if errs := checked(Exit{}, "XLog.Format: text", out("stdout", "time=... level=INFO msg=ready")); len(errs) != 0 {
		t.Errorf("with NonJSON set, the lines are not checked for being JSON, got %v", errs)
	}
	errs := checked(Exit{Code: 66}, "XLog.Format: text", out("stderr", "WARNING: DATA RACE"))
	if len(errs) != 1 || !strings.Contains(errs[0], "data race") {
		t.Errorf("a data race is still looked for, got %v", errs)
	}
}
