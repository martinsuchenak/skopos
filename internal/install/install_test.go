package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetTOMLSectionAppends(t *testing.T) {
	out := setTOMLSection("", "[mcp_servers.skopos]\nenabled = true\nurl = \"x\"\n")
	if !strings.Contains(out, `[mcp_servers.skopos]`) || !strings.Contains(out, `url = "x"`) {
		t.Fatalf("expected appended block, got:\n%s", out)
	}
}

func TestSetTOMLSectionReplacesOnlySkopos(t *testing.T) {
	content := `[other]
foo = 1

[mcp_servers.skopos]
enabled = true
url = "old"

[mcp_servers.skopos.headers]
Authorization = "Bearer old"

[more]
bar = 2
`
	block := `[mcp_servers.skopos]
enabled = true
url = "new"

[mcp_servers.skopos.headers]
Authorization = "Bearer new"
`
	out := setTOMLSection(content, block)
	if strings.Contains(out, `"old"`) {
		t.Errorf("old value should be replaced:\n%s", out)
	}
	if !strings.Contains(out, `[other]`) || !strings.Contains(out, `foo = 1`) {
		t.Errorf("other table must be preserved:\n%s", out)
	}
	if !strings.Contains(out, `[more]`) || !strings.Contains(out, `bar = 2`) {
		t.Errorf("more table must be preserved:\n%s", out)
	}
	if !strings.Contains(out, `url = "new"`) || !strings.Contains(out, `Bearer new`) {
		t.Errorf("new block missing:\n%s", out)
	}
	// exactly one skopos table header
	if strings.Count(out, "[mcp_servers.skopos]") != 1 {
		t.Errorf("expected exactly one skopos table, got %d", strings.Count(out, "[mcp_servers.skopos]"))
	}
}

func TestMergeJSONFilePreservesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	orig := []byte(`{"mcpServers":{"other":{"url":"http://x"}},"hooks":{"Stop":[]}}`)
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	var actions []string
	entry := map[string]any{"type": "http", "url": "http://localhost:8080/mcp"}
	if err := mergeJSONFile(path, []string{"mcpServers", "skopos"}, entry, Options{}, &actions); err != nil {
		t.Fatalf("merge: %v", err)
	}

	data, _, err := readJSONMap(path)
	if err != nil {
		t.Fatal(err)
	}
	servers, ok := data["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %v", data)
	}
	if _, ok := servers["other"]; !ok {
		t.Error("existing 'other' entry should be preserved")
	}
	if _, ok := servers["skopos"]; !ok {
		t.Error("skopos entry should be added")
	}
	if _, err := os.Stat(path + ".skopos.bak"); err != nil {
		t.Error("backup file should exist")
	}
}

func TestAppendBlockActionIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	content := "some existing notes\n"

	var a1, a2 []string
	if err := appendBlockAction(path, content, Options{}, &a1); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := appendBlockAction(path, content, Options{}, &a2); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)

	if strings.Count(string(second), "<!-- skopos:begin -->") != 1 {
		t.Errorf("expected exactly one skopos block after second append, got %d", strings.Count(string(second), "<!-- skopos:begin -->"))
	}
	if string(first) != string(second) {
		t.Error("second append should not change the file (idempotent)")
	}
}

func TestResolveAgents(t *testing.T) {
	got, err := resolveAgents("all")
	if err != nil || len(got) != len(Agents) {
		t.Fatalf("all: got %v err %v", got, err)
	}
	got, err = resolveAgents("kiro")
	if err != nil || len(got) != 1 || got[0] != "kiro" {
		t.Fatalf("kiro: got %v err %v", got, err)
	}
	if _, err := resolveAgents("nope"); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	results, err := Install(Options{Agent: "kiro", URL: "http://example/mcp", APIKey: "k", Scope: "project", DryRun: true})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(results) != 1 || len(results[0].Actions) == 0 {
		t.Fatalf("expected actions, got %+v", results)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("dry-run should write nothing, got %v", entries)
	}
}
