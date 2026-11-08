//go:build windows

package serve

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// daemonSysProcAttr starts the daemon on a console of its own with no window,
// so it outlives the client, and Ctrl+C in the client's console does not reach
// it (the same attributes as upstream's cmd/tuios).
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
}

// pidAlive is false on Windows: the migration's live check dials the socket.
func pidAlive(int) bool { return false }
