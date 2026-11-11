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

// What a runner policy sees of the embeddings endpoint: derived from the
// URL, or declared when it cannot be derived — and a declaration the URL
// contradicts refuses to start.
func TestEmbeddingsFactsFor(t *testing.T) {
	for _, c := range []struct{ url, declared, provider string }{
		{"http://localhost:11434/v1", "", "local"},
		{"https://api.openai.com/v1", "", "openai"},
		{"http://10.0.0.5:11434/v1", "", "unknown:10.0.0.5"},
		{"http://10.0.0.5:11434/v1", "local", "local"}, // a model on another machine on the network
		{"http://embed.lan/v1", " Local ", "local"},
		{"https://api.openai.com/v1", "openai", "openai"},
	} {
		f, err := embeddingsFactsFor(c.url, "nomic-embed-text", c.declared)
		if err != nil || f.Provider != c.provider || f.Kind != "embeddings" || f.Model != "nomic-embed-text" {
			t.Errorf("%s declared %q: %+v %v, want provider %s", c.url, c.declared, f, err, c.provider)
		}
	}
	if _, err := embeddingsFactsFor("https://api.openai.com/v1", "m", "local"); err == nil {
		t.Fatal("declared local on a cloud URL must be refused")
	}
	if _, err := embeddingsFactsFor("http://localhost:11434/v1", "m", "openai"); err == nil {
		t.Fatal("declared openai on a loopback URL must be refused")
	}
}
