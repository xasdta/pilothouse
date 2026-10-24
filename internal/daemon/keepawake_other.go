//go:build !linux

package daemon

import "syscall"

// awakeSysProcAttr: own process group, so Stop kills all of it.
// (caffeinate -w ends it when the daemon dies.)
func awakeSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
