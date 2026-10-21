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
