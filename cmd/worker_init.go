package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
			&cli.StringSliceFlag{Name: "workspace", Usage: "Workspace ID to emit a [worker.repos.<id>] block for (repeatable; stays commented until filled in)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			return runWorkerInit(os.Stdout, configFileFor(cmd), cmd.GetString("server-url"), cmd.GetStringSlice("workspace"))
		},
	}
}

func runWorkerInit(out io.Writer, configPath, serverURL string, workspaces []string) error {
	host, _ := os.Hostname()
	section := workerSectionTOML("worker-"+host, workspaces)

	if raw, err := os.ReadFile(configPath); err == nil {
		// Existing config (e.g. from `skopos setup`): add only the [worker]
		// section, preserving everything else. A re-run must refuse — the
		// section holds operator edits by now.
		if hasTOMLSection(string(raw), "worker") {
			return fmt.Errorf("%s already has a [worker] section — edit it directly", configPath)
		}
		updated := install.ReplaceTOMLSection(string(raw), "worker", section)
		mode := statMode(configPath, 0o600)
		if err := os.WriteFile(configPath, []byte(updated), mode); err != nil {
			return fmt.Errorf("writing %s: %w", configPath, err)
		}
		fmt.Fprintf(out, "Added a [worker] section to %s — edit repos/accounts, then:\n", configPath)
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
		fmt.Fprintf(out, "Wrote %s (0600) — edit repos/accounts, then:\n", configPath)
	}

	if err := writeWorkerEnvFile(out, configPath); err != nil {
		return err
	}
	fmt.Fprint(out, `
Next steps:
  1. skopos key create --name <this-host> --workspace <workspace-id>   # worker-scoped key
  2. fill in [client].api_key (or SKOPOS_API_KEY in skopos-worker.env) and the repos/accounts blocks
  3. skopos worker install                                            # launchd/systemd service
`)
	return nil
}

// writeWorkerEnvFile creates skopos-worker.env next to the config if absent
// (never overwrites — it holds secrets by then).
func writeWorkerEnvFile(out io.Writer, configPath string) error {
	envPath := workerEnvFilePath(configPath)
	if _, err := os.Stat(envPath); err == nil {
		fmt.Fprintf(out, "%s already exists — left untouched\n", envPath)
		return nil
	}
	content := `# skopos worker environment — loaded by 'skopos worker' before the config.
# Real environment variables always win over this file. Keep it chmod 600.
# The push token for repos.*.push.token_env lives HERE, never in the TOML.

# SKOPOS_API_KEY=sk_...
# GITEA_PUSH_TOKEN=...
`
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", envPath, err)
	}
	if err := os.Chmod(envPath, 0o600); err != nil {
		return fmt.Errorf("setting env file permissions: %w", err)
	}
	fmt.Fprintf(out, "Wrote %s (0600) — put the worker key and push tokens there\n", envPath)
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

// workerSectionTOML is the annotated [worker] section: real keys, placeholder
// values, one commented repos block per requested workspace.
func workerSectionTOML(workerID string, workspaces []string) string {
	repoBlocks := ""
	if len(workspaces) == 0 {
		workspaces = []string{"<workspace-id>"}
	}
	for _, ws := range workspaces {
		// Workspace ids contain '/' (github.com/owner/repo) — quoted table
		// keys are mandatory in TOML.
		key := fmt.Sprintf("%q", ws)
		repoBlocks += fmt.Sprintf(`
[worker.repos.%s]
# A local clone of the repo; worktrees are cut from it per run.
path = "/srv/clones/<repo>"
default_branch = "main"
branch_prefix = "agent/"           # agent branches: agent/<item>-<slug>
setup = []                        # commands run in every fresh worktree ({clone} expands)
checks = []                       # verification commands, e.g. ["task test"]
protected_branches = ["PROD", "main", "master", "develop", "release/**"]

[worker.repos.%s.push]
kind = "none"                     # gitea | github | none
remote_url = ""                   # https push URL
token_env = ""                    # env var holding the token (set it in skopos-worker.env)
`, key, key)
	}
	return fmt.Sprintf(`[worker]
# id must keep the "worker-" prefix (it marks worker-created plans so the
# server's orphan cleanup never touches hand-made ones).
id = %q
state_dir = "~/.local/state/skopos-worker"
runners = ["claude"]              # what this instance offers; must cover the
                                  # workspaces' stage preferences
workspaces = [%s]
poll_every = "30s"

[[worker.accounts]]
# The credential pool: picked at run start; a session stays on the account
# that created it. env values are injected into the agent run only.
name = "claude-1"
runner = "claude"
priority = 1
env = { ANTHROPIC_API_KEY = "sk-ant-..." }

# Repos: one block per workspace this worker serves.%s
`, workerID, quotedList(workspaces), repoBlocks)
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
