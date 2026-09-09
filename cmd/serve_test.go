package cmd

import (
	"testing"

	"github.com/paularlott/cli"
)

func TestServeCmdExists(t *testing.T) {
	cmd := serveCmd()
	if cmd == nil {
		t.Fatal("serveCmd should not return nil")
	}
	if cmd.Name != "serve" {
		t.Errorf("expected command name 'serve', got %q", cmd.Name)
	}
}

func TestServeDefaultsToLoopback(t *testing.T) {
	for _, f := range serveCmd().Flags {
		sf, ok := f.(*cli.StringFlag)
		if !ok || sf.Name != "server-host" {
			continue
		}
		if sf.DefaultValue != "127.0.0.1" {
			t.Errorf("server-host must default to loopback, got %q", sf.DefaultValue)
		}
		return
	}
	t.Fatal("server-host flag not found")
}
