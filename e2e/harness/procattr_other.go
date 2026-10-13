//go:build !linux

package harness

import "syscall"

// sysProcAttr 只有 Linux 有 Pdeathsig；别的系统上只是要能编译过——
// scripts/test.sh 在那里也会编这个模块，e2e 本身只在 Linux 上跑
func sysProcAttr() *syscall.SysProcAttr { return nil }
