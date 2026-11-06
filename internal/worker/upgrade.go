package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/build"
)

// ErrRestartForUpgrade: the worker replaced its binary and must restart on
// it; the command exits with ExitUpgrade so the service manager does.
var ErrRestartForUpgrade = errors.New("upgraded: restarting on the new binary")

// ExitUpgrade is the exit code after a self-upgrade: non-zero, so systemd's
// Restart=on-failure restarts it (EX_TEMPFAIL).
const ExitUpgrade = 75

const (
	defaultUpgradeURL = "https://github.com/martinsuchenak/skopos/releases/download"
	// upgradeRetry: how long a failed upgrade waits before the next try.
	upgradeRetry = time.Hour
	// maxBinarySize bounds a download (today's binaries are ~50 MB).
	maxBinarySize = 300 << 20
)

// maybeUpgrade replaces the worker's binary with the server's release when
// the server is newer (tmp/self-healing-workers — design.md §5). Called
// only when idle: no run is cut off. It downloads the release's binary for
// this platform, checks it against the release's checksums.txt, makes sure
// it runs and names that version, and renames it over the running
// executable. true = replaced, restart. Every failure is logged and
// retried after upgradeRetry; it never stops the worker. Never a downgrade,
// never from or to a pre-release or a dev build, never when auto_upgrade
// is off.
func (w *Worker) maybeUpgrade(ctx context.Context) bool {
	if w.cfg.AutoUpgrade != nil && !*w.cfg.AutoUpgrade {
		return false
	}
	target := w.client.ServerVersion()
	if !newerRelease(build.Version, target) {
		return false
	}
	if !w.upgradeTried.IsZero() && time.Since(w.upgradeTried) < upgradeRetry {
		return false
	}
	log := w.log.With("from", build.Version, "to", target)
	log.Info("the server runs a newer release; upgrading")
	if err := w.upgradeTo(ctx, strings.TrimPrefix(target, "v")); err != nil {
		w.upgradeTried = time.Now()
		log.Warn("self-upgrade failed; trying again later", "err", err, "retry_in", upgradeRetry)
		return false
	}
	log.Info("upgraded")
	return true
}

func (w *Worker) upgradeTo(ctx context.Context, version string) error {
	exePath := w.exePath
	if exePath == nil {
		exePath = os.Executable
	}
	exe, err := exePath()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	asset := fmt.Sprintf("skopos_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	base := strings.TrimRight(w.cfg.UpgradeURL, "/")
	if base == "" {
		base = defaultUpgradeURL
	}
	base += "/v" + version + "/"
	sums, err := w.fetch(ctx, base+"checksums.txt", 1<<20)
	if err != nil {
		return err
	}
	want := checksumFor(sums, asset)
	if want == "" {
		return fmt.Errorf("checksums.txt of v%s lists no %s", version, asset)
	}
	bin, err := w.fetch(ctx, base+asset, maxBinarySize)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(bin); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s does not match its checksum", asset)
	}
	// Next to the binary, so the rename is atomic (same filesystem).
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".skopos-upgrade-*")
	if err != nil {
		return fmt.Errorf("writing next to %s: %w", exe, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, tmp.Name(), "--version").CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte(version)) {
		return fmt.Errorf("the downloaded binary does not run as v%s: %v %s", version, err, bytes.TrimSpace(out))
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return fmt.Errorf("replacing %s: %w", exe, err)
	}
	return nil
}

func (w *Worker) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	hc := w.upgradeHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("downloading %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// checksumFor finds asset's sha256 in a checksums.txt ("<hex>  <name>").
func checksumFor(sums []byte, asset string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// newerRelease: is target a release newer than current? Both must be plain
// x.y.z releases (an optional leading v); a dev build or a pre-release on
// either side never upgrades.
func newerRelease(current, target string) bool {
	c, ok1 := parseRelease(current)
	t, ok2 := parseRelease(target)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
}

func parseRelease(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
