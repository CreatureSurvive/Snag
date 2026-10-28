//go:build unix

package server

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// LockDataDir takes an exclusive lock on the data directory for the life of the process, so a
// restore can't run under a live server (or two servers share one directory).
func LockDataDir(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".snag.lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another snag-server is using this data directory; stop it first")
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
