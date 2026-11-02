//go:build !unix

package worker

import (
	"errors"
	"os"
)

// branchCheckoutSupported: the clone lock needs flock.
const branchCheckoutSupported = false

func tryLockFile(string) (*os.File, bool, error) {
	return nil, false, errors.New("branch checkout needs a unix host")
}

func unlockFile(*os.File) {}
