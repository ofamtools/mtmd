//go:build !windows

package serve

import "syscall"

// daemonSysProcAttr detaches the spawned daemon from this process group so it
// outlives the client that started it.
func daemonSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// pidAlive reports whether a process with this pid exists (signal 0).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
