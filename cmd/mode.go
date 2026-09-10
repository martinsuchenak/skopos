package cmd

import (
	"context"
	"fmt"

	"github.com/paularlott/cli"
)

func init() {
	Register(modeCmd())
}

// modeCmd prints the resolved workflow mode: "remote <url>" when a client
// server_url is configured (config/env/flag), "local" otherwise. Hooks and
// scripts use it to tailor guidance — MCP tools exist only against a server;
// in local mode the CLI is the interface.
func modeCmd() *cli.Command {
	return &cli.Command{
		Name:   "mode",
		Usage:  "Print the resolved workflow: local, or remote <server-url>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:       "server-url",
				Usage:      "Client server URL (set by skopos setup)",
				EnvVars:    []string{"SKOPOS_SERVER_URL"},
				ConfigPath: []string{"client.server_url"},
			},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			if url := cmd.GetString("server-url"); url != "" {
				fmt.Printf("remote %s\n", url)
				return nil
			}
			fmt.Println("local")
			return nil
		},
	}
}
