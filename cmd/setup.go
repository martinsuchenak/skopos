package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
	"github.com/paularlott/cli"
)

func init() {
	Register(setupCmd())
}

const configFileName = "skopos-config.toml"

func setupCmd() *cli.Command {
	return &cli.Command{
		Name:  "setup",
		Usage: "Interactive setup: choose a local-only or remote workflow, configure it, and index your first repo",
		Run: func(ctx context.Context, cmd *cli.Command) error {
			return runSetup(ctx, bufio.NewReader(os.Stdin), filepath.Dir(configFileFor(cmd)))
		},
	}
}

// configFileFor resolves the config path the same way the CLI does.
func configFileFor(cmd *cli.Command) string {
	if p := cmd.GetString("config"); p != "" {
		return p
	}
	return configFileName
}

// setupOut is the output sink (an io.Writer, swapped in tests).
var setupOut io.Writer = os.Stdout

func setupPrint(format string, args ...any) {
	fmt.Fprintf(setupOut, format, args...)
}

// runSetup drives the wizard. configDir is where skopos-config.toml lives.
func runSetup(ctx context.Context, in *bufio.Reader, configDir string) error {
	setupPrint(`skopos setup
============
This configures how the code index commands talk to skopos.

  1) Local only   — index into ./indexes in this repo, no server needed
  2) Remote       — push to and query a central skopos server (URL + API key)

`)

	mode := prompt(in, "Choose a workflow [1-2]", "1")
	switch strings.TrimSpace(mode) {
	case "2":
		return setupRemote(ctx, in, configDir)
	default:
		return setupLocal(ctx, in)
	}
}

func setupLocal(ctx context.Context, in *bufio.Reader) error {
	workspace := workspaceOrDefault("")
	if workspace == "" {
		workspace = prompt(in, "Workspace ID for this repo (e.g. github.com/you/repo)", "")
		if workspace == "" {
			return fmt.Errorf("a workspace ID is required (run inside a git repo or provide one)")
		}
	} else {
		setupPrint("Workspace: %s (from the git remote)\n", workspace)
		if alt := prompt(in, "Override", ""); alt != "" {
			workspace = alt
		}
	}

	dir := prompt(in, "Local index directory", "indexes")
	store, err := codeindex.NewStore(dir)
	if err != nil {
		return err
	}
	defer store.Close()

	setupPrint("Indexing current directory...\n")
	results, head, err := codeindex.Build(ctx, parse.NewExtractor(), ".", "")
	if err != nil {
		return err
	}
	branch := gitBranch(".")
	if branch == "" {
		branch = "main"
	}
	host, _ := os.Hostname()
	if err := codeindex.CommitLocal(store, workspace, branch, "local-setup:"+host, results, head); err != nil {
		return err
	}
	symbols := 0
	for _, r := range results {
		symbols += len(r.Symbols)
	}
	setupPrint("\nDone. Indexed %d files (%d symbols) into %s for %s@%s.\n\n", len(results), symbols, dir, workspace, branch)
	printLocalExamples()
	return nil
}

func setupRemote(ctx context.Context, in *bufio.Reader, configDir string) error {
	serverURL := ""
	for {
		serverURL = strings.TrimRight(prompt(in, "skopos server URL", "http://localhost:8080"), "/")
		if serverURL == "" {
			continue
		}
		if err := checkServerAlive(ctx, serverURL); err != nil {
			setupPrint("  cannot reach the server: %v\n", err)
			if strings.TrimSpace(prompt(in, "Try again? [Y/n]", "y")) == "n" {
				return fmt.Errorf("aborted")
			}
			continue
		}
		break
	}

	apiKey := ""
	for {
		apiKey = prompt(in, "API key (empty if the server runs without auth)", "")
		code, err := checkServerAuth(ctx, serverURL, apiKey)
		if err != nil {
			setupPrint("  auth check failed: %v\n", err)
			continue
		}
		switch code {
		case http.StatusOK:
			setupPrint("  connection OK%s\n", authNote(apiKey))
		case http.StatusUnauthorized:
			setupPrint("  the server rejected this key\n")
			continue
		}
		break
	}

	// Persist [client] into skopos-config.toml (creating the file if needed).
	cfgPath := filepath.Join(configDir, configFileName)
	if configDir == "" {
		cfgPath = configFileName
	}
	if err := writeClientConfig(cfgPath, serverURL, apiKey); err != nil {
		return err
	}
	setupPrint("Saved %s (server URL and API key; flags and SKOPOS_* env vars still override)\n", cfgPath)
	if _, err := os.Stat(".git"); err == nil {
		setupPrint("Tip: add %s to .gitignore — it holds the API key.\n", filepath.Base(cfgPath))
	}

	// Offer to index the current directory.
	if strings.HasPrefix(strings.ToLower(prompt(in, "\nIndex the current directory now? [y/N]", "n")), "y") {
		workspace := workspaceOrDefault("")
		if workspace == "" {
			workspace = prompt(in, "Workspace ID for this repo", "")
			if workspace == "" {
				return fmt.Errorf("a workspace ID is required (run inside a git repo or provide one)")
			}
		}
		branch := gitBranch(".")
		if branch == "" {
			branch = "main"
		}
		uploaded, total, err := PushToServer(ctx, serverURL, apiKey, workspace, branch, ".")
		if err != nil {
			return err
		}
		setupPrint("Pushed %d files (%d uploaded) for %s@%s.\n", total, uploaded, workspace, branch)
	}

	setupPrint("\n")
	printRemoteExamples(serverURL)
	setupPrint("\nAgents: 'skopos install --url %s/mcp", serverURL)
	if apiKey != "" {
		setupPrint(" --api-key <key>")
	}
	setupPrint("' wires MCP clients to this server.\n")
	return nil
}

func authNote(apiKey string) string {
	if apiKey == "" {
		return " (server runs without authentication)"
	}
	return ""
}

// checkServerAlive verifies something skopos-like answers at /health.
func checkServerAlive(ctx context.Context, serverURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/health returned %s", resp.Status)
	}
	return nil
}

// checkServerAuth verifies the key against an auth-protected endpoint.
func checkServerAuth(ctx context.Context, serverURL, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/api/sessions", nil)
	if err != nil {
		return 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// writeClientConfig creates or updates the [client] section, preserving the
// rest of the file. New files are owner-only (they may hold an API key).
func writeClientConfig(path, serverURL, apiKey string) error {
	block := fmt.Sprintf("[client]\nserver_url = %q\napi_key = %q\n", serverURL, apiKey)
	existing := ""
	if raw, err := os.ReadFile(path); err == nil {
		existing = string(raw)
	}
	updated, existed := replaceClientSection(existing, block)
	if !existed {
		if existing != "" && !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		updated = existing + "\n" + block
	}
	mode := os.FileMode(0o644)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		mode = 0o600
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, []byte(updated), mode)
}

// replaceClientSection swaps the [client] block (until the next section or
// EOF) with block; reports whether a section existed.
func replaceClientSection(content, block string) (string, bool) {
	lines := strings.Split(content, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "[client]" {
			start = i
			break
		}
	}
	if start == -1 {
		return content, false
	}
	end := start + 1
	for end < len(lines) {
		t := strings.TrimSpace(lines[end])
		if strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[client") {
			break
		}
		end++
	}
	before := strings.Join(lines[:start], "\n")
	after := strings.Join(lines[end:], "\n")
	res := before
	if res != "" && !strings.HasSuffix(res, "\n") {
		res += "\n"
	}
	res += block
	if after != "" {
		if !strings.HasSuffix(res, "\n") {
			res += "\n"
		}
		res += after
	}
	return res, true
}

func prompt(in *bufio.Reader, label, def string) string {
	if def != "" {
		setupPrint("%s [%s]: ", label, def)
	} else {
		setupPrint("%s: ", label)
	}
	line, err := in.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return def
	}
	if strings.TrimSpace(line) == "" {
		return def
	}
	return strings.TrimSpace(line)
}

func printLocalExamples() {
	setupPrint("Example commands (from this repo):\n\n" +
		"  skopos search <query>          # symbol search (camelCase-aware)\n" +
		"  skopos who-calls <name>        # call sites\n" +
		"  skopos impact <name>           # what a change affects\n" +
		"  skopos outline <file>          # a file's definitions\n" +
		"  skopos index status            # what's indexed\n\n" +
		"Re-run 'skopos index build' after pulling changes to refresh the index.\n" +
		"See docs/guides/local.md for the full walkthrough.\n")
}

func printRemoteExamples(serverURL string) {
	setupPrint(`Example commands (configured against %s):

  skopos index push              # from any checkout of a repo
  skopos search <query>          # queries the central index
  skopos index refresh --wait    # server re-indexes a registered repo
  skopos index status            # indexed branches

See docs/guides/remote.md for the full walkthrough.
`, serverURL)
}
