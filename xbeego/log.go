package xbeego

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/beego/beego/v2/core/logs"
)

// adapterName is what beego's logs package knows this adapter by. It is registered once per
// process, see bindOnce -- logs.Register panics on a repeated name.
const adapterName = "xone"

// registerBeeLogAdapter makes the adapter available under adapterName.
func registerBeeLogAdapter() {
	logs.Register(adapterName, func() logs.Logger { return beeLog{} })
}

// useBeeLogAdapter makes it the only place beego's logs go.
//
// Reset first, then SetLogger: beego's logger attaches a console adapter of its own the first time
// anything is written to it (writeMsg in core/logs/log.go), and an application may have set one up
// as well. Without the Reset, every line would be written twice -- once as JSON through slog and
// once as beego's own text on stderr. Reset leaves the logger initialized, so no console adapter
// comes back afterwards.
//
// This replaces whatever beego logging the application configured. XBeego.BeegoLogs turns it off
// for an application that wants to keep its own.
func useBeeLogAdapter() {
	logs.Reset()
	if err := logs.SetLogger(adapterName); err != nil {
		slog.Warn("xbeego failed to route beego's own logs into slog, beego keeps its own logger", "error", err)
	}
}

// beeLog is a beego log adapter writing through slog.
//
// Beego's logs package has no ctx to carry a trace, so these lines have no trace_id: they are the
// framework's own (a payload too large, an fcgi failure, a template error), not a handler's. A
// handler should write through slog with the request's ctx, which does carry one.
type beeLog struct{}

// Init takes the adapter's JSON configuration; there is none, and the level is slog's.
//
// Beego filters by its own level before reaching an adapter, and its default passes everything
// (LevelDebug in NewLogger), so what is written is decided by XLog.Level alone -- one level for the
// whole process, as it is for every other integration.
func (beeLog) Init(string) error { return nil }

// WriteMsg writes one line. The message field holds beego's text, as xecho's echo internal log does
// for gommon's, so a query across the integrations finds both.
//
// Formatting is the adapter's job: beego hands the format string over in Msg with the arguments
// beside it in Args, and only an adapter's own formatter puts the two together
// (LogMsg.OldStyleFormat in core/logs/log_msg.go). Writing Msg out as it is would log
// "cannot reach %s".
func (beeLog) WriteMsg(lm *logs.LogMsg) error {
	msg := lm.Msg
	if len(lm.Args) > 0 {
		msg = fmt.Sprintf(lm.Msg, lm.Args...)
	}

	attrs := make([]slog.Attr, 0, 3)
	attrs = append(attrs, slog.String("message", msg))
	// The prefix is beego's own (logs.SetPrefix), empty unless the application set one.
	if lm.Prefix != "" {
		attrs = append(attrs, slog.String("prefix", lm.Prefix))
	}
	// Beego fills the call site in for every line (runtime.Caller in core/logs/log.go), and only
	// shows it when logs.SetLogFuncCall is on. It is recorded here either way: the whole point of
	// routing these into slog is to be able to find where a line came from.
	if lm.FilePath != "" {
		attrs = append(attrs, slog.String("source", lm.FilePath+":"+strconv.Itoa(lm.LineNumber)))
	}
	slog.LogAttrs(context.Background(), beeLevel(lm.Level), "beego internal log", attrs...)
	return nil
}

func (beeLog) Destroy()                       {}
func (beeLog) Flush()                         {}
func (beeLog) SetFormatter(logs.LogFormatter) {} // the format is slog's handler's business

// beeLevel maps beego's eight levels onto slog's four.
//
// Emergency, Alert and Critical are recorded as ERROR: slog has nothing above it, and a logging
// platform's alerting goes by ERROR anyway. Notice is recorded as INFO, which is what it is --
// beego uses it for nothing a human has to act on.
func beeLevel(l int) slog.Level {
	switch l {
	case logs.LevelEmergency, logs.LevelAlert, logs.LevelCritical, logs.LevelError:
		return slog.LevelError
	case logs.LevelWarning:
		return slog.LevelWarn
	case logs.LevelDebug:
		return slog.LevelDebug
	}
	return slog.LevelInfo // Notice and Informational
}
