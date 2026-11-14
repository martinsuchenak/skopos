package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Where git finds the repository of a worktree is written in two small
// files: <worktree>/.git names the worktree's own git directory, and the
// "commondir" file in that directory names the clone's. Both are writable
// by an agent that works in the worktree — also by one inside a wrapper,
// which binds the worktree and the git directory. An agent that rewrites
// either makes every git command the WORKER later runs there (a status, a
// log, the push with its token in the environment) read its config and its
// hooks from a directory the agent made: code run outside the wrapper, as
// the worker's user.
//
// So the worker does not let git read those files. For a command in a
// worktree it made, it names the directories itself, from the clone's side:
// GIT_DIR (the worktree's own), GIT_COMMON_DIR (the clone's) and
// GIT_WORK_TREE. What remains is the clone's own config and hooks, which
// the documented wrapper profile binds read-only.
//
// A clone the agent works in directly (checkout = "branch") is not covered:
// there the agent can write to the directory git starts from.

// worktreeRoots maps a directory the worker makes worktrees in
// (<state_dir>/worktrees/<repo>) to the clones whose worktrees are there.
var (
	worktreeRootsMu sync.Mutex
	worktreeRoots   = map[string][]string{}
	// worktreePins: the answer for a worktree, once found. The directory
	// names do not change while the worktree exists.
	worktreePins sync.Map // worktree path -> gitPin
)

type gitPin struct{ dir, common string }

// registerWorktreeRoot says that worktrees under root belong to clone.
func registerWorktreeRoot(root, clone string) {
	root, clone = filepath.Clean(root), filepath.Clean(clone)
	worktreeRootsMu.Lock()
	defer worktreeRootsMu.Unlock()
	for _, c := range worktreeRoots[root] {
		if c == clone {
			return
		}
	}
	worktreeRoots[root] = append(worktreeRoots[root], clone)
}

// forgetWorktreePin drops what is remembered about a worktree (it was
// removed; the path may be made again).
func forgetWorktreePin(worktree string) { worktreePins.Delete(filepath.Clean(worktree)) }

// gitPinEnv is the environment that fixes where git finds the repository
// for a command run in dir, and whether dir is such a worktree at all. An
// error means dir is a worktree the worker made and its git directory
// cannot be told from the clone's side: nothing is run there on trust.
func gitPinEnv(dir string) ([]string, bool, error) {
	dir = filepath.Clean(dir)
	worktreeRootsMu.Lock()
	clones := worktreeRoots[filepath.Dir(dir)]
	worktreeRootsMu.Unlock()
	if len(clones) == 0 {
		return nil, false, nil
	}
	if p, ok := worktreePins.Load(dir); ok {
		return pinEnv(dir, p.(gitPin)), true, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		// Not a worktree (yet): `git worktree add` runs in the clone.
		return nil, false, nil
	}
	var found []gitPin
	for _, clone := range clones {
		common, err := cloneGitDir(clone)
		if err != nil {
			continue
		}
		admins, _ := filepath.Glob(filepath.Join(common, "worktrees", "*"))
		for _, admin := range admins {
			raw, err := os.ReadFile(filepath.Join(admin, "gitdir"))
			if err != nil {
				continue
			}
			if samePath(filepath.Dir(strings.TrimSpace(string(raw))), dir) {
				found = append(found, gitPin{dir: admin, common: common})
			}
		}
	}
	if len(found) != 1 {
		return nil, true, fmt.Errorf("the git directory of worktree %s cannot be told from its clone (%d candidates): not running git there", dir, len(found))
	}
	worktreePins.Store(dir, found[0])
	return pinEnv(dir, found[0]), true, nil
}

func pinEnv(dir string, p gitPin) []string {
	return []string{"GIT_DIR=" + p.dir, "GIT_COMMON_DIR=" + p.common, "GIT_WORK_TREE=" + dir}
}

// cloneGitDir is the clone's own git directory, asked in the clone (which
// no agent of a worktree run can write to).
func cloneGitDir(clone string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = clone
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return filepath.Clean(strings.TrimSpace(string(out))), nil
}

// samePath compares two directory paths after resolving links (a state
// directory under /tmp or /var is spelt two ways on some systems).
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, erra := filepath.EvalSymlinks(a)
	rb, errb := filepath.EvalSymlinks(b)
	return erra == nil && errb == nil && ra == rb
}

// gitCommand builds a git command for dir. In a worktree the worker made,
// the repository is named explicitly (gitPinEnv) and the file system
// monitor, which runs a configured command, is off.
func gitCommand(ctx context.Context, dir string, env []string, args ...string) (*exec.Cmd, error) {
	pin, isWorktree, err := gitPinEnv(dir)
	if err != nil {
		return nil, err
	}
	if isWorktree {
		args = append([]string{"-c", "core.fsmonitor=false"}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...), pin...)
	return cmd, nil
}
