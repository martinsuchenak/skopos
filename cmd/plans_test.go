package cmd

import "testing"

func TestPlanCmdExists(t *testing.T) {
	cmd := planCmd()
	if cmd == nil {
		t.Fatal("planCmd should not return nil")
	}
	if cmd.Name != "plan" {
		t.Fatalf("expected plan command, got %q", cmd.Name)
	}
	if len(cmd.Commands) == 0 {
		t.Fatal("expected plan subcommands")
	}
}
