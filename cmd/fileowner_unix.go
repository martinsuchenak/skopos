//go:build unix

package cmd

import (
	"os"
	"syscall"
)

// otherOwner says, for a check run as root, whether path belongs to another
// user (and which uid): the worker runs as that user, not as root.
func otherOwner(path string) (uid int, other bool) {
	if os.Geteuid() != 0 {
		return 0, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return 0, false
	}
	return int(st.Uid), true
}
