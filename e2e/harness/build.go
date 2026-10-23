package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// build compiles the five binaries e2e uses in one go build, once per test process:
//
//	service       the service under test
//	service-beego the beego application served by xbeego, see service-beego/main.go
//	service-echo  the echo application served by xecho, see service-echo/main.go
//	baseline      the bare gin service to compare against (for the load tests)
//	covapp        a minimal program shaped like a one-off job (for when the configuration is read, see covapp/main.go)
var build struct {
	once sync.Once
	dir  string
	out  []byte
	err  error
}

// ServiceBinary is the executable built from e2e/service
func ServiceBinary(t testing.TB) string { return binary(t, "service") }

// BeegoBinary is the executable built from e2e/service-beego
func BeegoBinary(t testing.TB) string { return binary(t, "service-beego") }

// EchoBinary is the executable built from e2e/service-echo
func EchoBinary(t testing.TB) string { return binary(t, "service-echo") }

// BaselineBinary is the executable built from e2e/baseline
func BaselineBinary(t testing.TB) string { return binary(t, "baseline") }

// CovAppBinary is the executable built from e2e/covapp, which explains itself there
func CovAppBinary(t testing.TB) string { return binary(t, "covapp") }

// RaceBuild says whether this round's binaries were built with -race, which they are everywhere
// but under load (XONE_E2E_LOAD=1). A process built with -race is several times slower and holds
// several times the memory, so a case asserting an absolute number loosens it or skips
func RaceBuild() bool { return os.Getenv("XONE_E2E_LOAD") != "1" }

func binary(t testing.TB, name string) string {
	t.Helper()
	Require(t)
	build.once.Do(func() {
		build.dir, build.err = os.MkdirTemp("", "xone-e2e-bin-")
		if build.err != nil {
			return
		}
		// GOWORK=off, as in scripts/test.sh: what is tested are the dependencies e2e/go.mod resolves
		// on its own.
		// A load test wants production numbers and goes without -race; everything else carries it:
		// a data race under real processes, real concurrency and real signals is one the unit tests
		// never meet, and checkOutput looks for a race report in stderr when the process exits
		args := []string{"build"}
		if RaceBuild() {
			args = append(args, "-race")
		}
		args = append(args, "-o", build.dir+string(filepath.Separator), "./service", "./service-beego", "./service-echo", "./baseline", "./covapp")
		cmd := exec.Command("go", args...)
		cmd.Dir = ModuleDir()
		cmd.Env = append(os.Environ(), "GOWORK=off")
		build.out, build.err = cmd.CombinedOutput()
	})
	if build.err != nil {
		t.Fatalf("build e2e binaries: %v\n%s", build.err, build.out)
	}
	return filepath.Join(build.dir, name)
}

func removeBinaries() {
	if build.dir != "" {
		os.RemoveAll(build.dir)
	}
}
