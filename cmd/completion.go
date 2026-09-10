package cmd

import (
	"github.com/paularlott/cli"
)

func init() {
	Register(cli.GenerateCompletionCommand())
}
