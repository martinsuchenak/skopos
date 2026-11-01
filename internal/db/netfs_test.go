package db

import (
	"path/filepath"
	"testing"
)

// A local temp directory is not a network filesystem; a missing path is
// "can't tell", never a false warning.
func TestNetworkFilesystemLocal(t *testing.T) {
	if fs := NetworkFilesystem(filepath.Join(t.TempDir(), "skopos.db")); fs != "" {
		t.Fatalf("a local temp dir reported as %q", fs)
	}
	if fs := NetworkFilesystem("/no/such/dir/skopos.db"); fs != "" {
		t.Fatalf("a missing dir under a local root reported as %q", fs)
	}
	if fs := NetworkFilesystem(filepath.Join(t.TempDir(), "not", "created", "yet", "index.db")); fs != "" {
		t.Fatalf("a not-yet-created dir on local disk reported as %q", fs)
	}
}
