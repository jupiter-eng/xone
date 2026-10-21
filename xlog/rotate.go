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

// The time suffix formats of a log file name, chosen by rotation period for the right granularity.
// A period shorter than a day needs a finer granularity, or different periods would produce the same file name and nothing would really rotate.
const (
	rotateLayoutDay    = "20060102"
	rotateLayoutHour   = "2006010215"
	rotateLayoutMinute = "200601021504"
)

// rotateLayouts holds the three suffix formats with their standard periods, from coarse to fine.
//
// Choosing a format (rotateLayoutFor) and cleaning up (expired) share this one table. Cleanup accepts all three:
// after RotateTime is changed, the files named at the old granularity are still ours and must not stay forever
// just because the granularity changed. The three have different lengths (8 / 10 / 12 characters), so one suffix
// can parse successfully under at most one of them.
var rotateLayouts = []struct {
	layout string
	period time.Duration // this format is chosen when the period is no shorter than this
}{
	{rotateLayoutDay, 24 * time.Hour},
	{rotateLayoutHour, time.Hour},
	{rotateLayoutMinute, time.Minute},
}

// defaultLogFilePerm is the default log file permission.
const defaultLogFilePerm = 0o644

// rotateErrLogInterval is the minimum interval between rotation failure warnings.
const rotateErrLogInterval = time.Minute

// rotateWriter is a log file writer that rotates by time.
//
// The file names look like {base}.20260912, with a symbolic link {base} kept pointing at the current file
// so tools such as tail always follow the latest log.
//
// slog's handler calls Write from any number of goroutines concurrently, and Close may come from another goroutine too, hence the lock.
// There is no buffering: every log line is written straight to the file, so there is nothing to flush on close.
type rotateWriter struct {
	base     string        // the file name prefix, such as /var/log/app.log
	linkName string        // the symbolic link path; empty means none is created
	layout   string        // the time suffix format
	maxAge   time.Duration // historical files older than this are cleaned up; <=0 means no cleanup
	rotate   time.Duration // the rotation period

	// clock is an injectable time source, which makes rotation and cleanup testable
	clock func() time.Time

	// perm is the log file permission
	perm os.FileMode

	mu          sync.Mutex
	file        *os.File // nil after Close
	currentName string

	// lastRotateErrAt is when a rotation failure was last recorded, for warning noise reduction
	lastRotateErrAt time.Time
}

// newRotateWriter creates the rotating writer, opens the current file immediately and cleans up expired files once.
//
// Opening early surfaces permission and path problems during initialization rather than at the first log line.
// The cleanup also has to happen here and not only on rotation: a service rotating daily restarts into the middle of
// a period on every deploy, and restarting several times a day could leave expired files waiting for a rotation that never comes.
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
	// What goes at {base} is a symbolic link. When something else is already there (an app.log an older version wrote
	// directly, or one another logging library left behind), Rename would replace it atomically and the logs inside
	// would vanish without a word. Moving it away is not right either -- where to and under what name are decisions
	// taken on the caller's behalf without their knowledge. So startup is refused and the choice handed back to them.
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

// occupied reports whether something is at the path and is not a symbolic link.
func occupied(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink == 0
}

// rotateLayoutFor picks the time suffix granularity by rotation period: the coarsest one the period reaches.
func rotateLayoutFor(d time.Duration) string {
	for _, l := range rotateLayouts {
		if d >= l.period {
			return l.layout
		}
	}
	return rotateLayoutMinute
}

// Write writes a log line, switching files first when a rotation period has been crossed.
func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, os.ErrClosed
	}

	if name := w.filenameFor(w.clock()); name != w.currentName {
		if err := w.rotateTo(name); err != nil {
			// A failed rotation must not lose logs: the old file handle is still usable, so it degrades to writing that.
			// Returning the error instead would send every log line down this path from the first failed rotation on,
			// and slog.Logger simply discards the error a handler returns --
			// what that looks like is the logs stopping one day with nothing reported.
			w.reportRotateFailure(err)
		} else {
			// Cleanup runs in the background so it does not block log writing; the current file name is passed as an argument to avoid taking the lock again.
			go w.purge(name)
		}
	}

	return w.file.Write(p)
}

// reportRotateFailure records a rotation failure.
// It is rate-limited by interval: once the rotation period has passed, every log line retries, and without a limit the warnings would flood.
func (w *rotateWriter) reportRotateFailure(err error) {
	now := w.clock()
	if w.lastRotateErrAt.IsZero() || now.Sub(w.lastRotateErrAt) >= rotateErrLogInterval {
		w.lastRotateErrAt = now
		warnf("rotate: failed, keep writing current file=[%s], err=[%v]", w.currentName, err)
	}
}

// Close closes the current log file and is safe to call several times.
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

// filenameFor computes the log file name of the period a given moment belongs to.
func (w *rotateWriter) filenameFor(t time.Time) string {
	return w.base + "." + truncateInLocation(t, w.rotate).Format(w.layout)
}

// rotateTo switches to the given file.
// The caller holds the lock; during construction the instance is not shared yet and no lock is needed.
//
// It returns a plain error: during construction newRotateWriter wraps it in an xerror, while a rotation failure at
// runtime only goes through warnf and never leaves this package.
func (w *rotateWriter) rotateTo(name string) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, w.perm)
	if err != nil {
		return fmt.Errorf("open log file failed, file=[%s]: %w", name, err)
	}

	if w.file != nil {
		// Failing to close the old file does not stop writing to the new one and is only recorded.
		if cerr := w.file.Close(); cerr != nil {
			warnf("rotate: close previous file failed, err=[%v]", cerr)
		}
	}
	w.file = f
	w.currentName = name

	w.updateSymlink(name)
	return nil
}

// updateSymlink points the symbolic link at the current file.
// Symbolic links are unsupported on some platforms and file systems, and a failure does not affect log writing.
func (w *rotateWriter) updateSymlink(target string) {
	if w.linkName == "" {
		return
	}
	// Construction already caught this once; what is guarded here is somebody replacing the link with a regular file
	// at runtime (a manual mv, a copytruncate-style tool), which a rotation would otherwise swallow by Renaming over it.
	if occupied(w.linkName) {
		warnf("rotate: %s is not a symlink, leaving it alone and not updating the link", w.linkName)
		return
	}

	// A temporary link is created first and then swapped atomically, so the link is never briefly missing.
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

// purge deletes historical log files older than maxAge, where current is the file being written.
//
// It only deletes files this package named ({base}.{time suffix}). Not everything base.* matches is ours:
// an app.log.bak the caller backed up by hand or an app.log.1.gz another tool compressed are in there too,
// and since no time parses out of them they are never touched -- however old their mtime is.
func (w *rotateWriter) purge(current string) {
	if w.maxAge <= 0 {
		return
	}

	matches, err := filepath.Glob(w.base + ".*")
	if err != nil {
		warnf("rotate: glob log files failed, err=[%v]", err)
		return
	}

	// filepath.Glob returns its results sorted, so the deletion order is deterministic.
	cutoff := w.clock().Add(-w.maxAge)
	for _, path := range matches {
		// Lstat rather than Stat: it must not follow the symbolic link and mistake the current file for a historical one.
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			continue // skip the symbolic link itself
		}
		if path == current {
			continue // do not delete the file being written
		}
		if !w.expired(path, cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			warnf("rotate: remove expired log failed, file=[%s], err=[%v]", path, err)
		}
	}
}

// expired reports whether a historical log file is past its retention; anything not named by this package is always false.
//
// It uses the time suffix in the file name rather than the mtime: a backup restore, an rsync and container image
// layers all rewrite the mtime, and going by that could take yesterday's log for a fresh one and never clean it,
// or take a file just rotated out for an expired one and delete it.
func (w *rotateWriter) expired(path string, cutoff time.Time) bool {
	suffix := strings.TrimPrefix(path, w.base+".")
	for _, l := range rotateLayouts {
		t, err := time.ParseInLocation(l.layout, suffix, w.clock().Location())
		if err != nil {
			continue
		}
		// The file name records the start of the period it belongs to, and it is only expired once that whole period has
		// ended. One named at the current granularity has RotateTime as its period; one named at another granularity is
		// left over from before the configuration changed and uses that granularity's standard period. Computing them
		// all with the current RotateTime would make today's daily file "expired" at midnight after switching from
		// daily to hourly -- while it was still being written to right up until this restart.
		period := l.period
		if l.layout == w.layout {
			period = w.rotate
		}
		return !t.Add(period).After(cutoff)
	}
	return false
}

// truncateInLocation truncates a time by period while aligning to the local time zone.
//
// time.Truncate works from the UTC epoch, so using it for daily rotation would put the cut at a non-midnight moment
// in local time. This moves the local time's fields onto UTC as they are, truncates there, and moves them back,
// so daily rotation aligns to local midnight (which matches the behavior of the file-rotatelogs it replaced).
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

// warnf writes a failure of the logging system itself straight to stderr.
//
// It cannot go through the logging system: that is what failed, and calling it again would form a cycle.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "xlog: "+format+"\n", args...)
}
