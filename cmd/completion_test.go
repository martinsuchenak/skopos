package cmd

import "testing"

func TestCompletionCmdExists(t *testing.T) {
	cmd := completionCmd()
	if cmd == nil {
		t.Fatal("completionCmd should not return nil")
	}
	if cmd.Name != "completion" {
		t.Errorf("expected command name 'completion', got %q", cmd.Name)
	}
}
