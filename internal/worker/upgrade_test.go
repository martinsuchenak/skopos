//go:build unix

package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/build"
)

func TestNewerRelease(t *testing.T) {
	for _, c := range []struct {
		cur, target string
		want        bool
	}{
		{"0.19.0", "0.19.1", true},
		{"0.19.0", "v0.20.0", true},
		{"0.9.9", "0.10.0", true},
		{"0.19.1", "0.19.1", false},
		{"0.19.1", "0.19.0", false}, // never a downgrade
		{"dev", "0.19.1", false},    // a dev build stays
		{"0.19.0", "0.20.0-rc1", false},
		{"0.19.0", "", false},
	} {
		if got := newerRelease(c.cur, c.target); got != c.want {
			t.Errorf("newerRelease(%q, %q) = %v, want %v", c.cur, c.target, got, c.want)
		}
	}
}

// An idle worker behind the server downloads the release's binary,
// verifies its checksum and that it runs as that version, and renames it
// over its own executable; a checksum mismatch leaves the binary alone and
// waits before trying again; auto_upgrade = false never upgrades.
func TestSelfUpgrade(t *testing.T) {
	asset := fmt.Sprintf("skopos_%s_%s", runtime.GOOS, runtime.GOARCH)
	newBin := []byte("#!/bin/sh\necho skopos version 0.20.0 '(test)'\n")
	sum := sha256.Sum256(newBin)
	checksum := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0.20.0/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  skopos_0.20.0_linux_amd64.tar.gz\n%s  %s\n", checksum, checksum, asset)
		case "/v0.20.0/" + asset:
			_, _ = w.Write(newBin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldVersion := build.Version
	build.Version = "0.19.1"
	defer func() { build.Version = oldVersion }()

	dir := t.TempDir()
	exe := filepath.Join(dir, "skopos")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "skopos-link") // the service runs a symlink
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	newWorker := func(url string) *Worker {
		client := NewClient("http://unused", "k")
		client.serverVersion = "0.20.0"
		w := New(&Config{UpgradeURL: url}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
		w.exePath = func() (string, error) { return link, nil }
		return w
	}

	w := newWorker(srv.URL)
	off := false
	w.cfg.AutoUpgrade = &off
	if w.maybeUpgrade(t.Context()) {
		t.Fatal("auto_upgrade = false must not upgrade")
	}

	// A tampered binary: refused, the old one stays, no retry at once.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0.20.0/checksums.txt" {
			_, _ = fmt.Fprintf(w, "%s  %s\n", checksum, asset)
			return
		}
		_, _ = w.Write([]byte("#!/bin/sh\necho evil 0.20.0\n"))
	}))
	defer bad.Close()
	w = newWorker(bad.URL)
	if w.maybeUpgrade(t.Context()) || w.upgradeTried.IsZero() {
		t.Fatal("a checksum mismatch must fail and wait")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("the binary must stay as it was: %q", b)
	}
	w.cfg.UpgradeURL = srv.URL
	if w.maybeUpgrade(t.Context()) {
		t.Fatal("a failed upgrade waits upgradeRetry before trying again")
	}

	w = newWorker(srv.URL)
	if !w.maybeUpgrade(t.Context()) {
		t.Fatal("a newer server release must upgrade")
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatalf("the symlink's target must be replaced: %q", b)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink must stay a symlink")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".skopos-upgrade-*")); len(left) != 0 {
		t.Fatalf("no temporary file may be left: %v", left)
	}
}

// A Homebrew install is upgraded by its brew, never by swapping the file in
// the Caskroom behind brew's back; the service's link must then name the
// server's version.
func TestSelfUpgradeHomebrew(t *testing.T) {
	oldVersion := build.Version
	build.Version = "0.19.1"
	defer func() { build.Version = oldVersion }()

	prefix, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is a link to /private/var
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cask := func(v string) string { return filepath.Join(prefix, "Caskroom", "skopos", v, "skopos") }
	writeExe := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExe(cask("0.19.1"), "#!/bin/sh\necho skopos version 0.19.1\n")
	link := filepath.Join(prefix, "bin", "skopos")
	if err := os.Symlink(cask("0.19.1"), link); err != nil {
		t.Fatal(err)
	}
	// brew upgrade --cask skopos: installs the new version, relinks, drops the old.
	// $1 = the version the tap has (TAP_VERSION), so a lagging tap can be shown.
	argsLog := filepath.Join(t.TempDir(), "brew-args")
	writeExe(filepath.Join(prefix, "bin", "brew"), `#!/bin/sh
echo "$@" >> `+argsLog+`
[ "$1" = update ] && exit 0
[ "$TAP_VERSION" = 0.19.1 ] && { echo "skopos 0.19.1 already installed"; exit 0; }
mkdir -p `+filepath.Join(prefix, "Caskroom", "skopos", "0.20.0")+`
printf '#!/bin/sh\necho skopos version 0.20.0\n' > `+cask("0.20.0")+`
chmod 755 `+cask("0.20.0")+`
ln -sf `+cask("0.20.0")+` `+link+`
rm -rf `+filepath.Join(prefix, "Caskroom", "skopos", "0.19.1")+`
`)

	newWorker := func() *Worker {
		client := NewClient("http://unused", "k")
		client.serverVersion = "0.20.0"
		// No upgrade_url server: a download would fail the test.
		w := New(&Config{UpgradeURL: "http://127.0.0.1:1"}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
		w.exePath = func() (string, error) { return link, nil }
		return w
	}

	// The tap hasn't got the release yet: brew changes nothing, the check
	// fails, and it's tried again later.
	t.Setenv("TAP_VERSION", "0.19.1")
	w := newWorker()
	if w.maybeUpgrade(t.Context()) || w.upgradeTried.IsZero() {
		t.Fatal("a tap without the release must fail and wait")
	}

	t.Setenv("TAP_VERSION", "0.20.0")
	w = newWorker()
	if !w.maybeUpgrade(t.Context()) {
		t.Fatal("brew must upgrade the cask")
	}
	if target, _ := filepath.EvalSymlinks(link); target != cask("0.20.0") {
		t.Fatalf("the link must name the new version: %s", target)
	}
	if b, _ := os.ReadFile(argsLog); !strings.Contains(string(b), "update --quiet\nupgrade --cask skopos\n") {
		t.Fatalf("brew must update its taps, then upgrade: %s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(prefix, "Caskroom", "skopos", "*", ".skopos-upgrade-*")); len(left) != 0 {
		t.Fatalf("nothing may be written into the Caskroom: %v", left)
	}
}

func TestHomebrewOf(t *testing.T) {
	for _, c := range []struct {
		exe, prefix string
		cask, ok    bool
	}{
		{"/opt/homebrew/Caskroom/skopos/0.23.2/skopos", "/opt/homebrew", true, true},
		{"/home/linuxbrew/.linuxbrew/Caskroom/skopos/0.23.2/skopos", "/home/linuxbrew/.linuxbrew", true, true},
		{"/usr/local/Cellar/skopos/0.23.2/bin/skopos", "/usr/local", false, true},
		{"/usr/local/bin/skopos", "", false, false},
		{"/home/me/.local/bin/skopos", "", false, false},
	} {
		hb, ok := homebrewOf(c.exe)
		if ok != c.ok || hb.prefix != c.prefix || hb.cask != c.cask {
			t.Errorf("%s: %+v %v", c.exe, hb, ok)
		}
	}
}
