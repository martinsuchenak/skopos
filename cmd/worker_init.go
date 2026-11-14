package cmd

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/martinsuchenak/skopos/internal/install"
	"github.com/paularlott/cli"
)

// workerInitCmd scaffolds the worker configuration: an annotated
// [worker] section (plus [client] when the file doesn't exist yet) and a
// skopos-worker.env for secrets. Mirrors `skopos setup`'s annotated-template
// approach — the generated file is a complete, commented starting point.
func workerInitCmd() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Write an annotated [worker] config section (and skopos-worker.env) — the starting point for a worker install",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", Usage: "Prefill [client] server_url (the skopos server this worker polls)"},
			&cli.StringSliceFlag{Name: "workspace", Usage: "Workspace ID to emit a [worker.repos.<id>] block for (repeatable; placeholder values to fill in)"},
			&cli.StringSliceFlag{Name: "repo", Usage: "The workspace's local clone, <workspace>=<path> (or just <path> with one workspace); repeatable"},
			&cli.BoolFlag{Name: "clone", Usage: "Clone a repo that isn't at its path yet (https://<workspace>.git, with your git credentials)"},
			&cli.StringFlag{Name: "push", DefaultValue: "auto", Usage: "Push kind for the repos: github, gitea, none, or auto (github for a github.com workspace when the token is set)"},
			&cli.StringFlag{Name: "push-token-env", DefaultValue: "GH_TOKEN", Usage: "Env var (in skopos-worker.env) holding the push token; its value is taken from the environment"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			cfgPath := configFileFor(cmd)
			o, err := workerInitOptionsFrom(cmd)
			if err != nil {
				return err
			}
			interactive := stdinIsTerminal()
			in := bufio.NewReader(os.Stdin)
			if interactive {
				askWorkerInit(in, os.Stdout, &o, promptSecret)
			}
			repos, secrets, err := prepareWorkerRepos(ctx, os.Stdout, o)
			if err != nil {
				return err
			}
			if err := runWorkerInitRepos(os.Stdout, cfgPath, o.ServerURL, repos, secrets, o.ClaudeEnv); err != nil {
				return err
			}
			// Interactive: set up the first runner right away instead of
			// sending the operator to the docs.
			if interactive {
				if a := strings.ToLower(promptTo(in, os.Stdout, "\nConfigure an agent runner now (wizard)? [Y/n]", "")); a == "" || strings.HasPrefix(a, "y") {
					return runRunnerWizard(ctx, in, os.Stdout, cfgPath, workerEnvFilePath(cfgPath), wizardEnv{})
				}
			}
			return nil
		},
	}
}

func runWorkerInit(out io.Writer, configPath, serverURL string, workspaces []string) error {
	repos := make([]repoInit, 0, len(workspaces))
	for _, ws := range workspaces {
		repos = append(repos, repoInit{Workspace: ws})
	}
	return runWorkerInitRepos(out, configPath, serverURL, repos, nil, "")
}

// claudeEnvKeys: Claude credentials init takes from the environment, in
// order (a subscription's long-lived token, then an API key).
var claudeEnvKeys = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}

// claudeCredential: the first of claudeEnvKeys set in the environment.
func claudeCredential() (key, value string) {
	for _, k := range claudeEnvKeys {
		if v := os.Getenv(k); v != "" {
			return k, v
		}
	}
	return "", ""
}

// runWorkerInitRepos writes the config with the repos as far as they are
// known (an empty path stays a placeholder), and puts secrets (the worker
// key, the push token) in the env file, never in the TOML.
//
// claudeEnv names the env var (in the env file) holding the claude
// account's credential; "" = the machine's own Claude login.
func runWorkerInitRepos(out io.Writer, configPath, serverURL string, repos []repoInit, secrets map[string]string, claudeEnv string) error {
	host, _ := os.Hostname()
	section := workerSectionTOML("worker-"+host, repos, claudeEnv)

	if raw, err := os.ReadFile(configPath); err == nil {
		// Existing config (e.g. from `skopos setup`): add only the [worker]
		// section, preserving everything else. A re-run must refuse — the
		// section holds operator edits by now.
		if hasTOMLSection(string(raw), "worker") {
			return fmt.Errorf("%s already has a [worker] section — edit it directly", configPath)
		}
		updated := install.ReplaceTOMLSection(string(raw), "worker", section)
		// --server-url is honored when the file has no [client] yet; an
		// existing [client] wins (it may hold a deliberately chosen URL).
		if serverURL != "" && !hasTOMLSection(updated, "client") {
			updated = install.ReplaceTOMLSection(updated, "client", clientSectionTOML(serverURL))
			_, _ = fmt.Fprintf(out, "Added a [client] section (no server URL was configured before)\n")
		}
		mode := statMode(configPath, 0o600)
		if err := os.WriteFile(configPath, []byte(updated), mode); err != nil {
			return fmt.Errorf("writing %s: %w", configPath, err)
		}
		_, _ = fmt.Fprintf(out, "Added a [worker] section to %s\n", configPath)
	} else {
		content := clientSectionTOML(serverURL) + section
		// The file may end up holding the worker key ([client].api_key), so
		// it is owner-only from creation — and chmod, because os.WriteFile
		// alone won't tighten an existing file.
		if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", configPath, err)
		}
		if err := os.Chmod(configPath, 0o600); err != nil {
			return fmt.Errorf("setting config file permissions: %w", err)
		}
		_, _ = fmt.Fprintf(out, "Wrote %s (0600)\n", configPath)
	}

	if err := writeWorkerEnvFile(out, configPath); err != nil {
		return err
	}
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if secrets[k] == "" {
			continue
		}
		added, err := appendEnvVar(workerEnvFilePath(configPath), k, secrets[k])
		if err != nil {
			return err
		}
		if added {
			_, _ = fmt.Fprintf(out, "Put %s in %s\n", k, workerEnvFilePath(configPath))
		}
	}
	_, _ = fmt.Fprint(out, "\nNext steps:\n")
	n := 0
	step := func(s string) { n++; _, _ = fmt.Fprintf(out, "  %d. %s\n", n, s) }
	if secrets["SKOPOS_API_KEY"] == "" {
		step("skopos key create --name <this-host> --workspace <workspace-id> --worker   # the worker's key")
		step("put it in skopos-worker.env as SKOPOS_API_KEY=")
	}
	if len(repos) == 0 || slices.ContainsFunc(repos, func(r repoInit) bool { return r.Path == "" }) {
		step("fill in the repos blocks (clone path, setup, checks, push)")
	}
	step("skopos worker runner add                                         # more agent runners + accounts (wizard)")
	step("skopos worker runner test                                        # one real prompt per account")
	step("skopos worker check && skopos worker install                     # launchd/systemd service")
	return nil
}

// writeWorkerEnvFile creates skopos-worker.env next to the config if absent
// (never overwrites — it holds secrets by then).
func writeWorkerEnvFile(out io.Writer, configPath string) error {
	envPath := workerEnvFilePath(configPath)
	if _, err := os.Stat(envPath); err == nil {
		_, _ = fmt.Fprintf(out, "%s already exists — left untouched\n", envPath)
		return nil
	}
	content := `# skopos worker environment — loaded by 'skopos worker' before the config.
# Real environment variables always win over this file. Keep it chmod 600.
# The push token for repos.*.push.token_env lives HERE, never in the TOML.

# SKOPOS_API_KEY=sk_...
# SKOPOS_AGENT_KEY=sk_...       # only for a server before v0.33: runs now get their own token from the claim
# CLAUDE_1_API_KEY=sk-ant-...   # for [[worker.accounts]] env = { ANTHROPIC_API_KEY = "${CLAUDE_1_API_KEY}" }
# GITEA_PUSH_TOKEN=...
`
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", envPath, err)
	}
	if err := os.Chmod(envPath, 0o600); err != nil {
		return fmt.Errorf("setting env file permissions: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Wrote %s (0600) — put the worker key and push tokens there\n", envPath)
	return nil
}

// clientSectionTOML is the [client] half of a fresh config. server_url is
// prefilled when given; the API key stays a comment pointing at the env file.
func clientSectionTOML(serverURL string) string {
	if serverURL == "" {
		serverURL = "https://skopos.example.com"
	}
	return fmt.Sprintf(`# skopos worker configuration (skopos worker init).
# Flags and env vars (SKOPOS_SERVER_URL, SKOPOS_API_KEY, ...) override these.

[client]
server_url = %q
# api_key lives in skopos-worker.env (SKOPOS_API_KEY) or here if you prefer:
# api_key = "sk_..."

`, serverURL)
}

// repoInit is what init knows of one repo; empty fields are written as
// placeholders (path) or defaults (main, no push).
type repoInit struct {
	Workspace     string
	Path          string
	DefaultBranch string
	PushKind      string // github | gitea | none ("" = none)
	RemoteURL     string
	TokenEnv      string
}

// workerSectionTOML is the annotated [worker] section: real keys, placeholder
// values where nothing is known, one repos block per workspace.
func workerSectionTOML(workerID string, repos []repoInit, claudeEnv string) string {
	repoBlocks := ""
	if len(repos) == 0 {
		repos = []repoInit{{Workspace: "<workspace-id>"}}
	}
	workspaces := make([]string, 0, len(repos))
	for _, r := range repos {
		workspaces = append(workspaces, r.Workspace)
		// Workspace ids contain '/' (github.com/owner/repo) — quoted table
		// keys are mandatory in TOML.
		key := fmt.Sprintf("%q", r.Workspace)
		path := cmp.Or(r.Path, "/srv/clones/<repo>")
		kind := cmp.Or(r.PushKind, "none")
		pr := ""
		if kind == "github" {
			pr = "pull_request = \"draft\"            # open (or reuse) a draft PR for the branch\n"
		}
		repoBlocks += fmt.Sprintf(`
[worker.repos.%s]
# A local clone of the repo; worktrees are cut from it per run.
path = %q
default_branch = %q
branch_prefix = "agent/"           # agent branches: agent/<item>-<slug>
setup = []                        # commands run in every fresh worktree ({clone} expands)
checks = []                       # verification commands, e.g. ["task test"]
protected_branches = ["main", "master", "develop", "release/**", "hotfix/**"]

[worker.repos.%s.push]
kind = %q                     # gitea | github | none
remote_url = %q                   # https push URL
token_env = %q                    # env var holding the token (set it in skopos-worker.env)
%s`, key, path, cmp.Or(r.DefaultBranch, "main"), key, kind, r.RemoteURL, r.TokenEnv, pr)
	}
	return fmt.Sprintf(`[worker]
# id must keep the "worker-" prefix (it marks worker-created plans so the
# server's orphan cleanup never touches hand-made ones).
id = %q
state_dir = "~/.local/state/skopos-worker"
# server_commands: take a workspace's setup, check and preflight commands
# from the server once the server holds its config (skopos workspace config).
# Whoever may change them there runs commands on this machine; set false to
# keep them in this file.
server_commands = true
# runners: what claims offer — defaults to every runner the accounts below
# use. Add runners (and their accounts) with: skopos worker runner add
workspaces = [%s]
poll_every = "30s"

[[worker.accounts]]
# The credential pool: picked at run start; a session stays on the account
# that created it. env values are injected into the agent run only, and
# ${VAR} expands from the daemon's environment — so the SECRET lives in
# skopos-worker.env, never in this file. Without env, the account uses this
# machine's own Claude login. Other ways: env = { ANTHROPIC_API_KEY =
# "${CLAUDE_1_API_KEY}" }, or a login directory of its own: env = {
# CLAUDE_CONFIG_DIR = "~/.claude-1" }.
name = "claude-1"
runner = "claude"
priority = 1
%s
# Agent runs load no user/project settings (no ~/.claude.json MCP servers,
# no CLAUDE.md, no hooks). MCP servers for runs come from mcp_config;
# mcp_servers allowlists their tools. Containment and extra tools per stage:
# [worker.profiles.<name>.<stage>] — see docs/deployment/worker.md.
# mcp_config = "~/.config/skopos/worker-mcp.json"
# mcp_servers = ["mcp__skopos"]

# Repos: one block per workspace this worker serves.%s
`, workerID, quotedList(workspaces), claudeAccountEnv(claudeEnv), repoBlocks)
}

func quotedList(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", x)
	}
	return out
}

func statMode(path string, fallback os.FileMode) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return fallback
}

// hasTOMLSection reports whether the file already defines the section (or a
// sub-table of it).
func hasTOMLSection(content, section string) bool {
	for _, l := range strings.Split(content, "\n") {
		t := strings.TrimSpace(l)
		if t == "["+section+"]" || t == "[["+section+"]]" ||
			strings.HasPrefix(t, "["+section+".") || strings.HasPrefix(t, "[["+section+".") {
			return true
		}
	}
	return false
}

// workerEnvFilePath is the env file that belongs to a config: skopos-worker.env
// next to it — exactly where the daemon's default --env-file lookup finds it.
func workerEnvFilePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "skopos-worker.env")
}

// workerInitOptions are init's inputs: flags, the environment, and what the
// interactive questions add.
type workerInitOptions struct {
	ServerURL  string
	Workspaces []string
	RepoPaths  map[string]string // workspace → clone path
	Clone      map[string]bool   // workspaces to clone when missing (all with --clone)
	CloneAll   bool
	Push       string // github | gitea | none | auto
	TokenEnv   string
	APIKey     string // SKOPOS_API_KEY from the environment, or asked
	Token      string // the push token, from the environment, or asked
	// The claude account's credential from the environment
	// (claudeEnvKeys); none = the machine's own Claude login.
	ClaudeEnv, ClaudeValue string
}

func workerInitOptionsFrom(cmd *cli.Command) (workerInitOptions, error) {
	o := workerInitOptions{
		ServerURL:  cmd.GetString("server-url"),
		Workspaces: cmd.GetStringSlice("workspace"),
		RepoPaths:  map[string]string{},
		Clone:      map[string]bool{},
		CloneAll:   cmd.GetBool("clone"),
		Push:       strings.ToLower(strings.TrimSpace(cmd.GetString("push"))),
		TokenEnv:   strings.TrimSpace(cmd.GetString("push-token-env")),
		APIKey:     os.Getenv("SKOPOS_API_KEY"),
	}
	o.ClaudeEnv, o.ClaudeValue = claudeCredential()
	switch o.Push {
	case "auto", "github", "gitea", "none":
	default:
		return o, fmt.Errorf("--push %q: github, gitea, none or auto", o.Push)
	}
	if o.TokenEnv == "" {
		o.TokenEnv = "GH_TOKEN"
	}
	o.Token = os.Getenv(o.TokenEnv)
	if o.Token == "" && o.TokenEnv == "GH_TOKEN" {
		o.Token = os.Getenv("GITHUB_TOKEN")
	}
	for _, r := range cmd.GetStringSlice("repo") {
		ws, path, ok := strings.Cut(r, "=")
		if !ok {
			if len(o.Workspaces) != 1 {
				return o, fmt.Errorf("--repo %q: use <workspace>=<path> (a bare path needs exactly one --workspace)", r)
			}
			ws, path = o.Workspaces[0], r
		}
		ws, path = strings.TrimSpace(ws), strings.TrimSpace(path)
		if !slices.Contains(o.Workspaces, ws) {
			o.Workspaces = append(o.Workspaces, ws)
		}
		o.RepoPaths[ws] = path
	}
	return o, nil
}

// askWorkerInit fills in, at a terminal, what neither flags nor the
// environment gave: the server, a workspace, the worker key, each clone
// (offering to clone it), and the push token for GitHub repos.
func askWorkerInit(in *bufio.Reader, out io.Writer, o *workerInitOptions, secret func(*bufio.Reader, io.Writer, string) string) {
	say := func(f string, a ...any) { _, _ = fmt.Fprintf(out, f, a...) }
	yes := func(label string) bool {
		a := strings.ToLower(promptTo(in, out, label+" [Y/n]", ""))
		return a == "" || strings.HasPrefix(a, "y")
	}
	for o.ServerURL == "" {
		o.ServerURL = promptTo(in, out, "Skopos server URL (e.g. https://skopos.example.com)", "")
	}
	for len(o.Workspaces) == 0 {
		if ws := promptTo(in, out, "Workspace this worker serves (e.g. github.com/you/repo)", ""); ws != "" {
			o.Workspaces = []string{ws}
		}
	}
	if o.APIKey == "" {
		say("\nThe worker's key: mint one with the root key, e.g.\n  skopos key create --name %s --workspace %s --worker\n", hostName(), o.Workspaces[0])
		o.APIKey = secret(in, out, "Worker key (Enter to add it to skopos-worker.env later)")
	}
	home, _ := os.UserHomeDir()
	for _, ws := range o.Workspaces {
		if o.RepoPaths[ws] == "" {
			o.RepoPaths[ws] = promptTo(in, out, "\nLocal clone of "+ws, filepath.Join(home, "src", repoName(ws)))
		}
		if !isGitClone(expandTilde(o.RepoPaths[ws], home)) && !o.CloneAll {
			o.Clone[ws] = yes(fmt.Sprintf("%s isn't a git clone. Clone https://%s.git there now?", o.RepoPaths[ws], ws))
		}
	}
	if o.Push == "auto" && slices.ContainsFunc(o.Workspaces, isGitHub) {
		if yes("\nPush the agents' branches to GitHub and open draft pull requests?") {
			o.Push = "github"
			if o.Token == "" {
				say("A fine-grained token with Contents and Pull requests (read and write) on the repo.\n")
				o.Token = secret(in, out, "GitHub token (Enter to add it to skopos-worker.env later)")
			}
		} else {
			o.Push = "none"
		}
	}
}

// prepareWorkerRepos clones what was asked for, reads each clone's default
// branch, and settles the push block; secrets go to the env file.
func prepareWorkerRepos(ctx context.Context, out io.Writer, o workerInitOptions) ([]repoInit, map[string]string, error) {
	home, _ := os.UserHomeDir()
	var repos []repoInit
	for _, ws := range o.Workspaces {
		r := repoInit{Workspace: ws, Path: expandTilde(o.RepoPaths[ws], home)}
		if r.Path != "" && !isGitClone(r.Path) && (o.CloneAll || o.Clone[ws]) {
			_, _ = fmt.Fprintf(out, "Cloning https://%s.git into %s ...\n", ws, r.Path)
			cmd := exec.CommandContext(ctx, "git", "clone", "https://"+ws+".git", r.Path)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, out
			if err := cmd.Run(); err != nil {
				return nil, nil, fmt.Errorf("cloning %s: %w", ws, err)
			}
		}
		if r.Path != "" && isGitClone(r.Path) {
			r.DefaultBranch = originDefaultBranch(ctx, r.Path)
		}
		kind := o.Push
		if kind == "auto" {
			kind = "none"
			if isGitHub(ws) && o.Token != "" {
				kind = "github"
			}
		}
		if kind != "none" {
			r.PushKind, r.RemoteURL, r.TokenEnv = kind, "https://"+ws+".git", o.TokenEnv
		}
		repos = append(repos, r)
	}
	secrets := map[string]string{"SKOPOS_API_KEY": o.APIKey}
	if o.Token != "" && slices.ContainsFunc(repos, func(r repoInit) bool { return r.PushKind != "" }) {
		secrets[o.TokenEnv] = o.Token
	}
	if o.ClaudeEnv != "" {
		secrets[o.ClaudeEnv] = o.ClaudeValue
	}
	return repos, secrets, nil
}

func isGitHub(ws string) bool { return strings.HasPrefix(ws, "github.com/") }

func repoName(ws string) string { return filepath.Base(strings.TrimSuffix(ws, ".git")) }

func isGitClone(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// originDefaultBranch: origin's HEAD branch ("" when unknown — init then
// writes main).
func originDefaultBranch(ctx context.Context, clone string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", clone, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
}

func hostName() string {
	h, _ := os.Hostname()
	return cmp.Or(h, "this-host")
}

// promptSecret reads a line without echoing it at a terminal (stty), and as
// a plain line otherwise (tests, pipes). Where the echo cannot be turned
// off it says so before the value is typed.
func promptSecret(in *bufio.Reader, out io.Writer, label string) string {
	v, err := readSecret(in, out, label, false)
	if err != nil {
		return ""
	}
	return v
}

// errEchoStaysOn: the terminal's echo could not be turned off.
var errEchoStaysOn = errors.New("this terminal's echo cannot be turned off, so the value would show on screen")

// readSecret is promptSecret; with strict set it refuses to read at a
// terminal that would show the value (no stty: Windows, or a PATH without
// it) instead of reading it in the open.
func readSecret(in *bufio.Reader, out io.Writer, label string, strict bool) (string, error) {
	if stdinIsTerminal() {
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		if off.Run() == nil {
			defer func() {
				on := exec.Command("stty", "echo")
				on.Stdin = os.Stdin
				_ = on.Run()
				_, _ = fmt.Fprintln(out)
			}()
		} else if strict {
			return "", errEchoStaysOn
		} else {
			_, _ = fmt.Fprintln(out, "(this terminal's echo cannot be turned off: what you type next will show)")
		}
	}
	_, _ = fmt.Fprintf(out, "%s: ", label)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line), nil
}

// claudeAccountEnv is the claude account's env line ("" = no env line).
func claudeAccountEnv(key string) string {
	if key == "" {
		return ""
	}
	return fmt.Sprintf("env = { %s = \"${%s}\" }\n", key, key)
}
