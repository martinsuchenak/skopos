package cmd

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/workspace"
	"github.com/paularlott/cli"
)

func init() {
	Register(workspaceCmd())
}

func workspaceCmd() *cli.Command {
	return &cli.Command{
		Name:  "workspace",
		Usage: "Print the resolved workspace ID for the current directory",
		Run: func(_ context.Context, _ *cli.Command) error {
			id, err := workspace.Resolve(".")
			if err != nil {
				return err
			}
			fmt.Println(id)
			return nil
		},
		Commands: []*cli.Command{
			pipelineCmd(pipeline.KindWorkspace),
			{
				Name:  "add",
				Usage: "Register a workspace on the server (root key; default: this checkout's workspace)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
					&cli.StringFlag{Name: "api-key", Usage: "Skopos API key (root)", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
					&cli.StringFlag{Name: "id", Usage: "Workspace ID, e.g. github.com/you/repo (default: resolved from this checkout's git remote)"},
					&cli.StringFlag{Name: "name", Usage: "Display name"},
					&cli.StringFlag{Name: "git-url", Usage: "Clone URL, for server-side code-index refresh"},
				},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					id := strings.TrimSpace(cmd.GetString("id"))
					if id == "" {
						resolved, err := workspace.Resolve(".")
						if err != nil {
							return fmt.Errorf("--id is required outside a git checkout: %w", err)
						}
						id = resolved
					}
					c := &apiClient{base: strings.TrimRight(cmd.GetString("server-url"), "/"), key: cmd.GetString("api-key")}
					var ws struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					}
					body := map[string]string{"id": id, "name": cmd.GetString("name"), "git_url": cmd.GetString("git-url")}
					if err := c.call(ctx, http.MethodPost, "/api/workspaces", body, &ws, "registering workspace"); err != nil {
						return err
					}
					fmt.Printf("registered %s\n", ws.ID)
					return nil
				},
			},
			{
				Name:  "list",
				Usage: "List the workspaces registered on the server",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
					&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
				},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					c := &apiClient{base: strings.TrimRight(cmd.GetString("server-url"), "/"), key: cmd.GetString("api-key")}
					var list []struct {
						ID     string `json:"id"`
						Name   string `json:"name"`
						GitURL string `json:"git_url"`
					}
					if err := c.call(ctx, http.MethodGet, "/api/workspaces", nil, &list, "listing workspaces"); err != nil {
						return err
					}
					for _, w := range list {
						line := w.ID
						if w.Name != "" {
							line += "  (" + w.Name + ")"
						}
						if w.GitURL != "" {
							line += "  " + w.GitURL
						}
						fmt.Println(line)
					}
					return nil
				},
			},
		},
	}
}
