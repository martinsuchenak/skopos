package db

import (
	"os"
	"path/filepath"
)

// NetworkFilesystem names the network filesystem holding path's directory
// ("nfs", "smb", "fuse", …), or "" for a local one or when it cannot tell.
// A directory that doesn't exist yet (an index dir before its first index)
// is judged by its nearest existing parent. SQLite over a network
// filesystem is slow (every commit is a network lock and flush) and its WAL
// mode is unsafe there (the -shm shared memory and the locks do not work
// across hosts), so serve warns at startup. FUSE is reported too: it backs
// SeaweedFS, GlusterFS and sshfs mounts, though a FUSE mount can be local.
func NetworkFilesystem(path string) string {
	dir := filepath.Dir(path)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return networkFS(dir)
}
