package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// build e2e 用到的三个二进制一次 go build 编完，整个测试进程只编一次：
//
//	service   被测服务
//	baseline  裸 gin 的对照服务（压测用）
//	covapp    一次性任务形状的最小程序（测配置加载时机，见 covapp/main.go）
var build struct {
	once sync.Once
	dir  string
	out  []byte
	err  error
}

// ServiceBinary e2e/service 编出来的可执行文件
func ServiceBinary(t testing.TB) string { return binary(t, "service") }

// BaselineBinary e2e/baseline 编出来的可执行文件
func BaselineBinary(t testing.TB) string { return binary(t, "baseline") }

// CovAppBinary e2e/covapp 编出来的可执行文件，见那里的说明
func CovAppBinary(t testing.TB) string { return binary(t, "covapp") }

// RaceBuild 这一轮的二进制是不是带 -race 编的：压测（XONE_E2E_LOAD=1）之外都带。
// 带 -race 的进程慢几倍、内存大几倍，按绝对数字断言性能的用例据此放宽或跳过
func RaceBuild() bool { return os.Getenv("XONE_E2E_LOAD") != "1" }

func binary(t testing.TB, name string) string {
	t.Helper()
	Require(t)
	build.once.Do(func() {
		build.dir, build.err = os.MkdirTemp("", "xone-e2e-bin-")
		if build.err != nil {
			return
		}
		// 和 scripts/test.sh 一样 GOWORK=off：测的是 e2e/go.mod 自己解出来的依赖。
		// 压测要的是生产形态的数字，不带 -race；其余一律带：真进程、真并发、真信号下的
		// 数据竞争单元测试碰不到，进程退出时 checkOutput 查 stderr 里有没有竞争报告
		args := []string{"build"}
		if RaceBuild() {
			args = append(args, "-race")
		}
		args = append(args, "-o", build.dir+string(filepath.Separator), "./service", "./baseline", "./covapp")
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
