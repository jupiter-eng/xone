package xlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/internal/logext"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xonetest"
)

// fileCfg 造一份只写文件的配置，返回配置和日志文件路径
func fileCfg(t *testing.T) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	c := DefaultConfig()
	c.Console = false
	c.File = FileConfig{Enable: true, Path: dir, Name: "app.log",
		RotateTime: 24 * time.Hour, MaxAge: 7 * 24 * time.Hour, Perm: "0644"}
	return c, filepath.Join(dir, "app.log")
}

// readLines 读日志文件里的每一行 JSON
func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志文件失败: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志不是合法 JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestNew_LevelParsing(t *testing.T) {
	for _, s := range []string{"debug", "info", "warn", "warning", "error", "INFO", " info ", ""} {
		c := DefaultConfig()
		c.Console = false
		c.Level = s
		if _, _, err := New(c); err != nil {
			t.Errorf("级别 %q 应被接受，got=%v", s, err)
		}
	}
}

func TestNew_FailsFastOnLevelTypo(t *testing.T) {
	c := DefaultConfig()
	c.Level = "verbose"
	_, _, err := New(c)
	if err == nil {
		t.Fatal("不认识的级别应该报错，而不是悄悄退回 info")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("错误里应回显写错的值，got=%v", err)
	}
	assertConfigOp(t, err)
}

func TestNew_FailsFastOnFormatTypo(t *testing.T) {
	c := DefaultConfig()
	c.Format = "xml"
	_, _, err := New(c)
	if err == nil {
		t.Fatal("不认识的格式应该报错")
	}
	assertConfigOp(t, err)
}

// assertConfigOp 配置写错报的是 config，不是 new：告警和排错按 op 分得清是配置还是运行环境
func assertConfigOp(t *testing.T, err error) {
	t.Helper()
	var xe *xerror.Error
	if !errors.As(err, &xe) || xe.Module != "xlog" || xe.Op != "config" {
		t.Errorf("配置写错该报 xlog config，got=%v", err)
	}
}

func TestNew_LeavesNoOpenLogFileOnFailure(t *testing.T) {
	// 格式校验曾经排在打开文件之后：New 返回错误，可日志文件已经建好、
	// fd 也开着，而调用方手上没有 Closer 可关 —— 那个 fd 和它的符号链接
	// 就一直留在那里。配置项应当全部校验完再动文件。
	c, _ := fileCfg(t)
	c.Format = "xml"
	if _, _, err := New(c); err == nil {
		t.Fatal("不认识的格式应该报错")
	}

	entries, err := os.ReadDir(c.File.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("New 失败不该留下任何文件，got=%v", names)
	}
}

func TestNew_FailsFastOnRotationPeriodUnderOneMinute(t *testing.T) {
	// 文件名的时间后缀最细到分钟。RotateTime 写成 0s 时 truncate 原样返回，
	// 结果是每分钟一个文件；写成 30s 时两个周期落在同一个文件名上，
	// 实际还是每分钟轮转——两种都是「配了 A、跑的是 B」，而且一声不吭
	for _, d := range []time.Duration{0, 30 * time.Second, -time.Hour} {
		c, _ := fileCfg(t)
		c.File.RotateTime = d
		_, _, err := New(c)
		if err == nil || !strings.Contains(err.Error(), "RotateTime") {
			t.Errorf("RotateTime=%v 该报错并点名字段，got=%v", d, err)
		}
	}
	c, _ := fileCfg(t)
	c.File.RotateTime = time.Minute
	if _, cl, err := New(c); err != nil {
		t.Errorf("一分钟是最细的合法粒度，不该报错：%v", err)
	} else {
		cl.Close()
	}
}

func TestNew_MaxAgeNegativeFailsFast(t *testing.T) {
	// 0 表示不清理；负数多半是写错了，当成「不清理」会让磁盘慢慢被写满
	c, _ := fileCfg(t)
	c.File.MaxAge = -time.Hour
	if _, _, err := New(c); err == nil || !strings.Contains(err.Error(), "MaxAge") {
		t.Errorf("MaxAge 为负该报错并点名字段，got=%v", err)
	}
}

func TestParsePerm_OctalNotations(t *testing.T) {
	// 实测 yaml.v3：Perm: 0644 不加引号进字符串字段也还是 "0644"；
	// YAML 1.2 的 0o644 写法同样该认
	for _, s := range []string{"0644", "644", "0o644"} {
		if got, err := parsePerm(s); err != nil || got != 0o644 {
			t.Errorf("parsePerm(%q)=%o err=%v，want 644", s, got, err)
		}
	}
}

func TestNew_FailsFastOnPermTypo(t *testing.T) {
	c, _ := fileCfg(t)
	c.File.Perm = "rw-r--r--"
	_, _, err := New(c)
	if err == nil {
		t.Fatal("权限格式不对应该报错")
	}
	assertConfigOp(t, err)
}

func TestNew_WorksWithAllOutputsDisabled(t *testing.T) {
	c := DefaultConfig()
	c.Console = false
	c.File.Enable = false

	l, closer, err := New(c)
	if err != nil {
		t.Fatalf("「我就是不要日志」是合理选择，不该让服务起不来: %v", err)
	}
	l.Info("这条会被丢弃")
	if err := closer.Close(); err != nil {
		t.Errorf("Close 不该出错: %v", err)
	}
}

func TestNew_CloserIsNeverNil(t *testing.T) {
	c := DefaultConfig()
	c.Console = true
	c.File.Enable = false
	_, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if closer == nil {
		t.Fatal("即便没有文件输出，Closer 也不该是 nil —— 调用方不必判空")
	}
	_ = closer.Close()
}

func TestNew_WritesFileAndFiltersByLevel(t *testing.T) {
	c, path := fileCfg(t)
	c.Level = "warn"

	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("debug 不该出现")
	l.Info("info 不该出现")
	l.Warn("warn 应该出现")
	l.Error("error 应该出现")
	closer.Close()

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("级别 warn 应只放过 2 条，got=%d: %v", len(lines), lines)
	}
	if lines[0]["level"] != "WARN" || lines[1]["level"] != "ERROR" {
		t.Errorf("放过的应是 WARN 和 ERROR，got=%v", lines)
	}
}

func TestNew_TextFormat(t *testing.T) {
	c, path := fileCfg(t)
	c.Format = FormatText

	l, closer, _ := New(c)
	l.Info("你好", "k", "v")
	closer.Close()

	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "k=v") {
		t.Errorf("text 格式应输出 k=v，got=%q", string(b))
	}
}

// ---- ctx 作用域 ----

func TestScope_FieldsAppearInLog(t *testing.T) {
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "user_id", 42)
	AddKVs(ctx, map[string]any{"req_id": "abc", "path": "/api"})
	l.InfoContext(ctx, "处理完成")
	closer.Close()

	got := readLines(t, path)[0]
	if got["user_id"] != float64(42) || got["req_id"] != "abc" || got["path"] != "/api" {
		t.Errorf("作用域里的字段都应出现在日志里，got=%v", got)
	}
}

func TestScope_DeepCallWritesVisibleToOwner(t *testing.T) {
	// 这是「存指针而不是存值」的意义：业务函数在调用栈深处拿不到 *gin.Context，
	// 没机会把新 context 回传，但它写的字段必须能被入口处的访问日志看到
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	ctx := CtxWithScope(context.Background())
	func(ctx context.Context) { // 深层业务函数，只拿到 ctx
		AddKV(ctx, "从深处写的", true)
	}(ctx)
	l.InfoContext(ctx, "入口处的访问日志") // 入口处用的还是原来那个 ctx
	closer.Close()

	if got := readLines(t, path)[0]; got["从深处写的"] != true {
		t.Errorf("深层写入应对持有同一 ctx 的地方可见，got=%v", got)
	}
}

func TestScope_ReopeningKeepsExistingFields(t *testing.T) {
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "a", 1)

	ctx2 := CtxWithScope(ctx) // 比如中间件被注册了两次
	AddKV(ctx2, "b", 2)

	n := 0
	scopeFrom(ctx).Each(func(k string, v any) { n++ })
	if n != 2 {
		t.Errorf("重复开启应幂等，两个字段都在，got=%d", n)
	}
}

func TestScope_DropsAndCountsWithoutScope(t *testing.T) {
	before := DroppedKVCount()
	AddKV(context.Background(), "k", "v")
	AddKVs(context.Background(), map[string]any{"a": 1, "b": 2})

	if got := DroppedKVCount() - before; got != 3 {
		t.Errorf("没有作用域的写入应被计数，便于排查「字段没出现在日志里」，got=%d want=3", got)
	}
}

func TestScope_nil_CtxDoesNotCrash(t *testing.T) {
	//lint:ignore SA1012 故意传 nil 验证不 panic
	AddKV(nil, "k", "v")
	if ctx := CtxWithScope(nil); ctx == nil {
		t.Error("CtxWithScope(nil) 应返回一个可用的 context")
	}
}

func TestCtxWithKV_AffectsOnlyDerivedCtx(t *testing.T) {
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	req := CtxWithScope(context.Background())
	AddKV(req, "req_id", "r1")
	a := CtxWithKV(req, map[string]any{"order_id": 1})
	b := CtxWithKV(req, map[string]any{"order_id": 2})
	AddKV(a, "only_a", true) // 写进 a 自己，不回流到 req、不串到 b

	l.InfoContext(a, "a")
	l.InfoContext(b, "b")
	l.InfoContext(req, "req")
	closer.Close()

	got := readLines(t, path)
	if got[0]["req_id"] != "r1" || got[0]["order_id"] != float64(1) || got[0]["only_a"] != true {
		t.Errorf("a 该带着父字段、自己的 order_id 和后写的字段，got=%v", got[0])
	}
	if got[1]["req_id"] != "r1" || got[1]["order_id"] != float64(2) || got[1]["only_a"] != nil {
		t.Errorf("b 只带父字段和自己的 order_id，got=%v", got[1])
	}
	if got[2]["order_id"] != nil || got[2]["only_a"] != nil {
		t.Errorf("父 ctx 不该被派生出来的字段污染，got=%v", got[2])
	}
}

func TestCtxWithKV_PassedValueWinsOnSameKey_WorksWithoutScope(t *testing.T) {
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	parent := CtxWithScope(context.Background())
	AddKV(parent, "k", "old")
	l.InfoContext(CtxWithKV(parent, map[string]any{"k": "new"}), "覆盖")
	l.InfoContext(CtxWithKV(context.Background(), map[string]any{"k": "bare"}), "没有作用域")
	closer.Close()

	got := readLines(t, path)
	if got[0]["k"] != "new" {
		t.Errorf("同名 key 该以传入的为准，got=%v", got[0])
	}
	if got[1]["k"] != "bare" {
		t.Errorf("父 ctx 没有作用域时也该带上字段，got=%v", got[1])
	}
	var kept any
	scopeFrom(parent).Each(func(_ string, v any) { kept = v })
	if kept != "old" {
		t.Errorf("父 ctx 的值不该被改，got=%v", kept)
	}
}

// ---- trace 提取器 ----

func TestTraceExtractor(t *testing.T) {
	t.Cleanup(func() { SetTraceExtractor(nil) })

	c, path := fileCfg(t)
	l, closer, _ := New(c)

	SetTraceExtractor(func(ctx context.Context) (string, string) {
		return "trace-1", "span-1"
	})
	l.InfoContext(context.Background(), "带链路的日志")
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "trace-1" || got["span_id"] != "span-1" {
		t.Errorf("注入的链路标识应出现在日志里，got=%v", got)
	}
}

func TestTraceExtractor_NoFieldsWhenNotInjected(t *testing.T) {
	SetTraceExtractor(nil)

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.InfoContext(context.Background(), "没有链路")
	closer.Close()

	if got := readLines(t, path)[0]; got["trace_id"] != nil {
		t.Errorf("未注入提取器时不该有 trace 字段，got=%v", got)
	}
}

func TestTraceExtractor_NoFieldsForEmptyTraceID(t *testing.T) {
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.InfoContext(context.Background(), "链路未采样")
	closer.Close()

	if got := readLines(t, path)[0]; got["trace_id"] != nil {
		t.Errorf("提取不到时不该写空字段，got=%v", got)
	}
}

func TestHandler_GroupKeepsCtxFields(t *testing.T) {
	// slog 的 With 走 WithAttrs、WithGroup 会给后续属性加前缀。
	// 用户划的分组是给业务字段用的，trace_id 是整条记录的身份，必须留在顶层。
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "s1" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "uid", "u9")
	l.With("固定字段", "x").WithGroup("g").InfoContext(ctx, "msg", "业务字段", "y")
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "t1" || got["span_id"] != "s1" {
		t.Errorf("链路字段应在顶层，got=%v", got)
	}
	if got["uid"] != "u9" {
		t.Errorf("scope 字段应在顶层，got=%v", got)
	}
	if got["固定字段"] != "x" {
		t.Errorf("With 挂的属性在分组之前，应留在顶层，got=%v", got)
	}
	g, ok := got["g"].(map[string]any)
	if !ok || g["业务字段"] != "y" {
		t.Errorf("分组之后的业务字段才该落进分组，got=%v", got)
	}
	if _, dup := g["trace_id"]; dup {
		t.Errorf("链路字段不该同时出现在分组里，got=%v", got)
	}
}

func TestHandler_NestedGroups(t *testing.T) {
	// 重放调用链要保持原顺序，否则嵌套分组会串位
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.WithGroup("a").With("内层", 1).WithGroup("b").InfoContext(context.Background(), "msg", "叶子", 2)
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "t1" {
		t.Errorf("链路字段应在顶层，got=%v", got)
	}
	a, ok := got["a"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 a 分组，got=%v", got)
	}
	if a["内层"] != float64(1) {
		t.Errorf("a.内层 应为 1，got=%v", got)
	}
	b, ok := a["b"].(map[string]any)
	if !ok || b["叶子"] != float64(2) {
		t.Errorf("a.b.叶子 应为 2，got=%v", got)
	}
}

func TestHandler_FastPathWithoutGroup(t *testing.T) {
	// 没开过分组时属性本来就在顶层，不该退化成每条记录重建 handler
	h := newCtxHandler(slog.NewJSONHandler(io.Discard, nil))
	w := h.WithAttrs([]slog.Attr{slog.String("a", "1")}).(*ctxHandler)
	if w.grouped {
		t.Error("只调用 WithAttrs 不该触发重放")
	}
	if same := h.WithAttrs(nil); same != slog.Handler(h) {
		t.Error("空属性应原样返回")
	}
	if same := h.WithGroup(""); same != slog.Handler(h) {
		t.Error("空分组名应原样返回")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Level != "info" || c.Format != FormatJSON {
		t.Errorf("默认应为 info + json，got=%+v", c)
	}
	if !c.Console {
		t.Error("控制台默认应开启")
	}
	if c.File.Enable {
		t.Error("文件输出默认应关闭")
	}
	if c.File.RotateTime != 24*time.Hour || c.File.MaxAge != 7*24*time.Hour {
		t.Errorf("轮转默认应为一天、保留七天，got=%+v", c.File)
	}
}

func TestHandler_SiblingDerivationsIndependent(t *testing.T) {
	// 两个分支从同一个父 logger 派生，调用链不能共享底层数组，
	// 否则后派生的会把先派生的最后一节覆盖掉
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	parent := l.With("共同", "p")
	a := parent.WithGroup("a")
	b := parent.WithGroup("b")
	a.InfoContext(context.Background(), "msg", "x", 1)
	b.InfoContext(context.Background(), "msg", "x", 2)
	closer.Close()

	lines := readLines(t, path)
	for i, want := range []string{"a", "b"} {
		got := lines[i]
		if got["共同"] != "p" || got["trace_id"] != "t1" {
			t.Errorf("第 %d 行顶层字段不对，got=%v", i, got)
		}
		g, ok := got[want].(map[string]any)
		if !ok || g["x"] != float64(i+1) {
			t.Errorf("第 %d 行应落进 %s 分组，got=%v", i, want, got)
		}
	}
}

func TestRegister_RegistrationMatchesFramework(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记、档位挂错，
	// 表现是「配置不生效」或者「比用它的东西晚就绪」，别处都测不出来
	var got *hook.Entry
	for _, e := range hook.Start() {
		if e.Pkg == "github.com/jupiter-eng/xone/xlog" {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatal("没有登记启动钩子")
	}
	if got.Stage != hook.StageLog {
		t.Errorf("日志必须最先起最后关，否则其余组件的启停日志会丢，got=%v", got.Stage)
	}

	var stopped bool
	for _, e := range hook.Stop() {
		if e.Pkg == "github.com/jupiter-eng/xone/xlog" {
			stopped = true
		}
	}
	if stopped != true {
		t.Errorf("停止钩子登记情况不对，got=%v", stopped)
	}
}

func TestTraceIDs(t *testing.T) {
	// 给不想依赖 OpenTelemetry、又需要链路标识的包用（xmetric 拿它做 exemplar）
	t.Cleanup(func() { SetTraceExtractor(nil) })

	if id, sp := TraceIDs(context.Background()); id != "" || sp != "" {
		t.Errorf("没注入提取器时应返回空，got=%q %q", id, sp)
	}
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "s1" })
	if id, sp := TraceIDs(context.Background()); id != "t1" || sp != "s1" {
		t.Errorf("应返回提取器给的值，got=%q %q", id, sp)
	}
	if id, sp := TraceIDs(nil); id != "" || sp != "" { //nolint:staticcheck // 故意传 nil
		t.Errorf("nil ctx 不该 panic，got=%q %q", id, sp)
	}
}

func TestAddObserver(t *testing.T) {
	// xmetric 靠它统计错误日志，而不必反过来让 xlog 认识 Prometheus
	t.Cleanup(logext.ResetObservers())

	var seen []string
	AddObserver(func(_ context.Context, r slog.Record) { seen = append(seen, r.Level.String()+":"+r.Message) })
	AddObserver(nil) // 忽略，不该炸

	c, _ := fileCfg(t)
	c.Level = "warn"
	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("被级别挡掉")
	l.Warn("警告")
	l.Error("错误")
	closer.Close()

	want := []string{"WARN:警告", "ERROR:错误"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("观察者只该收到实际写出的日志，got=%v want=%v", seen, want)
	}
}

func TestAddObserver_PanicDoesNotInterruptLogging(t *testing.T) {
	t.Cleanup(logext.ResetObservers())

	AddObserver(func(context.Context, slog.Record) { panic("炸了") })
	var reached bool
	AddObserver(func(context.Context, slog.Record) { reached = true })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.Error("出事了")
	closer.Close()

	if !reached {
		t.Error("一个观察者 panic 不该影响后面的观察者")
	}
	if got := readLines(t, path)[0]; got["msg"] != "出事了" {
		t.Errorf("观察者 panic 了，日志本身还得写出去，got=%v", got)
	}
}

func TestNew_RendersTimeInConfiguredTimezone(t *testing.T) {
	c := DefaultConfig()
	c.Console, c.Format, c.Timezone = false, FormatJSON, "Asia/Shanghai"

	dir := t.TempDir()
	c.File.Enable, c.File.Path, c.File.Name = true, dir, "tz"

	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	line := readFile(t, filepath.Join(dir, "tz"))
	var rec struct {
		Time string `json:"time"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("解不出日志：%v，原文=%s", err, line)
	}
	got, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil {
		t.Fatal(err)
	}
	if _, offset := got.Zone(); offset != 8*3600 {
		t.Errorf("该按 Asia/Shanghai 渲染（+08:00），got=%s offset=%d", rec.Time, offset)
	}
}

func TestNew_FailsWhenTimezoneCannotLoad(t *testing.T) {
	// 悄悄退回本地时区意味着你以为在看东八区的时间、实际是 UTC，
	// 差八小时而毫无提示。scratch 镜像里没有时区库正是这个场景
	c := DefaultConfig()
	c.Console, c.Timezone = false, "Nowhere/Nothing"

	_, _, err := New(c)
	if err == nil {
		t.Fatal("时区加载不到该报错")
	}
	if !strings.Contains(err.Error(), "time/tzdata") {
		t.Errorf("错误里该告诉人怎么修，got=%v", err)
	}
}

func TestLocation_LocalTimezoneWhenUnset(t *testing.T) {
	location.Store(nil)
	if got := Location(); got != time.Local {
		t.Errorf("没配 Timezone 时该返回 time.Local，got=%v", got)
	}

	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("本机没有时区库")
	}
	location.Store(sh)
	t.Cleanup(func() { location.Store(nil) })
	if got := Location(); got != sh {
		t.Errorf("配了之后该返回配置的时区，got=%v", got)
	}
}

// keepGlobals 记下会被 install 改掉的那些全局值，测试结束还原
func keepGlobals(t *testing.T) {
	t.Helper()
	oldLogger, oldCloser, oldLoc := slog.Default(), closer, location.Load()
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
		closer = oldCloser
		custom.Store(nil)
		installed.Store(false)
		location.Store(oldLoc)
	})
}

func TestInitXLog_InstallsUsableDefaultWhenUnconfigured(t *testing.T) {
	// 日志是唯一一个「没配也必须有」的东西：配置本身出问题时，
	// 使用者要能看见那条错误
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	if err := initXLog(context.Background()); err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	if slog.Default() == nil {
		t.Fatal("没装上全局 logger")
	}
}

func TestInitXLog_ConfigTypoFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XLog:\n  Lvl: debug\n")

	if err := initXLog(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
}

func TestInitXLog_InvalidValueFailsStartup(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XLog:\n  Level: verbose\n")

	if err := initXLog(context.Background()); err == nil {
		t.Fatal("认不出的级别应当让启动失败，而不是悄悄退回某个默认级别")
	}
}

func TestInitXLog_ConfigAppliedToGlobalLogger(t *testing.T) {
	keepGlobals(t)
	dir := t.TempDir()
	xonetest.UseConfigYAML(t, "XLog:\n  Level: error\n  Console: false\n  File:\n    Enable: true\n    Path: \""+dir+"\"\n    Name: app.log\n")

	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	slog.Info("这条不该写出去")
	slog.Error("这条该写出去")

	if err := closeXLog(context.Background()); err != nil {
		t.Fatalf("关闭不该报错：%v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatalf("没按配置里的路径写文件：%v", err)
	}
	if strings.Contains(string(b), "这条不该写出去") {
		t.Error("配置里的 Level 没生效")
	}
	if !strings.Contains(string(b), "这条该写出去") {
		t.Error("该写出去的没写出去")
	}
}

func TestInitXLog_TimezoneAppliedGlobally(t *testing.T) {
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XLog:\n  Timezone: Asia/Tokyo\n")

	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := location.Load(); got == nil || got.String() != "Asia/Tokyo" {
		t.Errorf("时区没装上，got=%v", got)
	}
}

func TestInitXLog_TimezoneClearedWhenLaterRunLeavesItUnset(t *testing.T) {
	// 同一个进程里再跑一次 Run（测试里常见）：从前 location 只在配了时区时才写，
	// 上一次的时区一直留着，xlog.Location() 和这一次的日志时间戳对不上
	keepGlobals(t)
	if _, err := time.LoadLocation("Asia/Tokyo"); err != nil {
		t.Skip("本机没有时区库")
	}
	xonetest.UseConfigYAML(t, "XLog:\n  Timezone: Asia/Tokyo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	xonetest.UseConfigYAML(t, "XLog:\n  Level: info\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := Location(); got != time.Local {
		t.Errorf("这一次没配 Timezone，该回到 time.Local，got=%v", got)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	// UseHandler 那一支同样
	xonetest.UseConfigYAML(t, "XLog:\n  Timezone: Asia/Tokyo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	UseHandler(slog.NewTextHandler(io.Discard, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := Location(); got != time.Local {
		t.Errorf("UseHandler 时没有 xlog 的时区，该回到 time.Local，got=%v", got)
	}
}

func TestCloseXLog_ClosesWithoutInit(t *testing.T) {
	keepGlobals(t)
	closer = nil
	if err := closeXLog(context.Background()); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestCloseXLog_LogsGoToStderrAfterFileClosed(t *testing.T) {
	// 回归用例。关掉文件之后 slog.Default() 原先还指着它：xone.Run 返回的错误、
	// 被丢下的停止钩子、在途请求打的日志，Console 关着时一声不响就没了
	keepGlobals(t)
	dir := t.TempDir()
	xonetest.UseConfigYAML(t, "XLog:\n  Console: false\n  File:\n    Enable: true\n    Path: \""+dir+"\"\n    Name: app.log\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	old := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = old })

	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	slog.Error("shutdown failed")

	b, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "shutdown failed") {
		t.Errorf("关掉文件之后的日志该写到 stderr，got=%q", b)
	}
}

func TestUseHandler_LogsGoToOwnHandlerWithCtxFields(t *testing.T) {
	// 换了后端，xlog 包的那一层照样在：AddKV 的字段、trace_id、观察者都不能丢
	keepGlobals(t)
	var buf bytes.Buffer
	UseHandler(slog.NewJSONHandler(&buf, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "user_id", "u1")
	slog.InfoContext(ctx, "hello")
	if out := buf.String(); !strings.Contains(out, `"msg":"hello"`) || !strings.Contains(out, `"user_id":"u1"`) {
		t.Errorf("日志该写进自己的 handler、带着 AddKV 的字段，got=%q", out)
	}
	if closer != nil {
		t.Errorf("自己的 handler 不归 xlog 关，closer 该是 nil")
	}
}

func TestUseHandler_XLogBlockFailsStartup(t *testing.T) {
	// 写了 Level: debug 却由别人的 handler 决定级别：以为生效了，其实没有
	keepGlobals(t)
	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	xonetest.UseConfigYAML(t, "XLog:\n  Level: debug\n")
	err := initXLog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "UseHandler") {
		t.Fatalf("UseHandler 和 XLog 的输出项同时写，该启动失败并指明原因，got=%v", err)
	}
}

func TestUseHandler_DefaultValuedXLogBlockIsFine(t *testing.T) {
	keepGlobals(t)
	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	xonetest.UseConfigYAML(t, "XLog:\n  Level: info\n")
	if err := initXLog(context.Background()); err != nil {
		t.Errorf("只写了默认值不算冲突，got=%v", err)
	}
}

func TestUseHandler_LateCallIsIgnored(t *testing.T) {
	// 日志装好之后再换，前面的日志已经写到别处了；不生效，但要说出来
	keepGlobals(t)
	var buf bytes.Buffer
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 把装好的 logger 换成能截到输出的，好看见那条 WARN
	slog.SetDefault(slog.New(newCtxHandler(slog.NewJSONHandler(&buf, nil))))
	before := slog.Default().Handler()

	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	if custom.Load() != nil || slog.Default().Handler() != before {
		t.Error("装好之后的 UseHandler 不该生效")
	}
	if !strings.Contains(buf.String(), "xlog.UseHandler called after logging was installed") {
		t.Errorf("调晚了该打一条 WARN，got=%q", buf.String())
	}
}

func TestUseHandler_CloseLeavesOwnHandlerInPlace(t *testing.T) {
	// 关的时候换成 stderr 是为了不写进已经关掉的文件；自己的 handler 没这个问题，不该被换掉
	keepGlobals(t)
	var buf bytes.Buffer
	UseHandler(slog.NewJSONHandler(&buf, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	slog.Info("after close")
	if !strings.Contains(buf.String(), "after close") {
		t.Errorf("关闭之后日志还该写进自己的 handler，got=%q", buf.String())
	}
}

func TestUseHandler_UsableAgainAfterClose(t *testing.T) {
	// 同一进程里跑第二次 Run（测试里常见）：上一轮关掉之后，UseHandler 得重新生效
	keepGlobals(t)
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")
	if err := initXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeXLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	UseHandler(slog.NewJSONHandler(io.Discard, nil))
	if custom.Load() == nil {
		t.Error("关掉之后再调 UseHandler 该生效")
	}
}

// identityLines 按 yml 跑一遍 xapp、xlog 的启动钩子，打一条日志，读回文件里的每一行
func identityLines(t *testing.T, yml string) []map[string]any {
	t.Helper()
	keepGlobals(t)
	dir := t.TempDir()
	xonetest.UseConfigYAML(t, yml+"  Console: false\n  File:\n    Enable: true\n    Path: \""+dir+"\"\n    Name: app.log\n")
	xonetest.StartHooks(t)
	slog.Info("hello")
	return readLines(t, filepath.Join(dir, "app.log"))
}

func TestIdentity_DefaultFieldsOnEveryLine(t *testing.T) {
	// 和 Span 上的 service.name / host.name / process.pid 对得上：日志平台里按服务、按实例过滤
	lines := identityLines(t, "XApp:\n  Name: order-api\n  Version: v1.2.3\nXLog:\n")
	host, _ := os.Hostname()
	l := lines[len(lines)-1]
	for k, want := range map[string]any{
		"service": "order-api", "version": "v1.2.3", "hostname": host, "pid": float64(os.Getpid()),
	} {
		if l[k] != want {
			t.Errorf("%s 应是 %v，got=%v", k, want, l[k])
		}
	}
	if _, ok := l["host"]; ok {
		t.Errorf("机器名叫 hostname：host 在 xgin 访问日志里是请求的 Host 头，got=%v", l)
	}
}

func TestIdentity_EmptyAppFieldsAreOmitted(t *testing.T) {
	// 没配 XApp.Name / Version 时不写一个空的 service 进去
	lines := identityLines(t, "XLog:\n")
	l := lines[len(lines)-1]
	if _, ok := l["service"]; ok {
		t.Errorf("没配 XApp.Name 就不该有 service，got=%v", l)
	}
	if _, ok := l["version"]; ok {
		t.Errorf("没配 XApp.Version 就不该有 version，got=%v", l)
	}
	if l["hostname"] == nil || l["pid"] == nil {
		t.Errorf("hostname、pid 照样有，got=%v", l)
	}
}

func TestFields_ExtraOverrideAndDrop(t *testing.T) {
	t.Setenv("XLOG_TEST_POD", "order-api-7d9f")
	lines := identityLines(t, "XApp:\n  Name: order-api\nXLog:\n  Fields:\n"+
		"    pod: ${XLOG_TEST_POD}\n    zone: ${XLOG_TEST_UNSET_ZONE:}\n    service: renamed\n    hostname: \"\"\n")
	l := lines[len(lines)-1]
	if l["pod"] != "order-api-7d9f" {
		t.Errorf("Fields 的值该从环境变量取，got=%v", l["pod"])
	}
	if _, ok := l["zone"]; ok {
		t.Errorf("${VAR:} 没设时是空串，空串的字段不写，got=%v", l)
	}
	if l["service"] != "renamed" {
		t.Errorf("同名的以 Fields 为准，got=%v", l["service"])
	}
	if _, ok := l["hostname"]; ok {
		t.Errorf("写成空串就是不要这个默认字段，got=%v", l)
	}
}

func TestNew_OnlyFieldsNoIdentity(t *testing.T) {
	// New 是纯构造器：只带 cfg.Fields，不去读 XApp、主机名
	c, path := fileCfg(t)
	c.Fields = map[string]string{"b": "2", "a": "1"}
	l, cl, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")
	cl.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	if !strings.Contains(line, `"a":"1","b":"2"`) || strings.Contains(line, "hostname") {
		t.Errorf("New 只带 Fields（按名字排序），got=%s", line)
	}
}

func TestUseHandler_CarriesIdentityAndFields(t *testing.T) {
	// 换了后端照样带身份字段；Fields 不归 handler 管，写了也不算冲突
	keepGlobals(t)
	var buf bytes.Buffer
	UseHandler(slog.NewJSONHandler(&buf, nil))
	xonetest.UseConfigYAML(t, "XApp:\n  Name: order-api\nXLog:\n  Fields:\n    zone: az1\n")
	xonetest.StartHooks(t)
	slog.Info("hello")
	out := buf.String()
	if !strings.Contains(out, `"service":"order-api"`) || !strings.Contains(out, `"zone":"az1"`) || !strings.Contains(out, `"pid":`) {
		t.Errorf("自己的 handler 也该带上身份字段和 Fields，got=%s", out)
	}
}
