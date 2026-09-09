package codeindex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

// RefreshState tracks the asynchronous server-side build for one workspace.
type RefreshState struct {
	Building     bool   `json:"building"`
	Branch       string `json:"branch,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	LastStarted  string `json:"last_started,omitempty"`
	LastFinished string `json:"last_finished,omitempty"`
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

	r.mu.Lock()
	if s := r.states[workspace]; s != nil && s.Building {
		r.mu.Unlock()
		return fmt.Errorf("%w: a refresh is already running for %s", ErrInvalidInput, workspace)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	r.states[workspace] = &RefreshState{Building: true, Branch: branch, LastStarted: now}
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
	defer func() { s.LastFinished = time.Now().UTC().Format(time.RFC3339) }()

	checkout, err := r.syncCheckout(gitURL, branch)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	results, head, err := Build(ctx, parse.NewExtractor(), checkout, branch)
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
	return out
}
