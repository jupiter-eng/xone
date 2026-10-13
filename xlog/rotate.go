package xlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jupiter-eng/xone/xerror"
)

// 日志文件名的时间后缀格式，按轮转周期选择合适的粒度
// 周期小于一天时需要更细的粒度，否则不同周期会生成同名文件而无法真正轮转
const (
	rotateLayoutDay    = "20060102"
	rotateLayoutHour   = "2006010215"
	rotateLayoutMinute = "200601021504"
)

// rotateLayouts 三种后缀格式，各自对应的标准周期，从粗到细。
//
// 选格式（rotateLayoutFor）和清理（expired）共用这一张表。清理时三种都认：
// 换过 RotateTime 之后，按旧粒度命名的那些文件同样是我们写的，不能因为
// 粒度变了就永远留在那里。三者长度各不相同（8 / 10 / 12 位），
// 一个后缀最多只能被其中一种解析成功
var rotateLayouts = []struct {
	layout string
	period time.Duration // 周期不短于它时选这种格式
}{
	{rotateLayoutDay, 24 * time.Hour},
	{rotateLayoutHour, time.Hour},
	{rotateLayoutMinute, time.Minute},
}

// defaultLogFilePerm 日志文件默认权限
const defaultLogFilePerm = 0o644

// rotateErrLogInterval 轮转失败告警的最小间隔
const rotateErrLogInterval = time.Minute

// rotateWriter 按时间轮转的日志文件写入器
//
// 文件名形如 {base}.20260912，并维护一个指向当前文件的符号链接 {base}，
// 便于 tail 等工具始终跟随最新日志。
//
// slog 的 handler 会从任意多个协程并发调用 Write，Close 也可能来自别的协程，故加锁保护。
// 没有缓冲：每条日志都直接 write 到文件，关闭时没有要 flush 的东西。
type rotateWriter struct {
	base     string        // 文件名前缀，如 /var/log/app.log
	linkName string        // 符号链接路径，为空表示不创建
	layout   string        // 时间后缀格式
	maxAge   time.Duration // 超过该时长的历史文件会被清理，<=0 表示不清理
	rotate   time.Duration // 轮转周期

	// clock 可注入的时间源，便于测试轮转与清理
	clock func() time.Time

	// perm 日志文件权限
	perm os.FileMode

	mu          sync.Mutex
	file        *os.File // Close 之后为 nil
	currentName string

	// lastRotateErrAt 上次记录轮转失败的时刻，用于告警降噪
	lastRotateErrAt time.Time
}

// newRotateWriter 创建轮转写入器，立即打开当前文件，并清理一次过期文件。
//
// 提前打开可将权限、路径等问题暴露在初始化阶段，而非首次写日志时才失败。
// 清理也要在这里做一次，不能只挂在轮转上：按天轮转的服务每次发版重启都落在
// 周期中间，一天重启几次的话，过期文件可能永远等不到那次轮转。
func newRotateWriter(base string, maxAge, rotate time.Duration, perm os.FileMode) (*rotateWriter, error) {
	if perm == 0 {
		perm = defaultLogFilePerm
	}
	w := &rotateWriter{
		base:     base,
		linkName: base,
		layout:   rotateLayoutFor(rotate),
		maxAge:   maxAge,
		rotate:   rotate,
		clock:    time.Now,
		perm:     perm,
	}
	// {base} 上要放的是符号链接。那里已经有一个别的文件时（老版本直接写的 app.log、
	// 别的日志库留下的），Rename 会把它原子地替换掉，里面的日志一声不响就没了。
	// 挪走它也不对——挪到哪、叫什么，都是在替使用者做他不知道的决定。
	// 所以拒绝启动，把选择交还给他
	if occupied(w.linkName) {
		return nil, xerror.Newf("xlog", "new",
			"%s exists and is not a symlink; xlog keeps a symlink to the current log file there and will not overwrite it: move it away or change File.Name", w.linkName)
	}
	if err := w.rotateTo(w.filenameFor(w.clock())); err != nil {
		return nil, xerror.New("xlog", "new", err)
	}
	w.purge(w.currentName)
	return w, nil
}

// occupied 路径上有东西，且不是符号链接
func occupied(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink == 0
}

// rotateLayoutFor 依据轮转周期选择时间后缀粒度：周期够得着的最粗那一档
func rotateLayoutFor(d time.Duration) string {
	for _, l := range rotateLayouts {
		if d >= l.period {
			return l.layout
		}
	}
	return rotateLayoutMinute
}

// Write 写入日志，跨越轮转周期时先切换文件
func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, os.ErrClosed
	}

	if name := w.filenameFor(w.clock()); name != w.currentName {
		if err := w.rotateTo(name); err != nil {
			// 轮转失败不能丢日志：旧文件句柄仍然可用，降级继续写它。
			// 直接返回错误的话，从第一次轮转失败起每条日志都走这条路，
			// 而 slog.Logger 会把 handler 返回的错误直接丢掉——
			// 现象就是某天日志突然断了，没有任何报错
			w.reportRotateFailure(err)
		} else {
			// 清理放到后台执行，避免阻塞日志写入；当前文件名以参数传入，避免再次取锁
			go w.purge(name)
		}
	}

	return w.file.Write(p)
}

// reportRotateFailure 记录轮转失败
// 按间隔降噪：轮转周期到了之后每条日志都会重试，不限流会把告警刷爆
func (w *rotateWriter) reportRotateFailure(err error) {
	now := w.clock()
	if w.lastRotateErrAt.IsZero() || now.Sub(w.lastRotateErrAt) >= rotateErrLogInterval {
		w.lastRotateErrAt = now
		warnf("rotate: failed, keep writing current file=[%s], err=[%v]", w.currentName, err)
	}
}

// Close 关闭当前日志文件，多次调用安全
func (w *rotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// filenameFor 计算指定时刻所属周期的日志文件名
func (w *rotateWriter) filenameFor(t time.Time) string {
	return w.base + "." + truncateInLocation(t, w.rotate).Format(w.layout)
}

// rotateTo 切换到指定文件
// 调用方需持有锁；构造阶段实例尚未被共享，此时无需加锁。
//
// 返回普通 error：构造时由 newRotateWriter 包成 xerror，运行中的轮转失败
// 只走 warnf，不出本包
func (w *rotateWriter) rotateTo(name string) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, w.perm)
	if err != nil {
		return fmt.Errorf("open log file failed, file=[%s]: %w", name, err)
	}

	if w.file != nil {
		// 旧文件关闭失败不影响继续写入新文件，仅记录
		if cerr := w.file.Close(); cerr != nil {
			warnf("rotate: close previous file failed, err=[%v]", cerr)
		}
	}
	w.file = f
	w.currentName = name

	w.updateSymlink(name)
	return nil
}

// updateSymlink 将符号链接指向当前文件
// 符号链接在部分平台或文件系统上不被支持，失败不影响日志写入
func (w *rotateWriter) updateSymlink(target string) {
	if w.linkName == "" {
		return
	}
	// 构造时已经拦过一次；这里防的是运行中有人把链接换成了普通文件
	// （手工 mv、copytruncate 式的工具），轮转时照样 Rename 过去就把它吞了
	if occupied(w.linkName) {
		warnf("rotate: %s is not a symlink, leaving it alone and not updating the link", w.linkName)
		return
	}

	// 先创建临时链接再原子替换，避免替换过程中链接短暂缺失
	tmp := w.linkName + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Base(target), tmp); err != nil {
		warnf("rotate: create symlink failed, link=[%s], err=[%v]", tmp, err)
		return
	}
	if err := os.Rename(tmp, w.linkName); err != nil {
		warnf("rotate: replace symlink failed, link=[%s], err=[%v]", w.linkName, err)
		_ = os.Remove(tmp)
	}
}

// purge 删除超过 maxAge 的历史日志文件，current 为当前正在写入的文件。
//
// 只删我们自己命名的文件（{base}.{时间后缀}）。base.* 匹配到的不全是我们写的：
// 使用者手工备份的 app.log.bak、别的工具压缩出来的 app.log.1.gz 都在里面，
// 它们解析不出时间，就一律不碰——哪怕 mtime 很旧
func (w *rotateWriter) purge(current string) {
	if w.maxAge <= 0 {
		return
	}

	matches, err := filepath.Glob(w.base + ".*")
	if err != nil {
		warnf("rotate: glob log files failed, err=[%v]", err)
		return
	}

	// filepath.Glob 返回结果已排序，删除顺序是确定的
	cutoff := w.clock().Add(-w.maxAge)
	for _, path := range matches {
		// Lstat 而非 Stat：避免跟随符号链接把当前文件误判为历史文件
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			continue // 跳过符号链接本身
		}
		if path == current {
			continue // 不删除正在写入的文件
		}
		if !w.expired(path, cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			warnf("rotate: remove expired log failed, file=[%s], err=[%v]", path, err)
		}
	}
}

// expired 判断历史日志文件是否已过保留期；不是我们命名的文件一律返回 false
//
// 用文件名里的时间后缀，而不是 mtime：备份恢复、rsync、容器镜像分层
// 都会重写 mtime，按它判断可能把昨天的日志当成刚写的而永远不清，
// 也可能把刚轮转出来的文件当成过期的删掉
func (w *rotateWriter) expired(path string, cutoff time.Time) bool {
	suffix := strings.TrimPrefix(path, w.base+".")
	for _, l := range rotateLayouts {
		t, err := time.ParseInLocation(l.layout, suffix, w.clock().Location())
		if err != nil {
			continue
		}
		// 文件名记的是所属周期的起点，整个周期结束后才算过期。
		// 按当前粒度命名的，周期就是 RotateTime；按别的粒度命名的是改配置之前
		// 留下的，按那一档的标准周期算。都按现在的 RotateTime 算的话，从按天
		// 改成按小时之后，今天那个按天的文件零点一过就「过期」了，
		// 而它直到这次重启之前都还在写
		period := l.period
		if l.layout == w.layout {
			period = w.rotate
		}
		return !t.Add(period).After(cutoff)
	}
	return false
}

// truncateInLocation 按周期截断时间，且对齐到本地时区
//
// time.Truncate 以 UTC 零点为基准，直接用于按天轮转会导致切割点落在本地时间的
// 非零点时刻。这里先把本地时间的各字段原样搬到 UTC 上做截断，再搬回本地时区，
// 使按天轮转对齐本地零点（与替换前的 file-rotatelogs 行为保持一致）。
func truncateInLocation(t time.Time, d time.Duration) time.Time {
	if d <= 0 {
		return t
	}
	if t.Location() == time.UTC {
		return t.Truncate(d)
	}

	base := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	base = base.Truncate(d)
	return time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), base.Minute(), base.Second(), base.Nanosecond(), t.Location())
}

// warnf 把日志系统自身的故障直接写到 stderr
//
// 不能走日志系统：出错的正是它，再调用它就成了环。
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "xlog: "+format+"\n", args...)
}
