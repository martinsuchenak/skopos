package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
	"github.com/paularlott/cli"
)

func init() {
	Register(indexCmd())
}

func indexCmd() *cli.Command {
	return &cli.Command{
		Name:  "index",
		Usage: "Build, push, and inspect the central code index",
		Commands: []*cli.Command{
			indexBuildCmd(),
			indexPushCmd(),
			indexStatusCmd(),
			indexDropCmd(),
		},
	}
}

// gitBranch returns the current branch of a checkout ("" when not a repo).
func gitBranch(root string) string {
	out, err := execGit(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func execGit(root string, args ...string) (string, error) {
	cmd := execCommand("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func indexBuildCmd() *cli.Command {
	return &cli.Command{
		Name:    "build",
		Usage:   "Parse a checkout and build/refresh its branch index locally",
		MaxArgs: 1,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "index-dir", DefaultValue: "indexes", Usage: "Local index directory"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID (default: git remote of the checkout)"},
			&cli.StringFlag{Name: "branch", Usage: "Branch (default: current git branch)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			root := "."
			if args := cmd.GetArgs(); len(args) > 0 {
				root = args[0]
			}
			workspace := workspaceOrDefault(cmd.GetString("workspace"))
			if workspace == "" {
				return fmt.Errorf("--workspace is required (or run inside a git repo)")
			}
			branch := cmd.GetString("branch")
			if branch == "" {
				branch = gitBranch(root)
			}
			if branch == "" {
				return fmt.Errorf("--branch is required (or run inside a git repo)")
			}
			store, err := codeindex.NewStore(cmd.GetString("index-dir"))
			if err != nil {
				return err
			}
			defer store.Close()

			results, head, err := codeindex.Build(ctx, parse.NewExtractor(), root, branch)
			if err != nil {
				return err
			}
			host, _ := os.Hostname()
			if err := codeindex.CommitLocal(store, workspace, branch, "local-build:"+host, results, head); err != nil {
				return err
			}
			symbols := 0
			for _, r := range results {
				symbols += len(r.Symbols)
			}
			fmt.Printf("indexed %d files (%d symbols) for %s@%s\n", len(results), symbols, workspace, branch)
			if head != "" {
				fmt.Printf("head: %s\n", head)
			}
			return nil
		},
	}
}

func indexPushCmd() *cli.Command {
	return &cli.Command{
		Name:    "push",
		Usage:   "Build a checkout and push its branch index to a skopos server",
		MaxArgs: 1,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID (default: git remote of the checkout)"},
			&cli.StringFlag{Name: "branch", Usage: "Branch (default: current git branch)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			root := "."
			if args := cmd.GetArgs(); len(args) > 0 {
				root = args[0]
			}
			workspace := workspaceOrDefault(cmd.GetString("workspace"))
			if workspace == "" {
				return fmt.Errorf("--workspace is required (or run inside a git repo)")
			}
			branch := cmd.GetString("branch")
			if branch == "" {
				branch = gitBranch(root)
			}
			if branch == "" {
				return fmt.Errorf("--branch is required (or run inside a git repo)")
			}
			serverURL := strings.TrimRight(cmd.GetString("server-url"), "/")
			apiKey := cmd.GetString("api-key")

			results, head, err := codeindex.Build(ctx, parse.NewExtractor(), root, branch)
			if err != nil {
				return err
			}

			entries := make([]codeindex.FileEntry, 0, len(results))
			byHash := map[string]*parse.FileResult{}
			for _, r := range results {
				entries = append(entries, codeindex.FileEntry{Path: r.Path, Hash: r.Hash})
				byHash[r.Hash] = r
			}

			// 1. manifest: which blobs does the server need?
			manifest, err := postJSON[struct {
				Missing []string `json:"missing"`
			}](ctx, serverURL, apiKey, "POST", fmt.Sprintf("/api/codeindex/%s/manifest", url.PathEscape(workspace)),
				map[string]any{"files": entries}, "application/json")
			if err != nil {
				return fmt.Errorf("manifest: %w", err)
			}

			// 2. upload only the missing blobs as ndjson.
			if len(manifest.Missing) > 0 {
				var buf bytes.Buffer
				w := bufio.NewWriter(&buf)
				enc := json.NewEncoder(w)
				for _, h := range manifest.Missing {
					if r, ok := byHash[h]; ok {
						enc.Encode(r)
					}
				}
				w.Flush()
				if _, err := postJSON[map[string]any](ctx, serverURL, apiKey, "POST",
					fmt.Sprintf("/api/codeindex/%s/blobs", url.PathEscape(workspace)), buf.Bytes(), "application/x-ndjson"); err != nil {
					return fmt.Errorf("blobs: %w", err)
				}
			}

			// 3. commit the branch.
			host, _ := os.Hostname()
			if _, err := postJSON[map[string]any](ctx, serverURL, apiKey, "POST",
				fmt.Sprintf("/api/codeindex/%s/commit", url.PathEscape(workspace)),
				map[string]any{
					"branch": branch, "head_sha": head,
					"source": "push:" + host, "files": entries,
				}, "application/json"); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			fmt.Printf("pushed %d files (%d uploaded) for %s@%s\n", len(entries), len(manifest.Missing), workspace, branch)
			return nil
		},
	}
}

func indexStatusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "List indexed branches of a workspace",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", EnvVars: []string{"SKOPOS_SERVER_URL"}, Usage: "Remote skopos (omit for local index-dir)"},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "index-dir", DefaultValue: "indexes", Usage: "Local index directory (when no server-url)"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			workspace := cmd.GetString("workspace")
			if workspace == "" {
				workspace = workspaceOrDefault("")
			}
			if workspace == "" {
				return fmt.Errorf("--workspace is required")
			}
			var status []codeindex.BranchStatus
			if cmd.GetString("server-url") != "" {
				res, err := getJSON[[]codeindex.BranchStatus](ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
					fmt.Sprintf("/api/codeindex/%s/status", url.PathEscape(workspace)))
				if err != nil {
					return err
				}
				status = res
			} else {
				store, err := codeindex.NewStore(cmd.GetString("index-dir"))
				if err != nil {
					return err
				}
				defer store.Close()
				status, err = codeindex.NewService(store).Status(ctx, workspace)
				if err != nil {
					return err
				}
			}
			if len(status) == 0 {
				fmt.Println("no indexed branches")
				return nil
			}
			for _, s := range status {
				fmt.Printf("%-30s %5d files %7d symbols  %s%s\n", s.Branch, s.FileCount, s.SymbolCount, s.BuiltAt, sourceSuffix(s.Source))
			}
			return nil
		},
	}
}

func indexDropCmd() *cli.Command {
	return &cli.Command{
		Name:  "drop",
		Usage: "Remove a branch's index state",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", EnvVars: []string{"SKOPOS_SERVER_URL"}, Usage: "Remote skopos (omit for local index-dir)"},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "index-dir", DefaultValue: "indexes", Usage: "Local index directory (when no server-url)"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
			&cli.StringFlag{Name: "branch", Usage: "Branch to drop"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			workspace := cmd.GetString("workspace")
			if workspace == "" {
				workspace = workspaceOrDefault("")
			}
			branch := cmd.GetString("branch")
			if workspace == "" || branch == "" {
				return fmt.Errorf("--workspace and --branch are required")
			}
			if cmd.GetString("server-url") != "" {
				base := strings.TrimRight(cmd.GetString("server-url"), "/")
				req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
					base+fmt.Sprintf("/api/codeindex/%s/branch/%s", url.PathEscape(workspace), url.PathEscape(branch)), nil)
				if err != nil {
					return err
				}
				if k := cmd.GetString("api-key"); k != "" {
					req.Header.Set("Authorization", "Bearer "+k)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent {
					return fmt.Errorf("%s", apiErrorMessage("dropping branch", resp))
				}
			} else {
				store, err := codeindex.NewStore(cmd.GetString("index-dir"))
				if err != nil {
					return err
				}
				defer store.Close()
				if err := codeindex.NewService(store).DropBranch(ctx, workspace, branch); err != nil {
					return err
				}
			}
			fmt.Printf("dropped %s@%s\n", workspace, branch)
			return nil
		},
	}
}

func sourceSuffix(source string) string {
	if source == "" {
		return ""
	}
	return "  (" + source + ")"
}
