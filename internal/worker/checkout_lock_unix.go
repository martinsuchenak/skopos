//go:build unix

package worker

import (
	"errors"
	"os"
	"syscall"
)

// branchCheckoutSupported: the clone lock needs flock.
const branchCheckoutSupported = true

// tryLockFile takes an exclusive flock on path without waiting. The lock
// goes with the process, so a killed worker never leaves it held.
func tryLockFile(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
