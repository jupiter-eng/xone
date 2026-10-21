package harness

import "syscall"

// sysProcAttr has a child process die with the test process (on a timeout, a panic or a Ctrl+C),
// so that no orphan is left holding a port
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
