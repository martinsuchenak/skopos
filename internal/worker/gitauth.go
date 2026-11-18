package worker

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/srvurl"
)

// The worker's own fetches, ls-remotes and clones send the repo's push
// token, as the push does, so a private repository needs no other git
// credential on the machine (tmp/worker-setup — design.md, D7). Only to the
// push URL's host: the token goes nowhere else, whatever the clone's origin
// says. Never in an agent's, a setup's or a check's environment: only the
// worker's own git process gets it, as GIT_CONFIG_VALUE_0.

// cloneRepos maps a clone's path to the repo it is the clone of (the
// worker's current view of it, managed config included).
var cloneRepos sync.Map // clean path -> func() *Repo

// registerCloneRepo says which repo the clone at path belongs to.
func registerCloneRepo(path string, repo func() *Repo) {
	cloneRepos.Store(filepath.Clean(path), repo)
}

// gitAuthEnv is the environment that makes git send the repo's push token
// to target ("" for none: no token, no push, or another host). TLS is
// verified, as for the push, whatever the clone's config says.
func gitAuthEnv(ctx context.Context, repo *Repo, target string) []string {
	if repo == nil || repo.Push.TokenEnv == "" || repo.Push.RemoteURL == "" {
		return nil
	}
	switch repo.Push.Kind {
	case "github", "gitea":
	default:
		return nil
	}
	token := os.Getenv(repo.Push.TokenEnv)
	if token == "" || !sameForgeAs(ctx, repo.Push.RemoteURL, target) {
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + pushAuthHeader(repo.Push.Kind, token),
		"GIT_CONFIG_KEY_1=http.sslVerify", "GIT_CONFIG_VALUE_1=true",
	}
}

// sameForgeAs is sameForge for a push URL that may be srv+: a clone of one
// has the resolved address as its origin.
func sameForgeAs(ctx context.Context, push, target string) bool {
	if sameForge(push, target) {
		return true
	}
	if !strings.HasPrefix(push, "srv+") {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resolved, err := srvurl.Resolve(rctx, push)
	return err == nil && sameForge(resolved, target)
}

// sameForge: target is on the push URL's host, over https (or the push
// URL's own scheme), with no credentials of its own.
func sameForge(push, target string) bool {
	p, err1 := url.Parse(push)
	t, err2 := url.Parse(target)
	if err1 != nil || err2 != nil || t.User != nil {
		return false
	}
	return strings.EqualFold(p.Host, t.Host) && (t.Scheme == "https" || t.Scheme == p.Scheme)
}

// networkGit names the git subcommands that talk to a remote.
func networkGit(args []string) bool {
	for len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "fetch", "ls-remote", "pull":
		return true
	}
	return false
}

// fetchAuthEnv is gitAuthEnv for a network command run in a clone the
// worker knows: the target is the clone's origin, read from the clone. A
// url.*.insteadOf rule anywhere in git's config sends no token: the clone's
// own config is writable by the code its setup and checks run, and such a
// rule would carry the fetch, and the header, to another host.
func fetchAuthEnv(ctx context.Context, dir string, env, args []string) []string {
	if !networkGit(args) || hasGitConfigEnv(env) {
		return nil
	}
	v, ok := cloneRepos.Load(filepath.Clean(dir))
	if !ok {
		return nil
	}
	repo := v.(func() *Repo)()
	if repo == nil || repo.Push.TokenEnv == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "config", "--get-regexp", `^(remote\.origin\.url|url\..*\.(insteadof|pushinsteadof))$`)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	origin := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, _ := strings.Cut(line, " ")
		if key != "remote.origin.url" {
			return nil // a URL rewrite: no token
		}
		origin = strings.TrimSpace(value)
	}
	return gitAuthEnv(ctx, repo, origin)
}

// hasGitConfigEnv: the caller already passes git settings by environment
// (the push's own header): they are not added to.
func hasGitConfigEnv(env []string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			return true
		}
	}
	return false
}
