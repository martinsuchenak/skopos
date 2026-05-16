package cmd

import (
	"context"
	"fmt"
	"os"

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
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(1)
			}
			fmt.Println(id)
			return nil
		},
	}
}
