package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// The codex sandbox check. Codex runs every command the agent gives its
// shell, file reads included, inside its own sandbox with the permission
// profile skopos passes (runner_codex.go): read the worktree and the git
// directory, write nothing, no network. On Linux that sandbox is
// bubblewrap, which needs the host to allow unprivileged user namespaces.
// Where it does not, every command fails and the agent works blind, and the
// run still reports a result. The check starts the sandbox the way a run
// does; where it cannot start, the worker reports the codex runner's
// executor as none:check with the reason, so the server hands codex runs to
// a worker where the sandbox works. A run in a wrapper is left out: the
// sandbox starts inside the wrapper there, which this check cannot try.

// codexSandboxGOOS: the OS the check runs on (tests set it). macOS's
// sandbox is the system's own and always there.
var codexSandboxGOOS = runtime.GOOS

// codexSandboxCheck is the worker's last codex sandbox check.
type codexSandboxCheck struct {
	key string
	at  time.Time
	bad map[string]string
}

// codexSandboxPlan is what the check tries: the codex binary of each
// runner of kind codex, and a key that changes when the check must run
// again.
func (c *Config) codexSandboxPlan() (bins map[string]string, key string) {
	if codexSandboxGOOS != "linux" {
		return nil, ""
	}
	for _, name := range c.UsedRunners() {
		r, err := c.RunnerFor(name)
		if err != nil {
			continue
		}
		if cr, ok := r.(codexRunner); ok {
			if bins == nil {
				bins = map[string]string{}
			}
			bins[name] = cr.bin
		}
	}
	if bins == nil {
		return nil, ""
	}
	buf, _ := json.Marshal(map[string]any{"bins": bins, "path": os.Getenv("PATH")})
	return bins, string(buf)
}

// codexSandboxProblems runs the check for every codex runner: why its
// sandbox cannot start on this host, by runner name (nil = it can, or there
// is no codex runner).
func (c *Config) codexSandboxProblems(ctx context.Context, bins map[string]string) map[string]string {
	env := scrubEnv(nil, c.TokenEnvs(), c.envFileKeys)
	byBin := map[string]string{}
	var out map[string]string
	names := make([]string, 0, len(bins))
	for name := range bins {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		bin := bins[name]
		why, seen := byBin[bin]
		if !seen {
			why = codexSandboxProblem(ctx, bin, env)
			byBin[bin] = why
		}
		if why != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = why
		}
	}
	return out
}

// checkCodexSandbox runs the check when it is due and records what it found
// for ExecutorFor. It reports whether that changed.
func (w *Worker) checkCodexSandbox(ctx context.Context) (changed bool) {
	bins, key := w.cfg.codexSandboxPlan()
	last := w.codexCheck
	if key == "" {
		w.codexCheck = codexSandboxCheck{}
		return w.cfg.setCodexSandboxBad(nil)
	}
	now := time.Now()
	due := last.key != key || (len(last.bad) > 0 && now.Sub(last.at) >= stageRecheckFailed) || now.Sub(last.at) >= stageCheckEvery
	if !due {
		return false
	}
	bad := w.cfg.codexSandboxProblems(ctx, bins)
	if ctx.Err() != nil {
		return false // shutting down: not a verdict
	}
	for name := range bins {
		was, why := last.bad[name], bad[name]
		switch {
		case why != "" && why != was:
			w.log.Warn("codex's sandbox cannot start here: not taking runs on this runner until it can", "runner", name, "reason", why)
		case why == "" && was != "":
			w.log.Info("codex's sandbox starts again", "runner", name)
		}
	}
	for name, why := range bad {
		bad[name] = clip(w.redacted(why), maxCheckReason)
	}
	w.codexCheck = codexSandboxCheck{key: key, at: now, bad: bad}
	return w.cfg.setCodexSandboxBad(bad)
}

// setCodexSandboxBad records the check's result and reports whether it
// changed.
func (c *Config) setCodexSandboxBad(bad map[string]string) bool {
	if len(bad) == 0 {
		bad = nil
	}
	if len(c.codexBad) == 0 && bad == nil {
		return false
	}
	same := len(c.codexBad) == len(bad)
	for k, v := range bad {
		if c.codexBad[k] != v {
			same = false
		}
	}
	c.codexBad = bad
	return !same
}

// codexSandboxBad says why a run on runner with the resolved profile may
// not start because codex's sandbox cannot ("" = it may).
func (c *Config) codexSandboxBad(prof *StageProfile, runner string) string {
	if len(c.codexBad) == 0 || prof.wraps() {
		return ""
	}
	return c.codexBad[runner]
}

// codexSandboxProblem starts codex's sandbox with a run's permission profile
// around a command that does nothing, in an empty CODEX_HOME (no login, no
// config of the account's), and says why it failed ("" = it started, or
// this codex has no sandbox command to tell).
func codexSandboxProblem(ctx context.Context, bin string, env []string) string {
	dir, err := os.MkdirTemp("", "skopos-codex-check-")
	if err != nil {
		return ""
	}
	defer func() { _ = os.RemoveAll(dir) }()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, stageCheckTimeout)
	defer cancel()
	// The filesystem a run gets, for a worktree in this folder: on Linux
	// codex starts its own binary again inside the sandbox, so its install
	// folder must be readable, as it is for a run (codexReadRoots).
	probe := AgentRun{Dir: dir, Env: map[string]string{"PATH": os.Getenv("PATH")}}
	filesystem := codexFilesystem(codexOpenTemps(probe), codexReadRoots(cctx, bin, probe))
	// The shape TestCodexLockdownProbe verifies against the real codex.
	cmd := exec.CommandContext(cctx, bin, "sandbox", "--permission-profile", "skopos", "-C", dir,
		"-c", "permissions.skopos.filesystem="+filesystem,
		"-c", "permissions.skopos.network.enabled=false",
		"--", "true")
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(env), "CODEX_HOME="+home, "PWD="+dir)
	containProcess(cmd)
	out, err := cmd.CombinedOutput()
	if err = settled(cmd, err); err == nil || ctx.Err() != nil {
		return ""
	}
	text := strings.TrimSpace(string(out))
	if cctx.Err() != nil {
		return fmt.Sprintf("codex's sandbox did not start within %s", stageCheckTimeout)
	}
	// A codex without the sandbox command cannot be checked this way.
	if strings.Contains(text, "unrecognized subcommand") || strings.Contains(text, "unexpected argument") {
		return ""
	}
	if strings.Contains(text, "namespace") {
		return "codex's sandbox cannot start: this host does not let bubblewrap create a user namespace (allow unprivileged user namespaces, or run codex in a wrapper): " + tail(outputOr(text, err), 160)
	}
	return "codex's sandbox cannot start: " + tail(outputOr(text, err), 160)
}
