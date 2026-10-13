package xlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestWriter 造一个时钟可控的轮转写入器
func newTestWriter(t *testing.T, rotate, maxAge time.Duration, now *time.Time) (*rotateWriter, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "app.log")
	w, err := newRotateWriter(base, maxAge, rotate, 0)
	if err != nil {
		t.Fatalf("创建轮转写入器失败：%v", err)
	}
	t.Cleanup(func() { w.Close() })
	w.clock = func() time.Time { return *now }
	// 构造时用的是真实时间，重新按测试时钟切一次，保证后续文件名可预期
	if err := w.rotateTo(w.filenameFor(*now)); err != nil {
		t.Fatalf("按测试时钟切换失败：%v", err)
	}
	return w, base
}

func TestRotateLayoutFor(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{24 * time.Hour, rotateLayoutDay},
		{48 * time.Hour, rotateLayoutDay},
		{time.Hour, rotateLayoutHour},
		{6 * time.Hour, rotateLayoutHour},
		{time.Minute, rotateLayoutMinute},
		{0, rotateLayoutMinute},
	}
	for _, c := range cases {
		if got := rotateLayoutFor(c.d); got != c.want {
			t.Errorf("周期 %v 应选 %s，got=%s", c.d, c.want, got)
		}
	}
}

func TestRotateWriter_SwitchesFileAcrossPeriods(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	w.Write([]byte("第一分钟\n"))
	now = now.Add(time.Minute)
	w.Write([]byte("第二分钟\n"))

	first := base + ".202609181030"
	second := base + ".202609181031"
	if got := readFile(t, first); got != "第一分钟\n" {
		t.Errorf("%s 内容不对，got=%q", first, got)
	}
	if got := readFile(t, second); got != "第二分钟\n" {
		t.Errorf("%s 内容不对，got=%q", second, got)
	}
}

func TestRotateWriter_SymlinkFollowsCurrentFile(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	if got, err := os.Readlink(base); err != nil || got != "app.log.202609181030" {
		t.Fatalf("链接应指向当前文件，got=%q err=%v", got, err)
	}
	now = now.Add(time.Minute)
	w.Write([]byte("x\n"))
	if got, _ := os.Readlink(base); got != "app.log.202609181031" {
		t.Errorf("轮转后链接应改指新文件，got=%q", got)
	}
	// 用链接名读到的就是新文件的内容
	if got := readFile(t, base); got != "x\n" {
		t.Errorf("经链接读到的内容不对，got=%q", got)
	}
}

func TestRotateWriter_PurgeRemovesExpiredFiles(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	stale := base + ".202609181010" // 20 分钟前，早该清
	fresh := base + ".202609181029" // 上一分钟，还在保留期内
	current := base + ".202609181030"
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w.purge(current)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("过期文件应被删除：%s", stale)
	}
	for _, p := range []string{fresh, current, base} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("不该删的被删了：%s (%v)", p, err)
		}
	}
}

func TestRotateWriter_PurgeKeepsSymlink(t *testing.T) {
	// 替换链接期间会短暂存在 base.tmp，它匹配 base.* 但不能当历史文件删
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, time.Minute, &now)

	tmp := base + ".tmp"
	if err := os.Symlink("app.log.202609181030", tmp); err != nil {
		t.Skipf("当前环境不支持符号链接：%v", err)
	}
	w.purge(base + ".202609181030")

	if _, err := os.Lstat(tmp); err != nil {
		t.Errorf("临时链接不该被删：%v", err)
	}
}

func TestRotateWriter_PurgeSkipsWhenClosed(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now) // maxAge <= 0

	stale := base + ".202601010000"
	os.WriteFile(stale, []byte("x"), 0o644)
	w.purge(base + ".202609181030")

	if _, err := os.Stat(stale); err != nil {
		t.Errorf("maxAge<=0 表示不清理，但文件没了：%v", err)
	}
}

func TestRotateWriter_ExpiredByFileNameNotMtime(t *testing.T) {
	// 备份恢复、rsync、镜像分层都会重写 mtime，按它判断会误删或永不清
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	old := base + ".202609181000"
	os.WriteFile(old, []byte("x"), 0o644)
	os.Chtimes(old, now, now) // mtime 是刚刚，文件名说是半小时前

	w.purge(base + ".202609181030")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("文件名已过期就该删，不该被新 mtime 救回来")
	}
}

func TestRotateWriter_PurgeOnlyDeletesOwnNamedFiles(t *testing.T) {
	// base.* 匹配到的不全是我们写的：使用者手工备份的 app.log.bak、
	// 别的工具压缩出来的 app.log.1.gz 都在里面。从前文件名解析不了时退回
	// 按 mtime 判断，于是一份一个月前的手工备份被当成过期日志删掉了
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	var others []string
	for _, suffix := range []string{".bak", ".1.gz", ".不是时间", ".2026091810301", ".tmp"} {
		p := base + suffix
		os.WriteFile(p, []byte("别人的"), 0o644)
		os.Chtimes(p, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))
		others = append(others, p)
	}

	// 换过 RotateTime 的话，之前按天命名的那些也是我们写的，照样到期就清，
	// 不能因为粒度变了就永远留在那里
	byDay := base + ".20260901"
	os.WriteFile(byDay, []byte("x"), 0o644)

	w.purge(base + ".202609181030")
	for _, p := range others {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("不是我们命名的文件不该删：%s (%v)", p, err)
		}
	}
	if _, err := os.Stat(byDay); !os.IsNotExist(err) {
		t.Errorf("按别的粒度命名的过期日志也是我们写的，该清掉：%s", byDay)
	}
}

func TestRotateWriter_PurgeUsesOldGranularityPeriodForExpiry(t *testing.T) {
	// 从按天轮转改成按小时之后，今天那个按天命名的文件直到重启前还在写。
	// 按现在的一小时周期算，它零点一过就「过期」了——里面是几分钟前的日志
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Hour, time.Hour, &now)

	today := base + ".20260918"
	old := base + ".20260915"
	for _, p := range []string{today, old} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w.purge(w.currentName)
	if _, err := os.Stat(today); err != nil {
		t.Errorf("今天的按天文件还没过保留期，不该删：%v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("三天前的按天文件早过期了，该删：%s", old)
	}
}

func TestRotateWriter_PurgesExpiredFilesOnOpen(t *testing.T) {
	// 清理只挂在轮转上的话，按天轮转的服务每次发版重启都在周期中间，
	// 一天重启几次，过期文件就可能永远等不到那次轮转
	dir := t.TempDir()
	base := filepath.Join(dir, "app.log")
	stale := base + "." + time.Now().Add(-30*24*time.Hour).Format(rotateLayoutDay)
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := newRotateWriter(base, 7*24*time.Hour, 24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("打开时就该清掉过期文件：%s", stale)
	}
}

func TestRotateWriter_RegularFileNotOverwrittenBySymlink(t *testing.T) {
	// {Path}/{Name} 上要放的是指向当前文件的符号链接。那里已经有一个普通文件时
	// （从前直接写 app.log 的老版本、别的日志库留下的），Rename 会把它
	// 原子地替换掉——里面的旧日志一声不响就没了
	dir := t.TempDir()
	base := filepath.Join(dir, "app.log")
	if err := os.WriteFile(base, []byte("老日志\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := newRotateWriter(base, 0, 24*time.Hour, 0)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Errorf("该拒绝启动并说清原因，got=%v", err)
	}
	if got := readFile(t, base); got != "老日志\n" {
		t.Errorf("原来的文件不该被动，got=%q", got)
	}
}

func TestRotateWriter_ExpiredBoundary(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)
	cutoff := now.Add(-5 * time.Minute) // 10:25

	// 10:24 这一分钟的文件，周期在 10:25 结束，正好等于 cutoff —— 算过期
	if !w.expired(base+".202609181024", cutoff) {
		t.Error("周期终点等于 cutoff 应判为过期")
	}
	// 10:25 这一分钟的文件，周期到 10:26 才结束 —— 不算过期
	if w.expired(base+".202609181025", cutoff) {
		t.Error("周期终点晚于 cutoff 不该判为过期")
	}
}

func TestRotateWriter_FallsBackToOldFileOnRotateFailure(t *testing.T) {
	// 轮转失败直接返错的话，从此每条日志都失败，而 Close 时才暴露出来，
	// 现象就是日志某天突然断了却没有任何报错
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	// 在下一周期的文件名上放一个目录，让 OpenFile 失败
	if err := os.Mkdir(base+".202609181031", 0o755); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)

	n, err := w.Write([]byte("降级\n"))
	if err != nil || n == 0 {
		t.Fatalf("轮转失败不该让写入失败，n=%d err=%v", n, err)
	}
	if got := readFile(t, base+".202609181030"); got != "降级\n" {
		t.Errorf("应继续写旧文件，got=%q", got)
	}
}

func TestRotateWriter_RotateFailureWarningsThrottled(t *testing.T) {
	// 周期到了之后每条日志都会重试轮转，不限流会把告警刷爆
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, _ := newTestWriter(t, time.Minute, 0, &now)

	w.reportRotateFailure(errFake{})
	first := w.lastRotateErrAt
	if first.IsZero() {
		t.Fatal("首次失败应记录时刻")
	}

	now = now.Add(rotateErrLogInterval / 2)
	w.reportRotateFailure(errFake{})
	if !w.lastRotateErrAt.Equal(first) {
		t.Error("间隔内的重复失败应被压掉")
	}

	now = now.Add(rotateErrLogInterval)
	w.reportRotateFailure(errFake{})
	if w.lastRotateErrAt.Equal(first) {
		t.Error("超过间隔后应重新告警")
	}
}

func TestRotateWriter_CloseRejectsWritesAfterClose(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, _ := newTestWriter(t, time.Minute, 0, &now)

	if err := w.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("重复 Close 应安全，got=%v", err)
	}
	if _, err := w.Write([]byte("x")); err != os.ErrClosed {
		t.Errorf("关闭后写入应返回 ErrClosed，got=%v", err)
	}
}

func TestRotateWriter_FailsToConstructWhenFileCannotOpen(t *testing.T) {
	// 权限、路径问题要在初始化阶段暴露，而不是等到第一条日志
	_, err := newRotateWriter(filepath.Join(t.TempDir(), "没有这个目录", "app.log"), 0, time.Hour, 0)
	if err == nil {
		t.Fatal("路径不存在时应构造失败")
	}
	if !strings.Contains(err.Error(), "xlog") {
		t.Errorf("错误应带模块名，got=%v", err)
	}
}

func TestTruncateInLocation(t *testing.T) {
	// 按天轮转必须对齐本地零点。time.Truncate 以 UTC 零点为基准，
	// 在 UTC+8 直接用会把切割点放在本地早上 8 点
	loc := time.FixedZone("UTC+8", 8*3600)
	got := truncateInLocation(time.Date(2026, 9, 18, 3, 15, 0, 0, loc), 24*time.Hour)
	want := time.Date(2026, 9, 18, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("按天截断应落在本地零点，got=%v want=%v", got, want)
	}

	utc := time.Date(2026, 9, 18, 3, 15, 0, 0, time.UTC)
	if g := truncateInLocation(utc, time.Hour); !g.Equal(time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC 按小时截断出错，got=%v", g)
	}
	if g := truncateInLocation(utc, 0); !g.Equal(utc) {
		t.Errorf("周期 <=0 应原样返回，got=%v", g)
	}
}

func TestWarnfWritesToStderr(t *testing.T) {
	// 轮转器是日志系统的底层写入器，自身故障不能再走日志系统，否则成环
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	warnf("出事了 file=[%s]", "a.log")
	os.Stderr = old
	f.Close()

	if got := readFile(t, f.Name()); !strings.Contains(got, "xlog: 出事了 file=[a.log]") {
		t.Errorf("stderr 内容不对，got=%q", got)
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake" }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return string(b)
}

func TestRotateWriter_NoSymlinkWhenNotConfigured(t *testing.T) {
	now := time.Now()
	w, base := newTestWriter(t, time.Hour, 0, &now)
	w.linkName = ""
	if err := os.Remove(base); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if err := w.rotateTo(w.filenameFor(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Errorf("没配 linkName 就不该建符号链接，got err=%v", err)
	}
}

func TestRotateWriter_KeepsWritingWhenSymlinkCannotBeCreated(t *testing.T) {
	// 部分平台和文件系统不支持符号链接，那不该让日志整个写不出去
	now := time.Now()
	w, _ := newTestWriter(t, time.Hour, 0, &now)
	// 指向一个不存在的目录，Symlink 必然失败
	w.linkName = filepath.Join(t.TempDir(), "nope", "app.log")

	now = now.Add(time.Hour) // 跨周期，让这次写入触发一次轮转
	if _, err := w.Write([]byte("still writing\n")); err != nil {
		t.Fatalf("日志应当照常写得出去：%v", err)
	}

	next := w.filenameFor(now)
	b, err := os.ReadFile(next)
	if err != nil || !strings.Contains(string(b), "still writing") {
		t.Errorf("内容没落到新文件上，err=%v, got=%q", err, b)
	}
}

func TestRotateWriter_DoesNotOverwriteOccupiedLinkPathAtRuntime(t *testing.T) {
	// 启动之后有人把链接换成了普通文件（手工 mv、copytruncate 式的工具）。
	// 轮转时照样 Rename 过去的话，那个文件就被吞掉了；临时链接也不该留下，
	// 否则目录里会越堆越多 app.log.tmp
	now := time.Now()
	w, base := newTestWriter(t, time.Hour, 0, &now)

	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("别人的\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.updateSymlink(w.filenameFor(now))
	if got := readFile(t, base); got != "别人的\n" {
		t.Errorf("占住链接位置的文件不该被覆盖，got=%q", got)
	}
	if _, err := os.Lstat(base + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("不该留下临时链接，got err=%v", err)
	}
}

func TestRotateWriter_NoPurgeWithoutMaxAge(t *testing.T) {
	// 0 表示「一直留着」，不是「全删掉」
	now := time.Now()
	w, base := newTestWriter(t, time.Hour, 0, &now)

	old := base + ".2000010100"
	if err := os.WriteFile(old, []byte("ancient"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.purge(w.currentName)

	if _, err := os.Stat(old); err != nil {
		t.Errorf("MaxAge 为 0 时不该删任何东西，got err=%v", err)
	}
}
