package cmd

import "testing"

func TestBlackboardCmdExists(t *testing.T) {
	cmd := blackboardCmd()
	if cmd == nil {
		t.Fatal("blackboardCmd should not return nil")
	}
	if cmd.Name != "blackboard" {
		t.Fatalf("expected blackboard command, got %q", cmd.Name)
	}
	if len(cmd.Commands) == 0 {
		t.Fatal("expected blackboard subcommands")
	}
}
