package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covLogLine GET /probe/log 打一条指定级别的业务日志
func covLogLine(t *testing.T, p *harness.Process, level, msg string) {
	t.Helper()
	if r := p.Get(t, "/probe/log?level="+level+"&msg="+msg); r.Status != 200 {
		t.Fatalf("GET /probe/log：%v", r)
	}
}

// xlog/README.md XLog.File：
//
//	Name        实际文件是 app.log.<时间后缀>，另有同名符号链接指向当前文件
//	RotateTime  轮转周期，按本地时区对齐，至少 1m；后缀最细到分钟
//	MaxAge      启动时清一次、之后每次轮转清一次，只删 app.log.<时间后缀> 这种自己命名的文件，
//	            app.log.bak / app.log.1.gz 一律不碰
//	Perm        按八进制解析的字符串
//
// RotateTime 配到下限 1m，等一次真的跨分钟的轮转，这个用例最多要一分多钟
func TestCoverage_LogRotatesByRotateTime_MaxAgePrunesOwnFiles_PermApplied(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	dir := t.TempDir()
	old := []string{"app.log.20200101", "app.log.2020010100", "app.log.202001010000"} // 天、小时、分钟三种粒度，都早于 MaxAge
	foreign := []string{"app.log.bak", "app.log.1.gz", "app.log.20200101.gz"}         // 不是按我们的格式命名的，一律不碰
	for _, n := range append(append([]string{}, old...), foreign...) {
		covWrite(t, dir, n, "old\n")
		past := time.Now().Add(-30 * 24 * time.Hour)
		_ = os.Chtimes(filepath.Join(dir, n), past, past)
	}

	p := harness.Start(t, harness.Options{Overlay: fmt.Sprintf(`XLog:
  File:
    Enable: true
    Path: %q
    RotateTime: 1m
    MaxAge: 1h
    Perm: "0600"
`, dir)})
	started := time.Now()
	exists := func(n string) bool { _, err := os.Lstat(filepath.Join(dir, n)); return err == nil }

	for _, n := range old {
		if exists(n) {
			t.Errorf("MaxAge: 1h：启动时应清掉早于它的 %s", n)
		}
	}
	for _, n := range foreign {
		if !exists(n) {
			t.Errorf("%s 不是 app.log.<时间后缀>，不该被删", n)
		}
	}

	current := func() string {
		t.Helper()
		target, err := os.Readlink(filepath.Join(dir, "app.log"))
		if err != nil {
			t.Fatalf("app.log 应是指向当前文件的符号链接：%v", err)
		}
		return target
	}
	first := current()
	if want := "app.log." + started.Format("200601021504"); first != want && first != "app.log."+started.Add(-time.Minute).Format("200601021504") {
		t.Errorf("RotateTime: 1m 的文件名后缀应到分钟（%s），实际符号链接指向 %s", want, first)
	}
	fi, err := os.Stat(filepath.Join(dir, first))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("Perm: \"0600\" 时日志文件权限应是 0600，实际 %o", fi.Mode().Perm())
	}

	// 轮转之后还要再清一次：启动之后才出现的过期文件，等下一次轮转时被删
	covWrite(t, dir, "app.log.200001010000", "old\n")

	// 等到跨过分钟边界，再打一条日志触发轮转（轮转发生在写的时候）
	deadline := time.Now().Add(75 * time.Second)
	var second string
	for time.Now().Before(deadline) {
		covLogLine(t, p, "info", "rotate-probe")
		if second = current(); second != first {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if second == first {
		t.Fatalf("RotateTime: 1m：75s 里没有轮转，符号链接一直指向 %s", first)
	}
	rotatedAt := time.Now()
	if !strings.HasPrefix(second, "app.log.") || len(second) != len("app.log.200601021504") {
		t.Errorf("轮转后的文件名应是 app.log.<年月日时分>，实际 %s", second)
	}
	if !exists(first) {
		t.Errorf("轮转后上一个文件 %s 没到 MaxAge，应保留", first)
	}
	logs, err := harness.ReadLogFile(filepath.Join(dir, "app.log"))
	if err != nil || len(logs) == 0 {
		t.Errorf("轮转后符号链接指向的新文件里应有日志，实际 %d 条（%v）", len(logs), err)
	}
	// 轮转时的清理在后台跑，给它一点时间
	for i := 0; i < 100 && exists("app.log.200001010000"); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if exists("app.log.200001010000") {
		t.Errorf("MaxAge：每次轮转都该清一次，启动之后才放进去的过期文件轮转后还在")
	}
	for _, n := range foreign {
		if !exists(n) {
			t.Errorf("轮转时的清理也不该碰 %s", n)
		}
	}
	t.Logf("数字：%s → %s，启动后 %v 轮转（跨分钟边界）", first, second, rotatedAt.Sub(started).Round(time.Millisecond))

	t.Run("RotateTime 短于 1m 启动失败", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: fmt.Sprintf("XLog:\n  File:\n    Enable: true\n    Path: %q\n    RotateTime: 30s\n", t.TempDir())})
		faultMustContain(t, "RotateTime: 30s 的启动错误", stderr, "xlog", "RotateTime")
	})
	t.Run("默认权限 0644", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		harness.Start(t, harness.Options{Overlay: fmt.Sprintf("XLog:\n  File:\n    Enable: true\n    Path: %q\n", d)})
		target, err := os.Readlink(filepath.Join(d, "app.log"))
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(d, target))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("Perm 默认 0644，实际 %o", fi.Mode().Perm())
		}
		if len(target) != len("app.log.20060102") {
			t.Errorf("RotateTime 默认 24h，后缀应是年月日，实际 %s", target)
		}
	})
}

// xlog/README.md XLog：Level（debug / info / warn / error，默认 info）、Format（json / text，默认 json）、
// Timezone（IANA 时区名，配了却加载不到直接启动失败）、AddSource（是否记代码位置，默认关）
func TestCoverage_LogLevel_Format_Timezone_AddSource(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("Level: warn 滤掉 info，留下 warn / error", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: logLevel("warn")})
		covLogLine(t, p, "info", "cov-info")
		covLogLine(t, p, "warn", "cov-warn")
		covLogLine(t, p, "error", "cov-error")
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-error" })
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cov-warn" && l.Level() == "WARN" })) != 1 {
			t.Errorf("Level: warn 时 WARN 级别的日志应在")
		}
		if n := len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cov-info" || l.Msg() == "request completed" })); n != 0 {
			t.Errorf("Level: warn 时 INFO 级别的业务日志和访问日志都不该出现，实际 %d 条", n)
		}
	})

	t.Run("Level: debug 放出 debug", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: debugLogs})
		covLogLine(t, p, "debug", "cov-debug")
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-debug" && l.Level() == "DEBUG" })
	})

	t.Run("默认 info 滤掉 debug", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{})
		covLogLine(t, p, "debug", "cov-debug")
		covLogLine(t, p, "info", "cov-info")
		p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-info" })
		if len(p.FindLogs(func(l harness.Log) bool { return l.Msg() == "cov-debug" })) != 0 {
			t.Errorf("默认 info 时 debug 日志不该出现")
		}
	})

	t.Run("Format: text", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XLog:\n  Format: text\n", NonJSON: "XLog.Format: text"})
		covLogLine(t, p, "warn", "cov-text")
		deadline := time.Now().Add(waitFor)
		for !strings.Contains(p.Stdout(), "msg=cov-text") && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		out := p.Stdout()
		if !strings.Contains(out, "level=WARN msg=cov-text") {
			t.Errorf("Format: text 时标准输出应是 key=value 文本（level=WARN msg=cov-text），实际没找到：\n%s", lastLine(out))
		}
		for _, l := range p.Logs() {
			if l.Stream == "stdout" && l.Msg() != "" {
				t.Errorf("Format: text 时标准输出上不该有 JSON 日志，实际有 %s", l.Line)
				break
			}
		}
	})

	t.Run("Timezone: Asia/Shanghai", func(t *testing.T) {
		t.Parallel()
		p := harness.Start(t, harness.Options{Overlay: "XLog:\n  Timezone: Asia/Shanghai\n"})
		covLogLine(t, p, "info", "cov-tz")
		l := p.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-tz" })
		ts := l.Str("time")
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if !strings.HasSuffix(ts, "+08:00") || err != nil || time.Since(parsed).Abs() > time.Minute {
			t.Errorf("Timezone: Asia/Shanghai 时时间戳应按东八区渲染（+08:00）且是此刻，实际 %q（%v）", ts, err)
		}
		t.Logf("数字：Asia/Shanghai 的时间戳 %s", ts)
	})

	t.Run("Timezone 写错启动失败", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Overlay: "XLog:\n  Timezone: Mars/Olympus\n"})
		faultMustContain(t, "Timezone 写错的启动错误", stderr, "unknown Timezone=[Mars/Olympus]")
	})

	t.Run("AddSource", func(t *testing.T) {
		t.Parallel()
		on := harness.Start(t, harness.Options{Overlay: "XLog:\n  AddSource: true\n"})
		off := harness.Start(t, harness.Options{})
		for _, p := range []*harness.Process{on, off} {
			covLogLine(t, p, "info", "cov-source")
		}
		l := on.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-source" })
		if file := l.Str("source.file"); !strings.HasSuffix(file, "service/probe.go") || l.Str("source.line") == "" {
			t.Errorf("AddSource: true 时应记代码位置（service/probe.go 的某一行），实际 source=%v", l.Fields["source"])
		}
		l = off.WaitLog(t, waitFor, func(l harness.Log) bool { return l.Msg() == "cov-source" })
		if _, ok := l.Get("source"); ok {
			t.Errorf("AddSource 默认关，日志里不该有 source，实际 %s", l.Line)
		}
	})
}
