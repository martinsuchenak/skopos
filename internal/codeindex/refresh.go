package codeindex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

// RefreshState tracks the asynchronous server-side build for one workspace.
// Progress is written atomically while Building (files done/total).
type RefreshState struct {
	Building     bool   `json:"building"`
	Branch       string `json:"branch,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	LastStarted  string `json:"last_started,omitempty"`
	LastFinished string `json:"last_finished,omitempty"`
	Progress     string `json:"progress,omitempty"` // "1234/5678 files"
}

// Refresher builds workspace indexes server-side by cloning/pulling the
// workspace's git URL into the index directory and indexing the checkout.
// Private repositories use the host's git credentials (the git CLI is invoked
// directly, never through a shell).
type Refresher struct {
	store        *Store
	checkoutsDir string
	gitURL       func(workspace string) (string, error)

	mu     sync.Mutex
	states map[string]*RefreshState
}

func NewRefresher(store *Store, indexDir string, gitURL func(workspace string) (string, error)) (*Refresher, error) {
	dir := filepath.Join(indexDir, "checkouts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Refresher{store: store, checkoutsDir: dir, gitURL: gitURL, states: map[string]*RefreshState{}}, nil
}

// State returns the current refresh state for a workspace.
func (r *Refresher) State(workspace string) RefreshState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.states[workspace]; ok {
		return *s
	}
	return RefreshState{}
}

// validBranch enforces git-refname safety: no leading dash (option
// injection into git fetch/clone), no control chars, whitespace, or
// ref-meta characters.
func validBranch(branch string) bool {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return false
	}
	if len(branch) > 200 || strings.ContainsAny(branch, " \t\r\n~^:?*[\\\x00") || strings.Contains(branch, "..") {
		return false
	}
	return true
}

// safeGitURL blocks git remote-helper transports (ext::, fd::, and any
// scheme with "::") that can execute local commands during clone/fetch,
// and non-git schemes (SSRF surface). Allowed: http(s)://, git://, ssh://,
// file://, plain paths, and scp-like user@host:path.
func safeGitURL(u string) bool {
	if u == "" || strings.Contains(u, "::") {
		return false
	}
	if i := strings.Index(u, "://"); i > 0 {
		switch strings.ToLower(u[:i]) {
		case "http", "https", "git", "ssh", "file":
			return true
		}
		return false
	}
	return true // no scheme: local path or scp-like syntax — no helper transport
}

// Start kicks an asynchronous refresh; it returns immediately. Only one build
// per workspace runs at a time.
func (r *Refresher) Start(ctx context.Context, workspace, branch string) error {
	url, err := r.gitURL(workspace)
	if err != nil {
		return err
	}
	if url == "" {
		return fmt.Errorf("%w: workspace %s has no git_url registered (POST /api/workspaces with git_url first)", ErrInvalidInput, workspace)
	}
	if !safeGitURL(url) {
		return fmt.Errorf("%w: workspace %s has an unsafe git_url (allowed: http(s), git, ssh, file, or a plain path)", ErrInvalidInput, workspace)
	}
	if branch != "" && !validBranch(branch) {
		return fmt.Errorf("%w: invalid branch name %q", ErrInvalidInput, branch)
	}

	r.mu.Lock()
	if s := r.states[workspace]; s != nil && s.Building {
		r.mu.Unlock()
		return fmt.Errorf("%w: a refresh is already running for %s", ErrInvalidInput, workspace)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	r.states[workspace] = &RefreshState{Building: true, Branch: branch, LastStarted: now, Progress: "cloning"}
	r.mu.Unlock()

	go func() {
		state := r.run(context.WithoutCancel(ctx), workspace, branch, url)
		r.mu.Lock()
		r.states[workspace] = &state
		r.mu.Unlock()
	}()
	return nil
}

func (r *Refresher) run(ctx context.Context, workspace, branch, gitURL string) RefreshState {
	s := RefreshState{Branch: branch, LastStarted: time.Now().UTC().Format(time.RFC3339)}
	var progress atomic.Value // string
	setProgress := func(txt string) {
		progress.Store(txt)
		r.mu.Lock()
		if cur, ok := r.states[workspace]; ok && cur.Building {
			cur.Progress = txt
		}
		r.mu.Unlock()
	}
	defer func() {
		s.LastFinished = time.Now().UTC().Format(time.RFC3339)
		if p, ok := progress.Load().(string); ok {
			s.Progress = p
		}
	}()

	checkout, err := r.syncCheckout(gitURL, branch)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	setProgress("0/? files")
	results, head, err := BuildWithCache(ctx, parse.NewExtractor(), checkout, branch, func(done, total int) {
		setProgress(fmt.Sprintf("%d/%d files", done, total))
	}, r.store.AsBuildCache(workspace))
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	if err := CommitLocal(r.store, workspace, branchName(branch, head), "server-build", results, head); err != nil {
		s.LastError = err.Error()
		return s
	}
	return s
}

func branchName(branch, head string) string {
	if branch != "" {
		return branch
	}
	if head != "" {
		return "default"
	}
	return "main"
}

func (r *Refresher) syncCheckout(gitURL, branch string) (string, error) {
	dir := filepath.Join(r.checkoutsDir, slugOf(gitURL))
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if branch == "" {
			if out, err := git(dir, "pull", "--ff-only"); err != nil {
				return "", fmt.Errorf("git pull: %w (%s)", err, lastLine(out))
			}
			return dir, nil
		}
		if out, err := git(dir, "fetch", "--depth", "1", "origin", branch); err != nil {
			return "", fmt.Errorf("git fetch: %w (%s)", err, lastLine(out))
		}
		if out, err := git(dir, "checkout", "-f", "-B", branch, "FETCH_HEAD"); err != nil {
			return "", fmt.Errorf("git checkout: %w (%s)", err, lastLine(out))
		}
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, gitURL, dir)
	if out, err := git("", args...); err != nil {
		os.RemoveAll(dir) // partial clone: start clean next time
		return "", fmt.Errorf("git clone: %w (%s)", err, lastLine(out))
	}
	return dir, nil
}

func git(dir string, args ...string) (string, error) {
	var cmd *exec.Cmd
	if dir != "" {
		cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
	} else {
		cmd = exec.Command("git", args...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func lastLine(out string) string {
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] == '\n' {
			out = out[i+1:]
			break
		}
	}
	if len(out) > 120 {
		out = out[:117] + "..."
	}
	// git errors can echo the remote URL; strip any embedded credentials.
	if i := strings.Index(out, "://"); i > 0 {
		if j := strings.Index(out[i+3:], "@"); j >= 0 {
			out = out[:i+3] + "***@" + out[i+3+j+1:]
		}
	}
	return out
}
