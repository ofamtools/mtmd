//go:build !unix

package coldlog

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// ErrLocked: another writer owns the log.
var ErrLocked = errors.New("coldlog: log is locked by another writer")

const lockName = "LOCK"

type dirLock struct{ path string }

// lockDir creates LOCK exclusively with the pid. Without flock a crash leaves
// the file behind; the log stays locked until it is removed by hand.
func lockDir(dir string) (*dirLock, error) {
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrLocked
		}
		return nil, err
	}
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	f.Close()
	return &dirLock{p}, nil
}

func (l *dirLock) release() { _ = os.Remove(l.path) }

// Locked reports whether a writer holds dir's lock.
func Locked(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, lockName))
	return err == nil
}
