package cmd

import "testing"

func TestWorkspaceCmdExists(t *testing.T) {
	cmd := workspaceCmd()
	if cmd == nil {
		t.Fatal("workspaceCmd should not return nil")
	}
	if cmd.Name != "workspace" {
		t.Fatalf("expected workspace command, got %q", cmd.Name)
	}
}
