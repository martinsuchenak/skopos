package install

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed assets/kiro-steering.md
var kiroSteering string

//go:embed assets/claude-skill.md
var claudeSkill string

//go:embed assets/claude-claude.md
var claudeClaude string

//go:embed assets/gemini-instructions.md
var geminiInstructions string

//go:embed assets/opencode-agents.md
var opencodeAgents string

//go:embed assets/codex-agents.md
var codexAgents string

//go:embed assets/copilot-instructions.md
var copilotInstructions string

// Agents is the set of supported install targets.
var Agents = []string{"claude-code", "codex", "gemini-cli", "github-copilot", "kiro", "opencode"}

const DefaultURL = "http://localhost:8080/mcp"

// Options configures an install run.
type Options struct {
	Agent  string // one of Agents, or "all"
	URL    string // MCP server URL (default DefaultURL)
	APIKey string // sent as Authorization: Bearer; empty omits the header
	Scope  string // "global" (default) or "project"
	DryRun bool
}

// Result describes what one agent install did (or would do, when DryRun).
type Result struct {
	Agent   string
	Actions []string
}

// Install runs the installer for the requested agent(s).
func Install(o Options) ([]Result, error) {
	targets, err := resolveAgents(o.Agent)
	if err != nil {
		return nil, err
	}
	if o.URL == "" {
		o.URL = DefaultURL
	}
	if o.Scope == "" {
		o.Scope = "global"
	}
	var results []Result
	for _, a := range targets {
		r, err := installAgent(a, o)
		if err != nil {
			return results, fmt.Errorf("%s: %w", a, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func resolveAgents(agent string) ([]string, error) {
	if agent == "all" {
		return Agents, nil
	}
	for _, a := range Agents {
		if a == agent {
			return []string{agent}, nil
		}
	}
	return nil, fmt.Errorf("unknown agent %q (valid: %s, all)", agent, strings.Join(Agents, ", "))
}

func installAgent(name string, o Options) (Result, error) {
	r := Result{Agent: name}
	entry := mcpEntry(name, o.URL, o.APIKey)

	switch name {
	case "claude-code":
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".claude", "settings.json"), filepath.Join(".claude", "settings.json"))
		if err := mergeJSONFile(cfg, []string{"mcpServers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		skill := scopePath(o.Scope, filepath.Join(homeOrErr(), ".claude", "commands", "skopos-report.md"), filepath.Join(".claude", "commands", "skopos-report.md"))
		if err := writeFileAction(skill, claudeSkill, o, &r.Actions); err != nil {
			return r, err
		}
		// Always-on behavioral instructions in the global CLAUDE.md.
		claudeMd := scopePath(o.Scope, filepath.Join(homeOrErr(), ".claude", "CLAUDE.md"), "CLAUDE.md")
		if err := appendBlockAction(claudeMd, claudeClaude, o, &r.Actions); err != nil {
			return r, err
		}

	case "codex":
		// Codex config is global only.
		cfg := filepath.Join(homeOrErr(), ".codex", "config.toml")
		if err := mergeCodexTOML(cfg, o, &r.Actions); err != nil {
			return r, err
		}
		// Always-on behavioral instructions in the global ~/AGENTS.md.
		agentsMd := scopePath(o.Scope, filepath.Join(homeOrErr(), "AGENTS.md"), "AGENTS.md")
		if err := appendBlockAction(agentsMd, codexAgents, o, &r.Actions); err != nil {
			return r, err
		}

	case "gemini-cli":
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".gemini", "settings.json"), filepath.Join(".gemini", "settings.json"))
		if err := mergeJSONFile(cfg, []string{"mcpServers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		// Always-on behavioral instructions in the global GEMINI.md.
		geminiMd := scopePath(o.Scope, filepath.Join(homeOrErr(), ".gemini", "GEMINI.md"), "GEMINI.md")
		if err := appendBlockAction(geminiMd, geminiInstructions, o, &r.Actions); err != nil {
			return r, err
		}

	case "github-copilot":
		cfg := scopePath(o.Scope, copilotGlobalMCP(), filepath.Join(".vscode", "mcp.json"))
		if err := mergeJSONFile(cfg, []string{"servers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		if err := appendBlockAction(filepath.Join(".github", "copilot-instructions.md"), copilotInstructions, o, &r.Actions); err != nil {
			return r, err
		}

	case "kiro":
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".kiro", "settings", "mcp.json"), filepath.Join(".kiro", "settings", "mcp.json"))
		if err := mergeJSONFile(cfg, []string{"mcpServers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		// Steering is inherently project-level.
		if err := writeFileAction(filepath.Join(".kiro", "steering", "skopos.md"), kiroSteering, o, &r.Actions); err != nil {
			return r, err
		}

	case "opencode":
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".config", "opencode", "opencode.json"), "opencode.json")
		if err := mergeJSONFile(cfg, []string{"mcp", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		// Write behavioral instructions to the global AGENTS.md.
		agentsPath := scopePath(o.Scope, filepath.Join(homeOrErr(), ".config", "opencode", "AGENTS.md"), "AGENTS.md")
		if err := appendBlockAction(agentsPath, opencodeAgents, o, &r.Actions); err != nil {
			return r, err
		}
	}
	return r, nil
}

// mcpEntry builds the MCP server entry for an agent's config format.
func mcpEntry(agent, url, apiKey string) map[string]any {
	e := map[string]any{"url": url}
	switch agent {
	case "claude-code", "github-copilot":
		e["type"] = "http"
	case "gemini-cli":
		e["type"] = "http"
		e["trust"] = true
	case "opencode":
		e["type"] = "remote"
	}
	if apiKey != "" {
		e["headers"] = map[string]any{"Authorization": "Bearer " + apiKey}
	}
	return e
}

func scopePath(scope, globalRel, projectRel string) string {
	if scope == "project" {
		return projectRel
	}
	return globalRel
}

func copilotGlobalMCP() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(homeOrErr(), "Library", "Application Support", "Code", "User", "mcp.json")
	}
	return filepath.Join(homeOrErr(), ".config", "Code", "User", "mcp.json")
}

func homeOrErr() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// ---- JSON merge ----

func mergeJSONFile(path string, keyPath []string, entry map[string]any, o Options, actions *[]string) error {
	// Detect merge-conflict markers or other corruption before touching the file.
	raw, err := os.ReadFile(path)
	if err == nil && strings.Contains(string(raw), "<<<<<<<") {
		return fmt.Errorf("%s has merge conflict markers — resolve them first, then re-run install", path)
	}

	data, existed, err := readJSONMap(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	// If the skopos entry already exists with the same URL, skip (idempotent).
	if existing := getNested(data, keyPath); existing != nil {
		if existingURL, ok := existing["url"].(string); ok && existingURL == entry["url"].(string) {
			if existingHeaders, ok2 := existing["headers"].(map[string]any); ok2 {
				entryHeaders, _ := entry["headers"].(map[string]any)
				if fmt.Sprint(existingHeaders) == fmt.Sprint(entryHeaders) {
					*actions = append(*actions, fmt.Sprintf("skopos entry already up-to-date in %s", path))
					return nil
				}
			}
		}
	}

	setNested(data, keyPath, entry)

	if o.DryRun {
		verb := "would merge"
		if !existed {
			verb = "would create"
		}
		*actions = append(*actions, fmt.Sprintf("%s skopos MCP entry into %s", verb, path))
		return nil
	}
	if existed {
		if err := backup(path); err != nil {
			return fmt.Errorf("backing up %s: %w", path, err)
		}
	}
	if err := writeJSON(path, data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	*actions = append(*actions, fmt.Sprintf("merged skopos MCP entry into %s", path))
	return nil
}

func readJSONMap(path string) (map[string]any, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, false, nil
		}
		return nil, false, err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, true, err
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, true, nil
}

func writeJSON(path string, data map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

func setNested(root map[string]any, path []string, val any) {
	cur := root
	for i := 0; i < len(path)-1; i++ {
		next, ok := cur[path[i]].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[path[i]] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = val
}

func getNested(root map[string]any, path []string) map[string]any {
	cur := root
	for i := 0; i < len(path)-1; i++ {
		next, ok := cur[path[i]].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	result, ok := cur[path[len(path)-1]].(map[string]any)
	if !ok {
		return nil
	}
	return result
}

// ---- Codex TOML (section replace) ----

func mergeCodexTOML(path string, o Options, actions *[]string) error {
	content := ""
	existed := false
	if raw, err := os.ReadFile(path); err == nil {
		content = string(raw)
		existed = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	block := fmt.Sprintf("[mcp_servers.skopos]\nenabled = true\nurl = %q\n", o.URL)
	if o.APIKey != "" {
		block += fmt.Sprintf("\n[mcp_servers.skopos.headers]\nAuthorization = \"Bearer %s\"\n", o.APIKey)
	}
	updated := setTOMLSection(content, block)

	if o.DryRun {
		verb := "would merge"
		if !existed {
			verb = "would create"
		}
		*actions = append(*actions, fmt.Sprintf("%s skopos MCP block into %s", verb, path))
		return nil
	}
	if existed {
		if err := backup(path); err != nil {
			return fmt.Errorf("backing up %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	*actions = append(*actions, fmt.Sprintf("merged skopos MCP block into %s", path))
	return nil
}

// setTOMLSection replaces the [mcp_servers.skopos] block (including its .headers
// subtable) with block, or appends it if absent. Everything else is preserved.
func setTOMLSection(content, block string) string {
	lines := strings.Split(content, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "[mcp_servers.skopos]" {
			start = i
			break
		}
	}
	if start == -1 {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return content + block
	}
	end := start + 1
	for end < len(lines) {
		t := strings.TrimSpace(lines[end])
		if strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[mcp_servers.skopos") {
			break
		}
		end++
	}
	before := strings.Join(lines[:start], "\n")
	after := strings.Join(lines[end:], "\n")
	res := before
	if res != "" && !strings.HasSuffix(res, "\n") {
		res += "\n"
	}
	res += block
	if after != "" {
		if !strings.HasSuffix(res, "\n") {
			res += "\n"
		}
		res += after
	}
	return res
}

// ---- behavioral file copies / appends ----

func writeFileAction(path, content string, o Options, actions *[]string) error {
	if o.DryRun {
		*actions = append(*actions, fmt.Sprintf("would write %s", path))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	*actions = append(*actions, fmt.Sprintf("wrote %s", path))
	return nil
}

// appendBlockAction idempotently inserts content into a markdown file between
// skopos marker comments, appending if absent.
func appendBlockAction(path, content string, o Options, actions *[]string) error {
	const begin = "<!-- skopos:begin -->"
	const end = "<!-- skopos:end -->"
	content = strings.TrimRight(content, "\n")
	inner := begin + "\n" + content + "\n" + end + "\n"

	existing := ""
	if raw, err := os.ReadFile(path); err == nil {
		existing = string(raw)
	} else if !os.IsNotExist(err) {
		return err
	}

	updated := existing
	hadBlock := strings.Contains(existing, begin)
	if hadBlock {
		updated = replaceMarker(existing, begin, end, inner)
	} else {
		if existing != "" && !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		updated = existing + inner
	}

	if o.DryRun {
		verb := "would write"
		if hadBlock {
			verb = "would update"
		}
		*actions = append(*actions, fmt.Sprintf("%s skopos block into %s", verb, path))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return err
	}
	*actions = append(*actions, fmt.Sprintf("updated skopos block in %s", path))
	return nil
}

// replaceMarker swaps the content between begin/end markers (consuming the
// newline after end) with inner.
func replaceMarker(s, begin, end, inner string) string {
	bi := strings.Index(s, begin)
	if bi < 0 {
		return s
	}
	rel := strings.Index(s[bi:], end)
	if rel < 0 {
		return s[:bi] + inner
	}
	ei := bi + rel + len(end)
	if ei < len(s) && s[ei] == '\n' {
		ei++
	}
	return s[:bi] + inner + s[ei:]
}

func backup(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".skopos.bak", raw, 0o644)
}
