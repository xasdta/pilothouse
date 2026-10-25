package daemon

import "syscall"

// awakeSysProcAttr: own process group (Stop kills all of it), and SIGTERM
// when the daemon dies, so the inhibitor lock goes with it at once.
func awakeSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
