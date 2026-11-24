package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
// A clone the agent works in directly (checkout = "branch") cannot be
// protected by naming directories alone: there the agent can write the
// clone's git directory itself — its config and its hooks. So the worker
// also records, for every clone it knows, the config entries and hooks that
// make git run a program or send a request elsewhere (cloneGuard), while
// the clone is still its own: at start for a clone that exists, right after
// cloning for its own. Every git command it runs in that clone or in one of
// its worktrees first compares them with the record, and nothing is run
// when they differ. The clone itself is pinned like a worktree: GIT_DIR and
// GIT_COMMON_DIR from the record, so a .git file or a commondir the agent
// writes is not followed.

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

// clonePins: a clone's own git directory, recorded by registerClone.
var clonePins sync.Map // clean clone path -> gitPin

// cloneGuards: what each recorded git directory held when it was recorded
// (cloneGuardState), by the directory's clean path.
var cloneGuards sync.Map // clean git dir -> string

// registerClone records the clone's git directory and what in it can make
// git run a program, as they are now. Called while no agent has worked in
// the clone since the worker started: at start, and after the worker's own
// clone. A clone that is not there (yet) is not recorded.
func registerClone(clone string) {
	clone = filepath.Clean(clone)
	if _, err := os.Stat(clone); err != nil {
		return
	}
	common, err := cloneGitDir(clone)
	if err != nil {
		return
	}
	state, err := cloneGuardState(common)
	if err != nil {
		return
	}
	cloneGuards.Store(common, state)
	clonePins.Store(clone, gitPin{dir: common, common: common})
}

// guardedKey matches the config entries that make git run a program, read
// files from elsewhere, or send a request elsewhere: their values are part
// of the record. Broad on purpose — an entry here is only compared, never
// refused for being there.
var guardedKey = regexp.MustCompile(`^(include|includeif|core|diff|difftool|merge|mergetool|filter|credential|url|http|https|protocol|gpg|ssh|sequence|pager|alias|uploadpack|receive|extensions|safe|fsmonitor|hook|submodule|lfs)\.|^remote\..*\.(url|pushurl|uploadpack|receivepack|proxy|vcs|helper)$`)

// cloneGuardState is what in the git directory can make git run a program:
// the guarded config entries, and every hook (name, link target, content
// hash), from the directory and from core.hooksPath when it names one.
func cloneGuardState(gitdir string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", "config", "--file", filepath.Join(gitdir, "config"), "--list", "-z")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(gitdir, "config")); statErr != nil {
			return "", err
		}
		return "", fmt.Errorf("reading %s: %w", filepath.Join(gitdir, "config"), err)
	}
	var lines []string
	hooksPath := ""
	for _, entry := range strings.Split(string(out), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		if key == "" || !guardedKey.MatchString(key) {
			continue
		}
		if key == "core.hookspath" {
			hooksPath = value
		}
		lines = append(lines, "config "+key+"="+value)
	}
	sort.Strings(lines)
	dirs := []string{filepath.Join(gitdir, "hooks")}
	if hooksPath != "" {
		if !filepath.IsAbs(hooksPath) {
			hooksPath = filepath.Join(filepath.Dir(gitdir), hooksPath)
		}
		dirs = append(dirs, hooksPath)
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sample") {
				continue
			}
			path := filepath.Join(d, e.Name())
			target, _ := os.Readlink(path)
			sum := ""
			if data, err := os.ReadFile(path); err == nil {
				h := sha256.Sum256(data)
				sum = hex.EncodeToString(h[:])
			}
			lines = append(lines, "hook "+path+" "+target+" "+sum)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// checkCloneGuard refuses a git directory whose config entries or hooks
// that run programs are not what was recorded for it. A directory never
// recorded is not checked (nothing to compare with).
func checkCloneGuard(gitdir string) error {
	want, ok := cloneGuards.Load(filepath.Clean(gitdir))
	if !ok {
		return nil
	}
	// Retryable: the worker's own clone is the problem, not the item —
	// the run goes back and another worker takes it, while this one
	// reports the workspace unhealthy (cloneGuardProblem).
	got, err := cloneGuardState(gitdir)
	if err != nil {
		return retryable(fmt.Errorf("the git config in %s cannot be read: not running git there: %w", gitdir, err))
	}
	if got != want.(string) {
		return retryable(fmt.Errorf("the git config or hooks in %s changed since the worker started (%s): not running git there — something that ran in the clone may have written them; check them, then restart the worker", gitdir, guardDiff(want.(string), got)))
	}
	return nil
}

// guardDiff names what changed, briefly (entry names, not values: a value
// may hold a credential).
func guardDiff(want, got string) string {
	seen := map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		seen[l] = true
	}
	var changed []string
	for _, l := range strings.Split(got, "\n") {
		if l != "" && !seen[l] {
			name, _, _ := strings.Cut(l, "=")
			if strings.HasPrefix(l, "hook ") {
				name = strings.Fields(l)[1]
			}
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return "entries removed"
	}
	if len(changed) > 5 {
		changed = append(changed[:5], "…")
	}
	return strings.Join(changed, ", ")
}

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
	if p, ok := clonePins.Load(dir); ok {
		if err := checkCloneGuard(p.(gitPin).common); err != nil {
			return nil, true, err
		}
		return pinEnv(dir, p.(gitPin)), true, nil
	}
	worktreeRootsMu.Lock()
	clones := worktreeRoots[filepath.Dir(dir)]
	worktreeRootsMu.Unlock()
	if len(clones) == 0 {
		return nil, false, nil
	}
	if p, ok := worktreePins.Load(dir); ok {
		if err := checkCloneGuard(p.(gitPin).common); err != nil {
			return nil, true, err
		}
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
	if err := checkCloneGuard(found[0].common); err != nil {
		return nil, true, err
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
// and in a clone it recorded, the repository is named explicitly
// (gitPinEnv), the config entries and hooks that run programs are what was
// recorded (checkCloneGuard), and the file system monitor is off.
func gitCommand(ctx context.Context, dir string, env []string, args ...string) (*exec.Cmd, error) {
	pin, isWorktree, err := gitPinEnv(dir)
	if err != nil {
		return nil, err
	}
	if isWorktree {
		args = append([]string{"-c", "core.fsmonitor=false"}, args...)
	}
	auth := fetchAuthEnv(ctx, dir, env, args)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...), auth...), pin...)
	return cmd, nil
}
