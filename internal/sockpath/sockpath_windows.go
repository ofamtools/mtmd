//go:build windows

package sockpath

import (
	"os"
	"path/filepath"

	"ofam.dev/mtmd/internal/hostnames"
	"ofam.dev/mtmd/internal/serverscope"
)

// RuntimeDir is "" on Windows: the daemon's socket lives under LOCALAPPDATA,
// and nothing there uses a runtime folder.
func RuntimeDir() string { return "" }

// SetRootsForTest does nothing on Windows, which has neither root.
func SetRootsForTest(runUser, tmp string) (restore func()) { return func() {} }

// DirFor is the socket folder under the names of n: LOCALAPPDATA's
// SocketApp folder, and the named server's folder inside it.
func DirFor(n hostnames.Names) string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
	}
	return serverscope.SocketDir(filepath.Join(base, n.SocketApp))
}
