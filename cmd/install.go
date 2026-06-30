package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/paularlott/cli"

	"github.com/martinsuchenak/skopos/internal/install"
)

func init() {
	Register(installCmd())
}

func installCmd() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "Install the skopos MCP server (and steering/skill) into an AI agent's config",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "agent",
				Usage:   "Agent: claude-code, codex, gemini-cli, github-copilot, kiro, opencode, or all",
				EnvVars: []string{"SKOPOS_INSTALL_AGENT"},
			},
			&cli.StringFlag{
				Name:         "url",
				DefaultValue: install.DefaultURL,
				Usage:        "MCP server URL (override for a remote skopos, e.g. https://skopos.example.com/mcp)",
				EnvVars:      []string{"SKOPOS_MCP_URL"},
			},
			&cli.StringFlag{
				Name:    "api-key",
				Usage:   "API key sent as Authorization: Bearer (env SKOPOS_API_KEY)",
				EnvVars: []string{"SKOPOS_API_KEY"},
			},
			&cli.StringFlag{
				Name:         "scope",
				DefaultValue: "global",
				Usage:        "Config scope: global (default) or project (writes into the current directory)",
				EnvVars:      []string{"SKOPOS_INSTALL_SCOPE"},
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "Print what would change without writing anything",
			},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			agent := cmd.GetString("agent")
			if agent == "" {
				return fmt.Errorf("--agent is required (one of: claude-code, codex, gemini-cli, github-copilot, kiro, opencode, all)")
			}
			results, err := install.Install(install.Options{
				Agent:  agent,
				URL:    cmd.GetString("url"),
				APIKey: cmd.GetString("api-key"),
				Scope:  cmd.GetString("scope"),
				DryRun: cmd.GetBool("dry-run"),
			})
			if err != nil {
				return err
			}
			for _, r := range results {
				fmt.Fprintf(os.Stdout, "=== %s ===\n", r.Agent)
				for _, a := range r.Actions {
					fmt.Fprintf(os.Stdout, "  - %s\n", a)
				}
			}
			return nil
		},
	}
}
