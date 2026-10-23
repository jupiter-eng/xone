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
// Why such a case is not simply a slow one among the rest: the package's wall time is its longest
// case and not the sum of them. Measured, it ran 119.8s with -parallel=16 while the top-level cases
// added up to 206.3s, and two cases alone set that 119.8s.
//
// One of the two has since been taken off this gate rather than left behind it:
// TestKafka_SIGKILLMidProcessing_Redelivered waited out franz-go's 45s session timeout because
// xkafka exposed no setting for it, and now that XKafka.Consumer.SessionTimeout exists
// service/application-kafka.yml writes 6s -- as low as the broker allows -- and the case is 17s and
// runs by default. A wait that can be configured away is not a slow case, it is a missing setting.
//
// What is left here is TestCoverage_LogRotatesByRotateTime_MaxAgePrunesOwnFiles_PermApplied, 15 to
// 50s depending on where in the minute it starts: it waits for a real minute boundary, and
// XLog.File.RotateTime is 1m at the least because the file names are only precise to the minute.
// Nothing here can shorten that one without changing what the file names mean.
//
// The reason goes into the skip so that a reader of the output knows which wait was skipped.
func requireSlow(t *testing.T, waitsFor string) {
	t.Helper()
	harness.Require(t)
	if os.Getenv("XONE_E2E_SLOW") != "1" {
		t.Skipf("slow case, off by default (it waits for %s): run scripts/e2e.sh --slow, or set XONE_E2E_SLOW=1", waitsFor)
	}
}
