package cmd

import "github.com/paularlott/cli"

var registry []*cli.Command

func Register(c *cli.Command) {
	registry = append(registry, c)
}

func Commands() []*cli.Command {
	return registry
}
