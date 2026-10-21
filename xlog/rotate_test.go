package xlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestWriter builds a rotating writer with a controllable clock.
func newTestWriter(t *testing.T, rotate, maxAge time.Duration, now *time.Time) (*rotateWriter, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "app.log")
	w, err := newRotateWriter(base, maxAge, rotate, 0)
	if err != nil {
		t.Fatalf("creating the rotating writer failed: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	w.clock = func() time.Time { return *now }
	// Construction used the real time, so it switches once more on the test clock to keep the later file names predictable.
	if err := w.rotateTo(w.filenameFor(*now)); err != nil {
		t.Fatalf("switching on the test clock failed: %v", err)
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
			t.Errorf("a period of %v must choose %s, got=%s", c.d, c.want, got)
		}
	}
}

func TestRotateWriter_SwitchesFileAcrossPeriods(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	w.Write([]byte("first minute\n"))
	now = now.Add(time.Minute)
	w.Write([]byte("second minute\n"))

	first := base + ".202609181030"
	second := base + ".202609181031"
	if got := readFile(t, first); got != "first minute\n" {
		t.Errorf("the contents of %s are wrong, got=%q", first, got)
	}
	if got := readFile(t, second); got != "second minute\n" {
		t.Errorf("the contents of %s are wrong, got=%q", second, got)
	}
}

func TestRotateWriter_SymlinkFollowsCurrentFile(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	if got, err := os.Readlink(base); err != nil || got != "app.log.202609181030" {
		t.Fatalf("the link must point at the current file, got=%q err=%v", got, err)
	}
	now = now.Add(time.Minute)
	w.Write([]byte("x\n"))
	if got, _ := os.Readlink(base); got != "app.log.202609181031" {
		t.Errorf("after a rotation the link must point at the new file, got=%q", got)
	}
	// Reading through the link name gives the new file's contents.
	if got := readFile(t, base); got != "x\n" {
		t.Errorf("the contents read through the link are wrong, got=%q", got)
	}
}

func TestRotateWriter_PurgeRemovesExpiredFiles(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	stale := base + ".202609181010" // 20 minutes ago, long due for cleanup
	fresh := base + ".202609181029" // the previous minute, still within the retention
	current := base + ".202609181030"
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w.purge(current)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("an expired file must be deleted: %s", stale)
	}
	for _, p := range []string{fresh, current, base} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("something that must not be deleted was: %s (%v)", p, err)
		}
	}
}

func TestRotateWriter_PurgeKeepsSymlink(t *testing.T) {
	// While the link is being swapped a base.tmp exists briefly; it matches base.* and must not be deleted as a historical file.
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, time.Minute, &now)

	tmp := base + ".tmp"
	if err := os.Symlink("app.log.202609181030", tmp); err != nil {
		t.Skipf("symbolic links are unsupported in this environment: %v", err)
	}
	w.purge(base + ".202609181030")

	if _, err := os.Lstat(tmp); err != nil {
		t.Errorf("the temporary link must not be deleted: %v", err)
	}
}

func TestRotateWriter_PurgeSkipsWhenClosed(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now) // maxAge <= 0

	stale := base + ".202601010000"
	os.WriteFile(stale, []byte("x"), 0o644)
	w.purge(base + ".202609181030")

	if _, err := os.Stat(stale); err != nil {
		t.Errorf("maxAge<=0 means no cleanup, but the file is gone: %v", err)
	}
}

func TestRotateWriter_ExpiredByFileNameNotMtime(t *testing.T) {
	// A backup restore, an rsync and image layers all rewrite the mtime, and going by it would delete the wrong files or never clean up.
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	old := base + ".202609181000"
	os.WriteFile(old, []byte("x"), 0o644)
	os.Chtimes(old, now, now) // the mtime is just now while the file name says half an hour ago

	w.purge(base + ".202609181030")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an expired file name must be deleted and must not be rescued by a fresh mtime")
	}
}

func TestRotateWriter_PurgeOnlyDeletesOwnNamedFiles(t *testing.T) {
	// Not everything base.* matches is ours: an app.log.bak the caller backed up by hand and an app.log.1.gz another
	// tool compressed are in there too. An unparseable file name used to fall back to judging by mtime,
	// which deleted a month-old manual backup as an expired log.
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	var others []string
	for _, suffix := range []string{".bak", ".1.gz", ".not-a-time", ".2026091810301", ".tmp"} {
		p := base + suffix
		os.WriteFile(p, []byte("somebody else's"), 0o644)
		os.Chtimes(p, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))
		others = append(others, p)
	}

	// After RotateTime has been changed, the files named daily before it are still ours and are cleaned up when due,
	// rather than staying forever because the granularity changed.
	byDay := base + ".20260901"
	os.WriteFile(byDay, []byte("x"), 0o644)

	w.purge(base + ".202609181030")
	for _, p := range others {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a file this package did not name must not be deleted: %s (%v)", p, err)
		}
	}
	if _, err := os.Stat(byDay); !os.IsNotExist(err) {
		t.Errorf("an expired log named at another granularity is still ours and must be cleaned up: %s", byDay)
	}
}

func TestRotateWriter_PurgeUsesOldGranularityPeriodForExpiry(t *testing.T) {
	// After switching from daily to hourly rotation, today's daily file was still being written to until the restart.
	// Computed with the current one-hour period it would be "expired" just after midnight -- while holding logs from minutes ago.
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
		t.Errorf("today's daily file is not past its retention and must not be deleted: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a daily file from three days ago is long expired and must be deleted: %s", old)
	}
}

func TestRotateWriter_PurgesExpiredFilesOnOpen(t *testing.T) {
	// With cleanup hanging on rotation alone, a service rotating daily restarts into the middle of a period on every
	// deploy, and restarting several times a day could leave expired files waiting for a rotation that never comes.
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
		t.Errorf("expired files must be cleaned up on open: %s", stale)
	}
}

func TestRotateWriter_RegularFileNotOverwrittenBySymlink(t *testing.T) {
	// What goes at {Path}/{Name} is a symbolic link to the current file. When a regular file is already there
	// (an older version writing app.log directly, or one another logging library left behind), Rename would replace it
	// atomically -- and the old logs inside would vanish without a word.
	dir := t.TempDir()
	base := filepath.Join(dir, "app.log")
	if err := os.WriteFile(base, []byte("old logs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := newRotateWriter(base, 0, 24*time.Hour, 0)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Errorf("startup must be refused with a clear reason, got=%v", err)
	}
	if got := readFile(t, base); got != "old logs\n" {
		t.Errorf("the existing file must not be touched, got=%q", got)
	}
}

func TestRotateWriter_ExpiredBoundary(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)
	cutoff := now.Add(-5 * time.Minute) // 10:25

	// The file for the 10:24 minute has its period end at 10:25, exactly equal to the cutoff -- it counts as expired.
	if !w.expired(base+".202609181024", cutoff) {
		t.Error("a period end equal to the cutoff must count as expired")
	}
	// The file for the 10:25 minute has its period end at 10:26 -- it does not count as expired.
	if w.expired(base+".202609181025", cutoff) {
		t.Error("a period end later than the cutoff must not count as expired")
	}
}

func TestRotateWriter_FallsBackToOldFileOnRotateFailure(t *testing.T) {
	// Returning the error on a failed rotation would make every log line fail from then on, surfacing only at Close --
	// what that looks like is the logs stopping one day with nothing reported.
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	// Put a directory at the next period's file name so OpenFile fails.
	if err := os.Mkdir(base+".202609181031", 0o755); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)

	n, err := w.Write([]byte("degraded\n"))
	if err != nil || n == 0 {
		t.Fatalf("a failed rotation must not fail the write, n=%d err=%v", n, err)
	}
	if got := readFile(t, base+".202609181030"); got != "degraded\n" {
		t.Errorf("it must keep writing the old file, got=%q", got)
	}
}

func TestRotateWriter_RotateFailureWarningsThrottled(t *testing.T) {
	// Once the period has passed, every log line retries the rotation, and without a limit the warnings would flood.
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, _ := newTestWriter(t, time.Minute, 0, &now)

	w.reportRotateFailure(errFake{})
	first := w.lastRotateErrAt
	if first.IsZero() {
		t.Fatal("the first failure must record the moment")
	}

	now = now.Add(rotateErrLogInterval / 2)
	w.reportRotateFailure(errFake{})
	if !w.lastRotateErrAt.Equal(first) {
		t.Error("a repeat failure within the interval must be suppressed")
	}

	now = now.Add(rotateErrLogInterval)
	w.reportRotateFailure(errFake{})
	if w.lastRotateErrAt.Equal(first) {
		t.Error("it must warn again once the interval has passed")
	}
}

func TestRotateWriter_CloseRejectsWritesAfterClose(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, _ := newTestWriter(t, time.Minute, 0, &now)

	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close must be safe to call again, got=%v", err)
	}
	if _, err := w.Write([]byte("x")); err != os.ErrClosed {
		t.Errorf("a write after closing must return ErrClosed, got=%v", err)
	}
}

func TestRotateWriter_FailsToConstructWhenFileCannotOpen(t *testing.T) {
	// Permission and path problems have to surface during initialization, not at the first log line.
	_, err := newRotateWriter(filepath.Join(t.TempDir(), "no-such-directory", "app.log"), 0, time.Hour, 0)
	if err == nil {
		t.Fatal("construction must fail when the path does not exist")
	}
	if !strings.Contains(err.Error(), "xlog") {
		t.Errorf("the error must carry the module name, got=%v", err)
	}
}

func TestTruncateInLocation(t *testing.T) {
	// Daily rotation has to align to local midnight. time.Truncate works from the UTC epoch,
	// so using it directly in UTC+8 would put the cut at 8 in the morning local time.
	loc := time.FixedZone("UTC+8", 8*3600)
	got := truncateInLocation(time.Date(2026, 9, 18, 3, 15, 0, 0, loc), 24*time.Hour)
	want := time.Date(2026, 9, 18, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("truncating by day must land on local midnight, got=%v want=%v", got, want)
	}

	utc := time.Date(2026, 9, 18, 3, 15, 0, 0, time.UTC)
	if g := truncateInLocation(utc, time.Hour); !g.Equal(time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("truncating by hour in UTC is wrong, got=%v", g)
	}
	if g := truncateInLocation(utc, 0); !g.Equal(utc) {
		t.Errorf("a period <=0 must be returned unchanged, got=%v", g)
	}
}

func TestWarnfWritesToStderr(t *testing.T) {
	// The rotator is the logging system's underlying writer, so a failure of its own cannot go back through the logging system or it would form a cycle.
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	warnf("something went wrong file=[%s]", "a.log")
	os.Stderr = old
	f.Close()

	if got := readFile(t, f.Name()); !strings.Contains(got, "xlog: something went wrong file=[a.log]") {
		t.Errorf("the stderr contents are wrong, got=%q", got)
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake" }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s failed: %v", p, err)
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
		t.Errorf("without linkName configured no symbolic link may be created, got err=%v", err)
	}
}

func TestRotateWriter_KeepsWritingWhenSymlinkCannotBeCreated(t *testing.T) {
	// Symbolic links are unsupported on some platforms and file systems, and that must not stop the logs from being written at all.
	now := time.Now()
	w, _ := newTestWriter(t, time.Hour, 0, &now)
	// Point at a directory that does not exist, so Symlink necessarily fails.
	w.linkName = filepath.Join(t.TempDir(), "nope", "app.log")

	now = now.Add(time.Hour) // cross a period so this write triggers a rotation
	if _, err := w.Write([]byte("still writing\n")); err != nil {
		t.Fatalf("the logs must still be written: %v", err)
	}

	next := w.filenameFor(now)
	b, err := os.ReadFile(next)
	if err != nil || !strings.Contains(string(b), "still writing") {
		t.Errorf("the contents did not land in the new file, err=%v, got=%q", err, b)
	}
}

func TestRotateWriter_DoesNotOverwriteOccupiedLinkPathAtRuntime(t *testing.T) {
	// Somebody replaced the link with a regular file after startup (a manual mv, a copytruncate-style tool).
	// Renaming over it on rotation would swallow that file; the temporary link must not be left behind either,
	// or app.log.tmp files would pile up in the directory.
	now := time.Now()
	w, base := newTestWriter(t, time.Hour, 0, &now)

	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("somebody else's\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.updateSymlink(w.filenameFor(now))
	if got := readFile(t, base); got != "somebody else's\n" {
		t.Errorf("a file occupying the link position must not be overwritten, got=%q", got)
	}
	if _, err := os.Lstat(base + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("no temporary link may be left behind, got err=%v", err)
	}
}

func TestRotateWriter_NoPurgeWithoutMaxAge(t *testing.T) {
	// 0 means "keep them forever", not "delete them all".
	now := time.Now()
	w, base := newTestWriter(t, time.Hour, 0, &now)

	old := base + ".2000010100"
	if err := os.WriteFile(old, []byte("ancient"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.purge(w.currentName)

	if _, err := os.Stat(old); err != nil {
		t.Errorf("with MaxAge 0 nothing may be deleted, got err=%v", err)
	}
}
