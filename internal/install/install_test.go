package install

import (
	"fmt"
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

[mcp_servers.skopos.http_headers]
Authorization = "Bearer old"

[more]
bar = 2
`
	block := `[mcp_servers.skopos]
enabled = true
url = "new"

[mcp_servers.skopos.http_headers]
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

func TestSetTOMLSectionMigratesLegacyHeadersSubtable(t *testing.T) {
	// Installs from before the http_headers fix wrote [mcp_servers.skopos.headers];
	// re-running install must replace that subtable, not leave it behind.
	content := `[mcp_servers.skopos]
url = "old"

[mcp_servers.skopos.headers]
Authorization = "Bearer old"
`
	block := `[mcp_servers.skopos]
enabled = true
url = "new"
`
	out := setTOMLSection(content, block)
	if strings.Contains(out, "mcp_servers.skopos.headers") || strings.Contains(out, `"old"`) {
		t.Errorf("legacy headers subtable should be removed:\n%s", out)
	}
}

func TestMCEntryShapes(t *testing.T) {
	const url = "http://localhost:8080/mcp"
	claude := mcpEntry("claude-code", url, "k")
	if claude["type"] != "http" || claude["url"] != url {
		t.Errorf("claude-code: %v", claude)
	}
	if h := claude["headers"].(map[string]any)["Authorization"]; h != "Bearer k" {
		t.Errorf("claude-code headers: %v", h)
	}
	gemini := mcpEntry("gemini-cli", url, "k")
	// "url" selects the SSE transport in Gemini CLI; streamable HTTP is "httpUrl".
	if _, ok := gemini["url"]; ok {
		t.Errorf("gemini-cli must not use url: %v", gemini)
	}
	if _, ok := gemini["type"]; ok {
		t.Errorf("gemini-cli has no type key: %v", gemini)
	}
	if gemini["httpUrl"] != url || gemini["trust"] != true {
		t.Errorf("gemini-cli: %v", gemini)
	}
	kiro := mcpEntry("kiro", url, "")
	// Kiro infers remote servers from url alone; a type key is not part of its schema.
	if _, ok := kiro["type"]; ok {
		t.Errorf("kiro must not have type: %v", kiro)
	}
	if kiro["url"] != url {
		t.Errorf("kiro: %v", kiro)
	}
	oc := mcpEntry("opencode", url, "")
	if oc["type"] != "remote" || oc["url"] != url {
		t.Errorf("opencode: %v", oc)
	}
	cp := mcpEntry("github-copilot", url, "")
	if cp["type"] != "http" || cp["url"] != url {
		t.Errorf("github-copilot: %v", cp)
	}
}

func TestMergeJSONFileIdempotentAcrossURLKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	existing := `{"mcpServers":{"skopos":{"httpUrl":"http://localhost:8080/mcp","trust":true}}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	var actions []string
	entry := mcpEntry("gemini-cli", "http://localhost:8080/mcp", "")
	if err := mergeJSONFile(path, []string{"mcpServers", "skopos"}, entry, Options{}, &actions); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(actions) == 0 || !strings.Contains(actions[0], "already up-to-date") {
		t.Fatalf("expected idempotent skip, got %v", actions)
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

	// Config files can embed the API key: both the file and its backup must be
	// owner-only, including when the file already existed with wider perms.
	for _, p := range []string{path, path + ".skopos.bak"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s: expected mode 0600, got %o", p, info.Mode().Perm())
		}
	}
}

func TestMergeCodexTOMLEscapesAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	var actions []string
	o := Options{URL: "http://localhost:8080/mcp", APIKey: `ab"cd`}
	if err := mergeCodexTOML(path, o, &actions); err != nil {
		t.Fatalf("merge: %v", err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A key containing a double quote must be escaped so the TOML stays parseable.
	if !strings.Contains(string(out), `Authorization = "Bearer ab\"cd"`) {
		t.Errorf("expected escaped Authorization header, got:\n%s", out)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected mode 0600, got %o", info.Mode().Perm())
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

func TestAppendBlockActionStripsNestedMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	// Content that already carries markers must not be wrapped a second time.
	content := "<!-- skopos:begin -->\ninner text\n<!-- skopos:end -->\n"
	var actions []string
	if err := appendBlockAction(path, content, Options{}, &actions); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	if strings.Count(string(out), "<!-- skopos:begin -->") != 1 || strings.Count(string(out), "<!-- skopos:end -->") != 1 {
		t.Errorf("expected exactly one marker pair, got:\n%s", out)
	}
}

func TestAgentBlockRenders(t *testing.T) {
	for _, agent := range Agents {
		b := renderAgentBlock(agent)
		if strings.Contains(b, "{{") {
			t.Errorf("%s: unresolved placeholder in block:\n%s", agent, b)
		}
		for _, want := range []string{
			"<!-- skopos:version:",
			"Mandatory: Code exploration via skopos",
			"**Do NOT**",
			"`skopos mode`",
			fmt.Sprintf("agent_type %q", agent),
		} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: block missing %q", agent, want)
			}
		}
		// Markers are owned by appendBlockAction, never the asset itself.
		if strings.Contains(b, "skopos:begin") || strings.Contains(b, "skopos:end") {
			t.Errorf("%s: block must not embed markers", agent)
		}
	}
	claude := renderAgentBlock("claude-code")
	if !strings.Contains(claude, `Skill(skill: "skopos"`) {
		t.Error("claude-code block should reference the exploration skill")
	}
	codex := renderAgentBlock("codex")
	if strings.Contains(codex, `Skill(skill: "skopos"`) {
		t.Error("non-claude block should not reference the Claude skill")
	}
	kiro := renderAgentBlock("kiro")
	// Kiro steering needs frontmatter to be picked up by the IDE.
	if !strings.HasPrefix(kiro, "---\n") || !strings.Contains(kiro, "alwaysApply: true") {
		t.Errorf("kiro steering missing frontmatter:\n%s", kiro[:min(200, len(kiro))])
	}
}

func TestInstallClaudeCodeGlobalLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	if _, err := Install(Options{Agent: "claude-code", URL: "http://localhost:8080/mcp", APIKey: "k"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	// MCP entry goes to ~/.claude.json (user scope), not settings.json.
	claudeJSON, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("expected %s to be written: %v", filepath.Join(home, ".claude.json"), err)
	}
	data, _, err := readJSONMap(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry := getNested(data, []string{"mcpServers", "skopos"})
	if entry == nil || entry["type"] != "http" || entry["url"] != "http://localhost:8080/mcp" {
		t.Fatalf("mcpServers.skopos missing or wrong shape in ~/.claude.json: %s", claudeJSON)
	}

	// settings.json holds hooks only — Claude Code ignores mcpServers there.
	settings, _, err := readJSONMap(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	if _, ok := settings["mcpServers"]; ok {
		t.Error("settings.json must not carry mcpServers")
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok || len(hooks) == 0 {
		t.Fatalf("hooks missing from settings.json: %v", settings)
	}

	// CLAUDE.md gets exactly one managed block.
	claudeMd, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(claudeMd), "<!-- skopos:begin -->") != 1 {
		t.Errorf("expected exactly one block marker, got:\n%s", claudeMd)
	}
	if !strings.Contains(string(claudeMd), "Mandatory: Code exploration via skopos") {
		t.Errorf("CLAUDE.md missing mandatory block:\n%s", claudeMd)
	}

	// Both slash commands ship: /skopos-report and /skopos.
	for _, c := range []string{"skopos-report.md", "skopos.md"} {
		if _, err := os.Stat(filepath.Join(home, ".claude", "commands", c)); err != nil {
			t.Errorf("command %s missing: %v", c, err)
		}
	}
}

func TestInstallCopilotGlobalInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Copy the global MCP file location decision: instructions go personal-global.
	if _, err := Install(Options{Agent: "github-copilot", URL: "http://localhost:8080/mcp", Scope: "global"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".github", "copilot-instructions.md")); err != nil {
		t.Errorf("personal instructions missing: %v", err)
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
