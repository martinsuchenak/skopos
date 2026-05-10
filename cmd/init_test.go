package cmd

import "testing"

func TestInitCmdExists(t *testing.T) {
	cmd := initCmd()
	if cmd == nil {
		t.Fatal("initCmd should not return nil")
	}
	if cmd.Name != "init" {
		t.Errorf("expected command name 'init', got %q", cmd.Name)
	}
}
