package e2e

import (
	"os"
	"testing"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// TestMain 跑完全部测试后删掉 harness 构建出来的二进制
func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }

// knownBug 测试揭示了框架的 bug 时，在失败的那个分支里调它：打印 KNOWN BUG 并跳过。
// 断言照文档写，不为了变绿去改；证据写进测试注释和汇报里
func knownBug(t *testing.T, msg string) {
	t.Helper()
	harness.KnownBug(t, msg)
}
