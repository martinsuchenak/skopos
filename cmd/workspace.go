package cmd

import (
	"context"
	"fmt"

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
	}
}
