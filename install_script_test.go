package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// install.sh, run for real against a file:// mirror of the release layout
// (curl reads those): it installs the release's binary for this machine,
// refuses one that doesn't match checksums.txt, and replaces an installed
// one when run again.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for Linux and macOS")
	}
	// cosign would want a real signature: leave its directory off PATH.
	path := os.Getenv("PATH")
	if c, err := exec.LookPath("cosign"); err == nil {
		var keep []string
		for _, d := range filepath.SplitList(path) {
			if d != filepath.Dir(c) {
				keep = append(keep, d)
			}
		}
		path = strings.Join(keep, string(os.PathListSeparator))
	}
	t.Setenv("PATH", path)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("needs curl")
	}
	asset := fmt.Sprintf("skopos_%s_%s", runtime.GOOS, runtime.GOARCH)
	mirror := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	release := func(version string, tamper bool) {
		t.Helper()
		dir := filepath.Join(mirror, "v"+version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// --version answers; anything else (worker init/check/install) is
		// logged, one call per line, and succeeds unless CHECK_FAILS is set.
		bin := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo skopos version " + version + " '(test)'; exit 0; fi\n" +
			"echo \"$*\" >> " + calls + "\n[ \"$2\" = check ] && [ -n \"$CHECK_FAILS\" ] && exit 1\nexit 0\n")
		sum := sha256.Sum256(bin)
		if tamper {
			bin = []byte("#!/bin/sh\necho evil " + version + "\n")
		}
		if err := os.WriteFile(filepath.Join(dir, asset), bin, 0o644); err != nil {
			t.Fatal(err)
		}
		sums := fmt.Sprintf("%s  skopos_%s_linux_amd64.tar.gz\n%s  %s\n", strings.Repeat("0", 64), version, hex.EncodeToString(sum[:]), asset)
		if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	release("9.1.0", false)
	release("9.2.0", false)
	release("9.3.0", true)

	dest := t.TempDir()
	run := func(args ...string) (string, error) {
		cmd := exec.Command("sh", append([]string{"install.sh"}, args...)...)
		cmd.Env = append(os.Environ(), "SKOPOS_DOWNLOAD_URL=file://"+mirror, "SKOPOS_INSTALL_DIR="+dest, "HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	version := func() string {
		out, err := exec.Command(filepath.Join(dest, "skopos"), "--version").CombinedOutput()
		if err != nil {
			t.Fatalf("installed binary: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	out, err := run("--version", "9.1.0")
	if err != nil || !strings.Contains(out, "Checksum: verified") || !strings.Contains(version(), "9.1.0") {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "isn't on your PATH") {
		t.Errorf("a directory not on PATH gets a hint:\n%s", out)
	}

	// Run again: the upgrade.
	if out, err := run("--version=v9.2.0"); err != nil || !strings.Contains(version(), "9.2.0") {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}

	// A binary that doesn't match its checksum: refused, the installed one stays.
	if out, err := run("--version", "9.3.0"); err == nil || !strings.Contains(out, "doesn't match its checksum") {
		t.Fatalf("tampered: %v\n%s", err, out)
	}
	if !strings.Contains(version(), "9.2.0") {
		t.Fatal("a refused download must leave the installed binary alone")
	}

	// No such release.
	if out, err := run("--version", "9.9.9"); err == nil || !strings.Contains(out, "no checksums.txt") {
		t.Fatalf("missing release: %v\n%s", err, out)
	}
	if left, _ := filepath.Glob(filepath.Join(dest, ".skopos-install.*")); len(left) != 0 {
		t.Fatalf("no temporary file may be left: %v", left)
	}

	// --worker: the install, then worker init with the flags (quoted as
	// given), worker check, and worker install when the check passes.
	home := t.TempDir()
	cfg := filepath.Join(home, ".config", "skopos", "skopos-config.toml")
	worker := func(env []string, args ...string) (string, []string) {
		t.Helper()
		_ = os.Remove(calls)
		cmd := exec.Command("sh", append([]string{"install.sh", "--version", "9.2.0"}, args...)...)
		cmd.Env = append(append(os.Environ(), "SKOPOS_DOWNLOAD_URL=file://"+mirror, "SKOPOS_INSTALL_DIR="+dest, "HOME="+home), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("install --worker: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(calls)
		return string(out), strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	out, got := worker(nil, "--worker", "--server-url", "https://skopos.example.com", "--workspace", "github.com/you/repo",
		"--repo", "/srv/it's here", "--clone")
	want := []string{
		"worker init --config " + cfg + " --server-url https://skopos.example.com --workspace github.com/you/repo --repo /srv/it's here --clone",
		"worker check --config " + cfg,
		"worker install --config " + cfg,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s\noutput:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), out)
	}

	// A config with a [worker] section is kept; a failing check stops before
	// the service and says what's next.
	if err := os.WriteFile(cfg, []byte("[worker]\nid = \"worker-x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, got = worker([]string{"CHECK_FAILS=1"}, "--worker")
	if strings.Join(got, "\n") != "worker check --config "+cfg || !strings.Contains(out, "already has a [worker] section") ||
		!strings.Contains(out, "The worker isn't ready yet") {
		t.Fatalf("calls %v\n%s", got, out)
	}
}
