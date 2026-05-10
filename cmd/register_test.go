package cmd

import "testing"

func TestCommandsNotEmpty(t *testing.T) {
	cmds := Commands()
	if len(cmds) == 0 {
		t.Fatal("Commands() should return at least one command")
	}
}
