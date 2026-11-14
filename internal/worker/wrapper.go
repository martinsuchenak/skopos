package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Wrapper placeholders (tmp/central-config — design.md, phase 1c). A
// wrapper that confines a run (bwrap, a container) has to name what the
// run still needs:
//
//	{dir}        the run's worktree (or the clone, for a branch checkout)
//	{gitdir}     the clone's git directory — a worktree's .git file points
//	             into it and commits write there
//	{login}      the account's login directory
//	{rundir}     the run's own directory under state_dir: its MCP file,
//	             its log and the runners' temporary config directories
//	{cli}        the directory the agent CLI's binary is in
//	{run} {stage} {workspace}
var wrapperPlaceholders = wsconfig.WrapperPlaceholders

// loginDir is the directory an account's agent CLI keeps its login in:
// login_dir on the account, else what its kind uses — CLAUDE_CONFIG_DIR
// (default ~/.claude), CODEX_HOME (default ~/.codex), opencode's data
// directory (XDG_DATA_HOME/opencode, default ~/.local/share/opencode),
// ~/.gemini for antigravity. "" for a command runner without login_dir.
// Read from the environment the agent really gets (agentEnv): a variable
// the scrub drops never reaches the CLI, so it names no login directory.
func (c *Config) loginDir(a *Account) string {
	if a == nil {
		return ""
	}
	if a.LoginDir != "" {
		return a.LoginDir
	}
	env := c.agentEnv(a)
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	switch _, kind := c.runnerSpec(accountRunner(a)); kind {
	case "claude":
		if d := env["CLAUDE_CONFIG_DIR"]; d != "" {
			return d
		}
		return filepath.Join(home, ".claude")
	case "codex":
		if d := env["CODEX_HOME"]; d != "" {
			return d
		}
		return filepath.Join(home, ".codex")
	case "opencode":
		if d := env["XDG_DATA_HOME"]; d != "" {
			return filepath.Join(d, "opencode")
		}
		return filepath.Join(home, ".local", "share", "opencode")
	case "antigravity":
		return filepath.Join(home, ".gemini")
	}
	return ""
}

// cliPath is the agent CLI's binary, symlinks resolved (a Homebrew or
// installer shim points into the real install). "" when it is not found.
func cliPath(bin string) string {
	p := bin
	if !filepath.IsAbs(p) {
		found, err := exec.LookPath(bin)
		if err != nil {
			return ""
		}
		p = found
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p
}

// cliDir is the directory of the agent CLI's resolved binary.
func cliDir(bin string) string {
	p := cliPath(bin)
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// gitCommonDir is the git directory shared by dir's worktrees, "" when dir
// is not in a repository. For a worktree the worker made it is the clone's,
// named from the clone's side (gitpin.go) and never read from the
// worktree's own files: a wrapper binds this directory, and an agent of an
// earlier turn could have pointed those files anywhere.
func gitCommonDir(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, err := gitCommand(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	o, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(o))
}

// expandWrapper fills a wrapper's placeholders for one run. A placeholder
// the wrapper uses and the worker cannot resolve is an error: a wrapper
// run with an empty bind would confine nothing, or the wrong thing.
func expandWrapper(wrapper []string, vals map[string]string) ([]string, error) {
	pairs := make([]string, 0, len(vals)*2)
	for _, ph := range wrapperPlaceholders {
		used := false
		for _, a := range wrapper {
			if strings.Contains(a, ph) {
				used = true
				break
			}
		}
		if !used {
			continue
		}
		v := vals[ph]
		if v == "" {
			return nil, fmt.Errorf("the stage wrapper uses %s, which this run has no value for%s", ph, placeholderHint(ph))
		}
		pairs = append(pairs, ph, v)
	}
	r := strings.NewReplacer(pairs...)
	out := make([]string, len(wrapper))
	for i, a := range wrapper {
		out[i] = r.Replace(a)
	}
	return out, nil
}

func placeholderHint(ph string) string {
	switch ph {
	case "{login}":
		return " (set login_dir on the account)"
	case "{cli}":
		return " (the agent CLI was not found on the worker's PATH)"
	case "{gitdir}":
		return " (the run's directory is not in a git repository)"
	}
	return ""
}

// runDir is a run's own directory under the state dir.
func (c *Config) runDir(runID string) string {
	return filepath.Join(c.StateDir, "runs", runID)
}

// plainRunID: a run id usable as a directory name under the state dir.
func plainRunID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`) && filepath.Base(id) == id
}

// runTempDir makes the directory a run's runners create their temporary
// config directories in — under the run's own directory, so a wrapper
// binds one path and nothing of the run lives in the shared /tmp. "" when
// it cannot be made (the runners then use the system's temp directory).
// A wrapped agent can write in the run's directory: whatever it left in
// place of the directory (a symlink) is removed, never followed.
func (c *Config) runTempDir(runID string) string {
	if c.StateDir == "" || !plainRunID(runID) {
		return ""
	}
	dir := filepath.Join(c.runDir(runID), "tmp")
	if fi, err := os.Lstat(dir); err == nil && !fi.IsDir() {
		_ = os.Remove(dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// writeNoFollow writes a file the worker owns in a directory an agent may
// have been able to write in (a run's directory is bound into its
// wrapper): whatever is at path is removed first and the file is created
// exclusively, so a symlink left there is replaced, never written through.
func writeNoFollow(path string, b []byte, mode os.FileMode) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// stageWrapper expands a stage profile's wrapper for one run: the argv
// prefix the agent command — and the worker's setup and check commands for
// the stage — run behind. usesCLI reports whether the wrapper binds {cli}
// (the agent is then started by its resolved path, which is inside it).
func (w *Worker) stageWrapper(prof *StageProfile, dir, phase string, run *Run, account *Account) (argv []string, err error) {
	runner := accountRunner(account)
	return expandWrapper(prof.Wrapper, map[string]string{
		"{dir}": dir, "{run}": run.ID, "{stage}": phase, "{workspace}": run.WorkspaceID,
		"{gitdir}": gitCommonDir(dir), "{login}": w.cfg.loginDir(account),
		"{rundir}": w.cfg.runDir(run.ID), "{cli}": cliDir(w.cfg.RunnerBin(runner)),
	})
}

// shellWrap gives the wrapper prefix for a setup or check command run in
// dir (nil = run it plainly).
type shellWrap func(dir string) ([]string, error)

type shellWrapKey struct{}

// withShellWrap makes runShell run its commands behind the stage's wrapper.
func withShellWrap(ctx context.Context, fn shellWrap) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, shellWrapKey{}, fn)
}

func shellWrapFrom(ctx context.Context) shellWrap {
	fn, _ := ctx.Value(shellWrapKey{}).(shellWrap)
	return fn
}

// stageShellWrap is the wrapper the worker's own setup and check commands
// of a stage run behind: the stage profile's, when it has a real one. Those
// commands execute the branch's code — code the implementer wrote — so a
// wrapper that confines the agent has to confine them too.
func (w *Worker) stageShellWrap(run *Run, account *Account, phase string) shellWrap {
	prof, problem := w.cfg.stageProfileFor(run.WorkspaceID, phase, accountRunner(account))
	if problem != "" {
		// The wrapper the stage needs cannot be used here: its commands
		// fail, they never run bare.
		return func(string) ([]string, error) { return nil, errors.New(problem) }
	}
	if !prof.wraps() {
		return nil
	}
	return func(dir string) ([]string, error) {
		return w.stageWrapper(prof, dir, phase, run, account)
	}
}
