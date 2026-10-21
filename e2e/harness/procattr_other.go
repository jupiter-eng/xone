//go:build !linux

package harness

import "syscall"

// sysProcAttr: Pdeathsig is Linux-only, and on other systems this only has to compile --
// scripts/test.sh builds this module there as well, while e2e itself runs on Linux alone
func sysProcAttr() *syscall.SysProcAttr { return nil }
