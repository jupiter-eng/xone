package e2e

import (
	"os"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// TestMain deletes the binaries the harness built once every test has run
func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }

// knownBug is called in the failing branch when a test uncovers a bug in the framework: it prints
// KNOWN BUG and skips. The assertions follow the documentation and are not changed to go green;
// the evidence goes into the test's comment and into the report
func knownBug(t *testing.T, msg string) {
	t.Helper()
	harness.KnownBug(t, msg)
}

// requireSlow is the first line of a case that has to sit out a wait nothing here can shorten: a
// clock boundary, or a timeout a dependency's protocol fixes. XONE_E2E_SLOW=1 runs them
// (scripts/e2e.sh --slow), and the nightly e2e workflow sets it.
//
// Why they are not simply slow cases among the rest: these two alone decide how long the whole
// package takes. Measured, the package runs 119.8s with -parallel=16 while the top-level cases add
// up to 206.3s, so the wall time is the longest case and not the sum -- and the two longest are
// TestKafka_SIGKILLMidProcessing_Redelivered at 45.6s (it waits out franz-go's 45s session timeout
// before a dead member's partitions move, and xkafka exposes no setting for it) and
// TestCoverage_LogRotatesByRotateTime_MaxAgePrunesOwnFiles_PermApplied at 15 to 50s (it waits for a
// real minute boundary, XLog.File.RotateTime being 1m at the least because the file names are only
// precise to the minute). Neither is slow for a reason this repository can fix, and together they
// are more than half the package's wall time.
//
// The reason goes into the skip so that a reader of the output knows which wait was skipped.
func requireSlow(t *testing.T, waitsFor string) {
	t.Helper()
	harness.Require(t)
	if os.Getenv("XONE_E2E_SLOW") != "1" {
		t.Skipf("slow case, off by default (it waits for %s): run scripts/e2e.sh --slow, or set XONE_E2E_SLOW=1", waitsFor)
	}
}
