package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/srvurl"
)

// A repo the worker's file lists without a path is cloned by the worker
// itself, into <state_dir>/clones/<workspace>. Until the clone is there the
// workspace is unhealthy for this worker ("cloning …"), so its runs go to
// other workers or wait; the clone runs in the background, one at a time,
// into a temporary folder renamed into place when it is complete. A clone the
// worker made and no longer serves is removed after cloneKeep.

// cloneKeep: how long a clone the worker made stays after its workspace
// left the worker's repos (another change may bring it back).
const cloneKeep = 7 * 24 * time.Hour

// cloneTimeout bounds one clone (a large repository over a slow line).
var cloneTimeout = 2 * time.Hour

// cloneBackoff: when a failed clone is tried again, by the number of
// failures so far (the last one repeats).
var cloneBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, time.Hour}

// cloneDirFor is where the worker keeps its own clone of a workspace: the
// workspace id as a relative path under <state_dir>/clones.
func cloneDirFor(stateDir, ws string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(ws)))
	if ws == "" || strings.ContainsAny(ws, `\:`) || filepath.IsAbs(filepath.FromSlash(ws)) || clean != ws ||
		slices.ContainsFunc(strings.Split(ws, "/"), func(s string) bool { return s == "" || s == "." || s == ".." || strings.Contains(s, ".cloning-") }) {
		return "", fmt.Errorf("the workspace id cannot name a folder for the worker's own clone: give the repo a path")
	}
	return filepath.Join(stateDir, "clones", filepath.FromSlash(ws)), nil
}

// SelfClone reports whether the worker makes this repo's clone itself.
func (r *Repo) SelfClone() bool { return r.selfClone }

// isGitClone: dir holds a clone (its .git is there).
func isGitClone(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// cloneSource is where the worker clones a workspace from: clone_url, else
// the push URL, else https://<workspace>.git.
func cloneSource(ws string, repo *Repo) string {
	switch {
	case repo.CloneURL != "":
		return repo.CloneURL
	case repo.Push.RemoteURL != "":
		return repo.Push.RemoteURL
	}
	return "https://" + ws + ".git"
}

// cloneSourceFor is where this worker clones a workspace from. The
// server's clone_url and push URL count only with worker.server_commands:
// the worker runs setup and checks in what it cloned, so where the code
// comes from is a command field; without it, the file's own repo says.
func (c *Config) cloneSourceFor(ws string, merged *Repo) string {
	if !c.ServerCommands || merged == nil {
		if local := c.Repos[ws]; local != nil {
			return cloneSource(ws, local)
		}
	}
	if merged == nil {
		return "https://" + ws + ".git"
	}
	return cloneSource(ws, merged)
}

// CloneSourceFor is cloneSourceFor for `skopos worker check`, with the
// server's config as CheckServerConfig read it.
func (c *Config) CloneSourceFor(ws string) string {
	return c.cloneSourceFor(ws, c.checkRepos[ws])
}

// cloneState is one workspace's clone as the worker knows it.
type cloneState struct {
	running  bool
	failures int
	reason   string    // the last failure ("" = none)
	next     time.Time // when a failed clone is tried again
}

// cloner runs the worker's clones, one at a time.
type cloner struct {
	mu      sync.Mutex
	state   map[string]*cloneState
	busy    bool
	cleaned bool // leftovers of an earlier process's clones removed
}

// cloneProblem says why the workspace's runs cannot start on this worker
// yet because its clone is not there ("" = the clone is there, or is not
// the worker's to make).
func (w *Worker) cloneProblem(ws string) string {
	repo := w.repo(ws)
	if repo == nil || !repo.selfClone || isGitClone(repo.Path) {
		return ""
	}
	src := redactURL(w.cfg.cloneSourceFor(ws, repo))
	w.clones.mu.Lock()
	defer w.clones.mu.Unlock()
	st := w.clones.state[ws]
	switch {
	case st == nil:
		return fmt.Sprintf("the worker has not cloned %s yet", src)
	case st.running:
		return fmt.Sprintf("cloning %s: the workspace's runs wait for it", src)
	case st.reason != "":
		return fmt.Sprintf("cloning %s failed: %s; trying again at %s", src, st.reason, st.next.Format("15:04"))
	}
	return fmt.Sprintf("the worker has not cloned %s yet", src)
}

// maybeClone starts the next clone that is due, in the background: a
// workspace the worker serves without a clone of its own yet. It never
// waits for a clone.
func (w *Worker) maybeClone(ctx context.Context) {
	// Paused (someone is working on this machine): no idle work, a clone
	// neither. The last claim answer said so.
	if w.paused != "" {
		return
	}
	w.clones.mu.Lock()
	defer w.clones.mu.Unlock()
	if w.clones.busy {
		return
	}
	if w.clones.state == nil {
		w.clones.state = map[string]*cloneState{}
	}
	if !w.clones.cleaned {
		// A clone a killed worker left half done is never taken for one:
		// it was made under another name.
		w.removeCloneLeftovers()
		w.clones.cleaned = true
	}
	now := time.Now()
	for _, ws := range slices.Sorted(slices.Values(w.cfg.Workspaces)) {
		repo := w.repo(ws)
		if repo == nil || !repo.selfClone || isGitClone(repo.Path) {
			continue
		}
		st := w.clones.state[ws]
		if st == nil {
			st = &cloneState{}
			w.clones.state[ws] = st
		}
		if now.Before(st.next) {
			continue
		}
		st.running, w.clones.busy = true, true
		src, dest, branch := w.cfg.cloneSourceFor(ws, repo), repo.Path, repo.DefaultBranch
		env := gitAuthEnv(ctx, repo, src)
		// Built here, in the loop's goroutine: the redactor reads what the
		// loop keeps (the run's secrets).
		redact := func(s string) string { return s }
		if r := w.redactor(); r != nil {
			redact = r.Replace
		}
		w.log.Info("cloning the repository", "workspace", ws, "from", redactURL(src), "into", dest)
		go func() {
			cctx, cancel := context.WithTimeout(ctx, cloneTimeout)
			err := cloneInto(cctx, src, dest, branch, env)
			cancel()
			w.clones.mu.Lock()
			defer w.clones.mu.Unlock()
			st.running, w.clones.busy = false, false
			if err == nil {
				registerClone(dest) // the worker's own: nothing has run in it yet
				st.failures, st.reason, st.next = 0, "", time.Time{}
				w.log.Info("cloned the repository", "workspace", ws, "into", dest)
				return
			}
			if ctx.Err() != nil {
				return // shutting down: not a verdict on the clone
			}
			st.reason = redact(err.Error())
			st.next = time.Now().Add(cloneBackoff[min(st.failures, len(cloneBackoff)-1)])
			st.failures++
			w.log.Warn("cloning the repository failed", "workspace", ws, "err", st.reason, "next", st.next.Format(time.RFC3339))
		}()
		return
	}
}

// cloneInto clones src into dest: into a folder beside it first, renamed
// into place when the clone and the checkout of branch are complete.
func cloneInto(ctx context.Context, src, dest, branch string, env []string) error {
	resolved, err := srvurl.Resolve(ctx, src)
	if err != nil {
		return err
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dest)+".cloning-")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(tmp)
		}
	}()
	run := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
		containProcess(cmd)
		out, err := cmd.CombinedOutput()
		if err := settled(cmd, err); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, tail(strings.TrimSpace(string(out)), 400))
		}
		return nil
	}
	if err := run(parent, "clone", "--quiet", "--no-checkout", "--origin", "origin", "--", resolved, tmp); err != nil {
		return err
	}
	if err := run(tmp, "checkout", "--quiet", branch); err != nil {
		return fmt.Errorf("the default branch %q could not be checked out: %w", branch, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	done = true
	return nil
}

// removeCloneLeftovers removes the temporary folders of clones a killed
// worker left (".<name>.cloning-*" under <state_dir>/clones).
func (w *Worker) removeCloneLeftovers() {
	root := filepath.Join(w.cfg.StateDir, "clones")
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		if strings.Contains(d.Name(), ".cloning-") {
			_ = os.RemoveAll(p)
			return filepath.SkipDir
		}
		if isGitClone(p) {
			return filepath.SkipDir
		}
		return nil
	})
}

// sweepClones removes the clones the worker made for workspaces it no
// longer has a repo for, cloneKeep after it first saw them unused. Only
// clones under <state_dir>/clones: never a path the file gives.
func (w *Worker) sweepClones(now time.Time) {
	root := filepath.Join(w.cfg.StateDir, "clones")
	if _, err := os.Stat(root); err != nil {
		return
	}
	inUse := map[string]bool{}
	for _, r := range w.cfg.Repos {
		if r != nil {
			inUse[filepath.Clean(r.Path)] = true
		}
	}
	var clones []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		if strings.Contains(d.Name(), ".cloning-") {
			return filepath.SkipDir
		}
		if isGitClone(p) {
			clones = append(clones, p)
			return filepath.SkipDir
		}
		return nil
	})
	recPath := filepath.Join(root, ".unused.json")
	unused := map[string]int64{}
	if raw, err := os.ReadFile(recPath); err == nil {
		_ = json.Unmarshal(raw, &unused)
	}
	seen := map[string]int64{}
	for _, p := range clones {
		if inUse[filepath.Clean(p)] {
			continue
		}
		first := unused[p]
		if first == 0 {
			first = now.UnixMilli()
		}
		if now.Sub(time.UnixMilli(first)) < cloneKeep {
			seen[p] = first
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			w.log.Warn("removing a clone the worker no longer uses failed", "clone", p, "err", err)
			seen[p] = first
			continue
		}
		w.log.Info("removed a clone the worker no longer uses", "clone", p)
		for dir := filepath.Dir(p); dir != root && strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil {
				break // not empty
			}
		}
	}
	if len(seen) == 0 {
		if err := os.Remove(recPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			w.log.Warn("removing the unused-clone record failed", "err", err)
		}
		return
	}
	raw, _ := json.Marshal(seen)
	if err := writeFile0600(recPath, raw); err != nil {
		w.log.Warn("writing the unused-clone record failed", "err", err)
	}
}

// redactURL drops what a URL could carry besides its address.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}
