package cmd

import (
	"context"
	"fmt"

	"github.com/paularlott/cli"
)

func init() {
	Register(completionCmd())
}

func completionCmd() *cli.Command {
	return &cli.Command{
		Name:  "completion",
		Usage: "Generate shell completion scripts",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:         "shell",
				DefaultValue: "bash",
				Usage:        "Shell type (bash, zsh, fish)",
			},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			shell := cmd.GetString("shell")
			switch shell {
			case "bash":
				fmt.Println("# Bash completion for skopos")
				fmt.Println("# Add to ~/.bashrc: eval \"$(skopos completion --shell bash)\"")
			case "zsh":
				fmt.Println("# Zsh completion for skopos")
				fmt.Println("# Add to ~/.zshrc: eval \"$(skopos completion --shell zsh)\"")
			case "fish":
				fmt.Println("# Fish completion for skopos")
				fmt.Println("# Add to config.fish: skopos completion --shell fish | source")
			default:
				return fmt.Errorf("unsupported shell: %s", shell)
			}
			return nil
		},
	}
}
