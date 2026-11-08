//go:build !windows

package serve

import "syscall"

// daemonSysProcAttr detaches the spawned daemon from this process group so it
// outlives the client that started it.
func daemonSysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
