package harness

import "syscall"

// sysProcAttr 测试进程死了（超时、panic、被 Ctrl+C）子进程跟着死，不留下占着端口的孤儿
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
