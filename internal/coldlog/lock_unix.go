//go:build unix

package coldlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ErrLocked: another writer (this process or another) owns the log.
var ErrLocked = errors.New("coldlog: log is locked by another writer")

const lockName = "LOCK"

type dirLock struct{ f *os.File }

// lockDir takes the sole-writer lock: an flock on LOCK (released by the kernel
// when the process dies, so a crash never leaves a stale lock) holding the
// owner's pid for people.
func lockDir(dir string) (*dirLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("coldlog: lock: %w", err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return &dirLock{f}, nil
}

func (l *dirLock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// Locked reports whether a writer holds dir's lock right now.
func Locked(dir string) bool {
	l, err := lockDir(dir)
	if err != nil {
		return errors.Is(err, ErrLocked)
	}
	// We only probed: put the file back the way an unlocked log has it.
	l.release()
	return false
}
