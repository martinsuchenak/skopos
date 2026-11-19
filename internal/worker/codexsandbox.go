package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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

// codexProbe is what the check starts for one codex runner: its binary,
// and the CODEX_HOME its runs use (the login folder of its first account).
// Codex keeps the helper its sandbox starts through in CODEX_HOME, and
// refuses to create it in a temporary folder, so the check uses the
// folder a run does.
type codexProbe struct {
	Bin  string `json:"bin"`
	Home string `json:"home"`
}

// codexSandboxPlan is what the check tries, per runner of kind codex, and
// a key that changes when the check must run again.
func (c *Config) codexSandboxPlan() (probes map[string]codexProbe, key string) {
	if codexSandboxGOOS != "linux" {
		return nil, ""
	}
	for _, name := range c.UsedRunners() {
		r, err := c.RunnerFor(name)
		if err != nil {
			continue
		}
		cr, ok := r.(codexRunner)
		if !ok {
			continue
		}
		var env map[string]string
		for _, a := range c.Accounts {
			if accountRunner(a) == name {
				env = c.agentEnv(a)
				if a.LoginDir != "" {
					env["CODEX_HOME"] = a.LoginDir
				}
				break
			}
		}
		if probes == nil {
			probes = map[string]codexProbe{}
		}
		probes[name] = codexProbe{Bin: cr.bin, Home: codexHome(env)}
	}
	if probes == nil {
		return nil, ""
	}
	buf, _ := json.Marshal(map[string]any{"probes": probes, "path": os.Getenv("PATH")})
	return probes, string(buf)
}

// codexSandboxProblems runs the check for every codex runner: why its
// sandbox cannot start on this host, by runner name (nil = it can, or there
// is no codex runner).
func (c *Config) codexSandboxProblems(ctx context.Context, probes map[string]codexProbe) map[string]string {
	env := scrubEnv(nil, c.TokenEnvs(), c.envFileKeys)
	seen := map[codexProbe]string{}
	var out map[string]string
	names := make([]string, 0, len(probes))
	for name := range probes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		p := probes[name]
		why, done := seen[p]
		if !done {
			why = codexSandboxProblem(ctx, p.Bin, p.Home, env)
			seen[p] = why
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
	probes, key := w.cfg.codexSandboxPlan()
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
	bad := w.cfg.codexSandboxProblems(ctx, probes)
	if ctx.Err() != nil {
		return false // shutting down: not a verdict
	}
	for name := range probes {
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
// around a command that does nothing, with the runner's CODEX_HOME (home),
// and says why it failed ("" = it started, or this codex has no sandbox
// command to tell, or the home is not there yet: the login check says so).
func codexSandboxProblem(ctx context.Context, bin, home string, env []string) string {
	if fi, err := os.Stat(home); home == "" || err != nil || !fi.IsDir() {
		return ""
	}
	dir, err := os.MkdirTemp("", "skopos-codex-check-")
	if err != nil {
		return ""
	}
	defer func() { _ = os.RemoveAll(dir) }()
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
