package db

import "syscall"

var networkTypes = map[string]bool{"nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "cifs": true, "macfuse": true, "osxfuse": true}

func networkFS(dir string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return ""
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	if name := string(b); networkTypes[name] {
		return name
	}
	return ""
}
