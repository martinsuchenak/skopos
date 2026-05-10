package cmd

import "testing"

func TestServeCmdExists(t *testing.T) {
	cmd := serveCmd()
	if cmd == nil {
		t.Fatal("serveCmd should not return nil")
	}
	if cmd.Name != "serve" {
		t.Errorf("expected command name 'serve', got %q", cmd.Name)
	}
}
