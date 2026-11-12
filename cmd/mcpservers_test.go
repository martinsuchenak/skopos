package cmd

import (
	"strings"
	"testing"
)

func TestKeyValues(t *testing.T) {
	m, err := keyValues("header", []string{"Authorization=Bearer ${secret:token}", " X-Tenant =globex", "Empty="})
	if err != nil || m["Authorization"] != "Bearer ${secret:token}" || m["X-Tenant"] != "globex" || m["Empty"] != "" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := keyValues("env", []string{"novalue"}); err == nil || !strings.Contains(err.Error(), "NAME=value") {
		t.Fatalf("a flag without =: %v", err)
	}
	if m, err := keyValues("env", nil); err != nil || m != nil {
		t.Fatal("no flags, no map")
	}
}

// A secret's value comes from an environment variable or standard input,
// never from the command line.
func TestReadSecretValueFromEnv(t *testing.T) {
	t.Setenv("SKOPOS_TEST_MCP_TOKEN", "from-the-environment")
	if v, err := readSecretValue("SKOPOS_TEST_MCP_TOKEN", "x token"); err != nil || v != "from-the-environment" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := readSecretValue("SKOPOS_TEST_NOT_SET", "x token"); err == nil {
		t.Fatal("an unset variable gave a value")
	}
	// set-secret has one flag of its own, and it names a variable.
	for _, sub := range mcpServerCmd().Commands {
		if sub.Name != "set-secret" {
			continue
		}
		if got, want := len(sub.Flags), len(keyClientFlags())+1; got != want {
			t.Fatalf("set-secret has %d flags, want %d: the value must not be a flag", got, want)
		}
		if len(sub.Arguments) != 2 {
			t.Fatalf("set-secret takes the server and the secret's name, not the value: %d arguments", len(sub.Arguments))
		}
	}
}
