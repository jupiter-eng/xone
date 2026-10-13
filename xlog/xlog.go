package xlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"
)

// New 按配置造一个 logger。
//
// 纯构造器：不碰全局、不读配置文件、不依赖框架运行。测试直接调它。
//
// 返回的 io.Closer 用于收尾（关闭日志文件），即便没有文件输出也不会是 nil，
// 调用方不必判空。
//
// 只带 cfg.Fields；service、hostname 这些默认字段是框架装配时（initXLog）才加的。
func New(cfg Config) (*slog.Logger, io.Closer, error) { return build(cfg, nil) }

// build 同 New，每条日志另带 base 这组字段（被 cfg.Fields 同名的覆盖）
func build(cfg Config, base []slog.Attr) (*slog.Logger, io.Closer, error) {
	// 配置项全部先校验完，再动文件。反过来的话，Format 写错时
	// 日志文件已经建好、fd 也开着，而 New 返回了错误——调用方手上
	// 没有 Closer 可关，那个 fd 和它的符号链接就留在那里了
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}
	newHandler, err := handlerFor(cfg.Format)
	if err != nil {
		return nil, nil, err
	}
	loc, err := parseLocation(cfg.Timezone)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.File.validate(); err != nil {
		return nil, nil, xerror.New("xlog", "config", err)
	}

	writers := make([]io.Writer, 0, 2)
	closers := make([]io.Closer, 0, 1)

	if cfg.Console {
		writers = append(writers, os.Stdout)
	}
	if cfg.File.Enable {
		w, err := newFileWriter(cfg.File)
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, w)
		closers = append(closers, w)
	}

	// 一个输出都没开时 MultiWriter 什么也不写（等同 io.Discard）而不是报错：
	// 「我就是不要日志」是个合理的选择，不该让服务起不来
	out := io.MultiWriter(writers...)

	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.AddSource, ReplaceAttr: inLocation(loc)}
	return withStatic(slog.New(newCtxHandler(newHandler(out, opts))), base, cfg.Fields), multiCloser(closers), nil
}

// identity 框架默认给每条日志带的身份字段，和 xtrace 的 Span 上的 service.name、host.name、process.pid 对得上。
//
// 机器名叫 hostname 不叫 host：xgin 的访问日志里 host 是请求的 Host 头，同名就是一条日志两个 host
func identity() []slog.Attr {
	hostname, _ := os.Hostname()
	return []slog.Attr{
		slog.String("service", xapp.Name()),
		slog.String("version", xapp.Version()),
		slog.String("hostname", hostname),
		slog.Int("pid", os.Getpid()),
	}
}

// withStatic 给 logger 挂上静态字段：base 在前，同名的以 fields 为准，其余按名字排序跟在后面；值为空串的不写。
//
// 挂在 With 上：JSON / text handler 在这里就把它们序列化好了，之后每条日志只是拷一段现成的字节
func withStatic(l *slog.Logger, base []slog.Attr, fields map[string]string) *slog.Logger {
	attrs := make([]any, 0, len(base)+len(fields))
	add := func(a slog.Attr) {
		if a.Value.Kind() != slog.KindString || a.Value.String() != "" {
			attrs = append(attrs, a)
		}
	}
	for _, a := range base {
		if v, ok := fields[a.Key]; ok {
			a = slog.String(a.Key, v)
		}
		add(a)
	}
	extra := make([]string, 0, len(fields))
	for k := range fields {
		if !slices.ContainsFunc(base, func(a slog.Attr) bool { return a.Key == k }) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	for _, k := range extra {
		add(slog.String(k, fields[k]))
	}
	if len(attrs) == 0 {
		return l
	}
	return l.With(attrs...)
}

// parseLocation 解析 IANA 时区名。留空表示跟随本地时区
func parseLocation(name string) (*time.Location, error) {
	if name == "" {
		return nil, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, xerror.Newf("xlog", "config",
			"unknown Timezone=[%s]; images without /usr/share/zoneinfo need `import _ \"time/tzdata\"` in your main package: %w",
			name, err)
	}
	return loc, nil
}

// inLocation 把记录时间换到指定时区。loc 为 nil 时返回 nil，
// 于是 HandlerOptions.ReplaceAttr 保持为空，热路径上一次多余的函数调用都没有
func inLocation(loc *time.Location) func([]string, slog.Attr) slog.Attr {
	if loc == nil {
		return nil
	}
	return func(groups []string, a slog.Attr) slog.Attr {
		// 只动顶层的时间字段：业务自己打的 time.Time 是它的数据，不该被改时区
		if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
			a.Value = slog.TimeValue(a.Value.Time().In(loc))
		}
		return a
	}
}

// handlerFor 按格式选出 handler 的构造函数。
// 只认格式、不碰输出，好让格式写错这件事在打开日志文件之前就暴露。
func handlerFor(format string) (func(io.Writer, *slog.HandlerOptions) slog.Handler, error) {
	switch strings.ToLower(format) {
	case FormatText:
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewTextHandler(w, o) }, nil
	case FormatJSON, "":
		return func(w io.Writer, o *slog.HandlerOptions) slog.Handler { return slog.NewJSONHandler(w, o) }, nil
	default:
		return nil, xerror.Newf("xlog", "config",
			"unknown log format Format=[%s], expected %s or %s", format, FormatJSON, FormatText)
	}
}

// newFileWriter 造文件写入器，目录不存在时创建
func newFileWriter(c FileConfig) (io.WriteCloser, error) {
	// 先解析权限再建目录：Perm 写错时不该留下一个空目录
	perm, err := parsePerm(c.Perm)
	if err != nil {
		return nil, err
	}
	// 建目录失败是运行环境的问题（没权限、只读盘），配置本身合法，所以 op 是 new 而不是 config
	if c.Path != "" {
		if err := os.MkdirAll(c.Path, 0o755); err != nil {
			return nil, xerror.Newf("xlog", "new", "create log dir failed Path=[%s]: %w", c.Path, err)
		}
	}
	return newRotateWriter(filepath.Join(c.Path, c.Name), c.MaxAge, c.RotateTime, perm)
}

// parseLevel 解析日志级别。不认识的值直接报错而不是退回默认值——
// 配置写错了应该在启动时知道，而不是上线后发现日志级别不对。
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, xerror.Newf("xlog", "config",
			"unknown log level Level=[%s], expected debug / info / warn / error", s)
	}
}

// parsePerm 解析八进制权限字符串，认 "0644"、"644" 和 YAML 1.2 的 "0o644"
func parsePerm(s string) (os.FileMode, error) {
	if s == "" {
		return defaultLogFilePerm, nil
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil {
		return 0, xerror.Newf("xlog", "config",
			"malformed log file permission Perm=[%s], expected an octal string such as \"0644\": %w", s, err)
	}
	return os.FileMode(v), nil
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var first error
	for _, c := range m {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---- 登记 ----

// location 生效中的日志时区，每次装日志时都重新写。nil 表示跟随本地时区
var location atomic.Pointer[time.Location]

// Location 返回日志时间戳用的时区。
//
// 业务自己要格式化时间、又想和日志对得上时用它，免得把配置里的时区名
// 在代码里再抄一遍。没配 XLog.Timezone 时返回 time.Local。
//
//	t.In(xlog.Location()).Format(time.RFC3339)
func Location() *time.Location {
	if loc := location.Load(); loc != nil {
		return loc
	}
	return time.Local
}

// init 日志要最先起、最后关，这样其余组件的启停日志都写得出去
func init() {
	xhook.BeforeStart(initXLog, xhook.At(xhook.StageLog))
	xhook.BeforeStop(closeXLog) // 档位跟着上面那个启动钩子
}

// closer 由 initXLog 填好，closeXLog 用它关掉写入器
var closer io.Closer

// custom 是 UseHandler 给的后端，nil 表示按 XLog 配置建；installed 表示日志已经装好
var (
	custom    atomic.Pointer[slog.Handler]
	installed atomic.Bool
)

// UseHandler 让日志写到你自己的 handler 上（zap 的 slog 桥、公司的日志 SDK……）。
// 在 xone.Run 之前调，比如 main 的第一行。
//
// xlog 照样把它包一层再装成 slog.Default()：trace_id、AddKV / CtxWithKV 的字段、
// 错误日志计数都还在，框架的访问日志、SQL 日志也都写进它，只是最后由它来写。
//
// 这时 XLog 里除了 Fields（每条都带的字段，照样带上）都是决定 xlog 自己怎么写的，一项都不起作用；
// 写了就启动失败，免得你以为 Level: debug 生效了。级别、格式、输出去向都由你的 handler 决定。
// 传 nil 恢复按 XLog 配置。
//
// 日志装好之后（xone.Run 已经走过日志那一档）再调不会生效，只打一条 WARN。
func UseHandler(h slog.Handler) {
	if installed.Load() {
		slog.Warn("xlog.UseHandler called after logging was installed, ignored; call it before xone.Run")
		return
	}
	if h == nil {
		custom.Store(nil)
		return
	}
	custom.Store(&h)
}

// initXLog 读配置，然后装好日志
func initXLog(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	if h := custom.Load(); h != nil {
		// Fields 不归 handler 管，换了后端照样带上；别的项决定的是 xlog 自己怎么写。
		// 块写了但一项都没改默认值（比如只写了 Level: info）不算冲突：它本来就是这个意思
		output := c
		output.Fields = nil
		if !reflect.DeepEqual(output, DefaultConfig()) {
			return xerror.Newf("xlog", "config",
				"%s has no effect when xlog.UseHandler is set: level, format and outputs are up to your handler; only %s.Fields applies",
				ConfigKey, ConfigKey)
		}
		closer = nil
		location.Store(nil) // 时间戳由你的 handler 决定，xlog 没有时区
		slog.SetDefault(withStatic(slog.New(newCtxHandler(*h)), identity(), c.Fields))
		installed.Store(true)
		return nil
	}
	return install(c)
}

// install 按配置建好 logger，并把它装成标准库的全局默认值
func install(c Config) error {
	l, cl, err := build(c, identity())
	if err != nil {
		return err
	}
	closer = cl
	// New 已经校验过，这里不会再失败。没配时存 nil（跟随本地时区）也要存：
	// 同一个进程里的上一次 Run 配过时区的话，不存就一直留着那一个
	loc, _ := parseLocation(c.Timezone)
	location.Store(loc)
	// 装进标准库的全局默认 logger：业务代码直接用 slog.Info / slog.InfoContext，
	// 不需要认识本包。这与 slog.SetDefault 是同一个模式。
	slog.SetDefault(l)
	installed.Store(true)
	return nil
}

// closeXLog 关掉日志文件。
//
// 写入没有缓冲（每条日志直接 write 到文件），所以这里没有要 flush 的东西，
// 只是把 fd 还回去。
//
// 关之前先把全局 logger 换成写 stderr 的那个：日志是最后关的，但关完之后
// 还有日志要打——xone.Run 返回的错误、超时后被丢下的停止钩子、没做完的在途请求。
// 原先 slog.Default() 还指着已经关掉的文件，Console 关着的话这些日志一声不响就没了，
// 而它们恰恰是排查「为什么没有正常退出」最要紧的那几条
func closeXLog(context.Context) error {
	installed.Store(false) // 同一进程里再跑一次 Run（测试里常见）时，UseHandler 照样能用
	if closer == nil {
		return nil
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	return closer.Close()
}
