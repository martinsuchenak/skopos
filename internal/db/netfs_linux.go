package db

import "syscall"

// Filesystem magic numbers (statfs f_type) of network filesystems.
var networkMagic = map[int64]string{
	0x6969:     "nfs",
	0x517b:     "smb",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x00c36400: "ceph",
	0x01021997: "9p",
	0x013111a8: "ibrix",
	0x6b414653: "afs",
	0x65735546: "fuse", // SeaweedFS, GlusterFS, sshfs, … (or a local FUSE mount)
}

func networkFS(dir string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return ""
	}
	return networkMagic[int64(st.Type)&0xffffffff]
}
