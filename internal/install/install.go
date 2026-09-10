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

//go:embed assets/agent-block.md
var agentBlock string

//go:embed assets/claude-skill.md
var claudeSkill string

//go:embed assets/claude-explore.md
var claudeExplore string

//go:embed assets/hooks/skopos-common.sh
var hookCommon string

//go:embed assets/hooks/skopos-session.sh
var hookSession string

//go:embed assets/hooks/skopos-prompt.sh
var hookPrompt string

//go:embed assets/hooks/skopos-pre-tool.sh
var hookPreTool string

//go:embed assets/hooks/skopos-post-tool.sh
var hookPostTool string

//go:embed assets/hooks/skopos-stop.sh
var hookStop string

// renderAgentBlock specializes the shared behavioral block for one agent:
// the report_status agent_type everywhere, plus the exploration-skill
// invocation line for agents that support slash-command skills.
func renderAgentBlock(agent string) string {
	skill := ""
	if agent == "claude-code" {
		// Leading/trailing newlines give the line its own paragraph.
		skill = "\nInvoke the exploration skill directly: `Skill(skill: \"skopos\", args: \"<your query>\")`\n"
	}
	b := strings.ReplaceAll(agentBlock, "{{SKILL_LINE}}\n", skill)
	b = strings.ReplaceAll(b, "{{AGENT_TYPE}}", agent)
	if agent == "kiro" {
		// Kiro steering documents need frontmatter to be picked up at all.
		b = "---\ndescription: Skopos code index, shared memory (blackboard), plans, and agent status\nalwaysApply: true\n---\n\n" + b
	}
	return b
}

// hookScripts maps file names to their embedded sources.
var hookScripts = map[string]string{
	"skopos-common.sh":    hookCommon,
	"skopos-session.sh":   hookSession,
	"skopos-prompt.sh":    hookPrompt,
	"skopos-pre-tool.sh":  hookPreTool,
	"skopos-post-tool.sh": hookPostTool,
	"skopos-stop.sh":      hookStop,
}

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
	// Hooks installs the Claude Code hook suite (session briefing, prompt
	// pre-fetch, search nudges, edit reminders). Default on for claude-code.
	Hooks *bool
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
		// Claude Code does not read mcpServers from settings.json: user scope
		// lives in ~/.claude.json, project scope in .mcp.json at the repo root.
		// (Hooks, skills, and CLAUDE.md do belong where they are below.)
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".claude.json"), ".mcp.json")
		if err := mergeJSONFile(cfg, []string{"mcpServers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		skill := scopePath(o.Scope, filepath.Join(homeOrErr(), ".claude", "commands", "skopos-report.md"), filepath.Join(".claude", "commands", "skopos-report.md"))
		if err := writeFileAction(skill, claudeSkill, o, &r.Actions); err != nil {
			return r, err
		}
		explore := scopePath(o.Scope, filepath.Join(homeOrErr(), ".claude", "commands", "skopos.md"), filepath.Join(".claude", "commands", "skopos.md"))
		if err := writeFileAction(explore, claudeExplore, o, &r.Actions); err != nil {
			return r, err
		}
		// Always-on behavioral instructions in the global CLAUDE.md.
		claudeMd := scopePath(o.Scope, filepath.Join(homeOrErr(), ".claude", "CLAUDE.md"), "CLAUDE.md")
		if err := appendBlockAction(claudeMd, renderAgentBlock(name), o, &r.Actions); err != nil {
			return r, err
		}
		if err := installClaudeHooks(o, &r.Actions); err != nil {
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
		if err := appendBlockAction(agentsMd, renderAgentBlock(name), o, &r.Actions); err != nil {
			return r, err
		}

	case "gemini-cli":
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".gemini", "settings.json"), filepath.Join(".gemini", "settings.json"))
		if err := mergeJSONFile(cfg, []string{"mcpServers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		// Always-on behavioral instructions in the global GEMINI.md.
		geminiMd := scopePath(o.Scope, filepath.Join(homeOrErr(), ".gemini", "GEMINI.md"), "GEMINI.md")
		if err := appendBlockAction(geminiMd, renderAgentBlock(name), o, &r.Actions); err != nil {
			return r, err
		}

	case "github-copilot":
		cfg := scopePath(o.Scope, copilotGlobalMCP(), filepath.Join(".vscode", "mcp.json"))
		if err := mergeJSONFile(cfg, []string{"servers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		// Global instructions are the personal file in ~/.github; project
		// instructions live in the repo.
		instructions := scopePath(o.Scope, filepath.Join(homeOrErr(), ".github", "copilot-instructions.md"), filepath.Join(".github", "copilot-instructions.md"))
		if err := appendBlockAction(instructions, renderAgentBlock(name), o, &r.Actions); err != nil {
			return r, err
		}

	case "kiro":
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".kiro", "settings", "mcp.json"), filepath.Join(".kiro", "settings", "mcp.json"))
		if err := mergeJSONFile(cfg, []string{"mcpServers", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		// Steering is inherently project-level.
		if err := writeFileAction(filepath.Join(".kiro", "steering", "skopos.md"), renderAgentBlock(name), o, &r.Actions); err != nil {
			return r, err
		}

	case "opencode":
		cfg := scopePath(o.Scope, filepath.Join(homeOrErr(), ".config", "opencode", "opencode.json"), "opencode.json")
		if err := mergeJSONFile(cfg, []string{"mcp", "skopos"}, entry, o, &r.Actions); err != nil {
			return r, err
		}
		// Write behavioral instructions to the global AGENTS.md.
		agentsPath := scopePath(o.Scope, filepath.Join(homeOrErr(), ".config", "opencode", "AGENTS.md"), "AGENTS.md")
		if err := appendBlockAction(agentsPath, renderAgentBlock(name), o, &r.Actions); err != nil {
			return r, err
		}
	}
	return r, nil
}

// mcpEntry builds the MCP server entry for an agent's config format. The
// shapes follow each agent's documented schema — they are not interchangeable:
// Claude Code and VS Code want type+url, Gemini CLI marks streamable HTTP as
// "httpUrl" ("url" would select the SSE transport), OpenCode uses type
// "remote", and Kiro infers remote servers from "url" alone.
func mcpEntry(agent, url, apiKey string) map[string]any {
	e := map[string]any{}
	switch agent {
	case "claude-code", "github-copilot":
		e["type"] = "http"
		e["url"] = url
	case "gemini-cli":
		e["httpUrl"] = url
		e["trust"] = true
	case "opencode":
		e["type"] = "remote"
		e["url"] = url
	default: // kiro
		e["url"] = url
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

	// If the skopos entry already exists with the same URL and headers, skip
	// (idempotent). The URL key differs per agent ("url" vs "httpUrl").
	if existing := getNested(data, keyPath); existing != nil {
		if entryURL(existing) != "" && entryURL(existing) == entryURL(entry) {
			existingHeaders, _ := existing["headers"].(map[string]any)
			entryHeaders, _ := entry["headers"].(map[string]any)
			if fmt.Sprint(existingHeaders) == fmt.Sprint(entryHeaders) {
				*actions = append(*actions, fmt.Sprintf("skopos entry already up-to-date in %s", path))
				return nil
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

// entryURL returns the server URL from an MCP entry, whichever key it uses.
func entryURL(e map[string]any) string {
	if u, ok := e["url"].(string); ok {
		return u
	}
	if u, ok := e["httpUrl"].(string); ok {
		return u
	}
	return ""
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
	return writeFilePrivate(path, out)
}

// writeFilePrivate writes data with owner-only permissions. Config files
// written by the installer embed the API key, so they must not be
// world-readable. Chmod also tightens files that already exist with looser
// permissions — os.WriteFile alone does not change an existing file's mode.
func writeFilePrivate(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
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

	// %q escaping keeps quotes/backslashes in the key from breaking the TOML.
	// Codex spells static headers "http_headers" — a plain "headers" table is
	// silently ignored, which would drop the API key.
	block := fmt.Sprintf("[mcp_servers.skopos]\nenabled = true\nurl = %q\n", o.URL)
	if o.APIKey != "" {
		block += fmt.Sprintf("\n[mcp_servers.skopos.http_headers]\nAuthorization = %q\n", "Bearer "+o.APIKey)
	}
	updated := setTOMLSection(content, block)

	if updated == content {
		*actions = append(*actions, "skopos MCP block already up-to-date in "+path)
		return nil
	}
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
	if err := writeFilePrivate(path, []byte(updated)); err != nil {
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
	content = strings.TrimSpace(stripMarkers(strings.TrimSpace(content)))
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
	if updated == existing && hadBlock {
		*actions = append(*actions, "skopos block already up-to-date in "+path)
		return nil
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

// stripMarkers removes any begin/end marker comments from content so a block
// is never wrapped twice (nested markers would confuse the replacement pass).
func stripMarkers(s string) string {
	s = strings.ReplaceAll(s, "<!-- skopos:begin -->\n", "")
	s = strings.ReplaceAll(s, "<!-- skopos:end -->\n", "")
	s = strings.ReplaceAll(s, "<!-- skopos:begin -->", "")
	s = strings.ReplaceAll(s, "<!-- skopos:end -->", "")
	return s
}

func backup(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return writeFilePrivate(path+".skopos.bak", raw)
}

// hooksWanted defaults hooks on unless explicitly disabled.
func hooksWanted(o Options) bool {
	return o.Hooks == nil || *o.Hooks
}

// hookEvent wires one (event, matcher, script) triple into settings' hooks.
type hookEvent struct {
	event   string
	matcher string // "" = all tools
	script  string
}

// installClaudeHooks writes the hook scripts under ~/.claude/hooks/ and
// registers them in settings.json. Idempotent: re-running replaces our
// scripts and skips already-registered entries.
func installClaudeHooks(o Options, actions *[]string) error {
	if !hooksWanted(o) {
		return nil
	}
	home := homeOrErr()
	if home == "" {
		return fmt.Errorf("cannot resolve home directory for hook install")
	}
	hooksDir := scopePath(o.Scope, filepath.Join(home, ".claude", "hooks"), filepath.Join(".claude", "hooks"))

	// Write scripts (common first — the others source it).
	names := []string{"skopos-common.sh", "skopos-session.sh", "skopos-prompt.sh", "skopos-pre-tool.sh", "skopos-post-tool.sh", "skopos-stop.sh"}
	if !o.DryRun {
		if err := os.MkdirAll(hooksDir, 0o755); err != nil {
			return fmt.Errorf("creating hooks dir: %w", err)
		}
		for _, name := range names {
			src, ok := hookScripts[name]
			if !ok {
				return fmt.Errorf("hook script %s not embedded", name)
			}
			path := filepath.Join(hooksDir, name)
			if err := os.WriteFile(path, []byte(src), 0o755); err != nil {
				return fmt.Errorf("writing hook %s: %w", path, err)
			}
			if err := os.Chmod(path, 0o755); err != nil {
				return err
			}
		}
	}
	*actions = append(*actions, fmt.Sprintf("hook scripts written to %s (skopos-*.sh)", hooksDir))

	events := []hookEvent{
		{event: "SessionStart", script: "skopos-session.sh"},
		{event: "UserPromptSubmit", script: "skopos-prompt.sh"},
		{event: "PreToolUse", matcher: "Grep", script: "skopos-pre-tool.sh"},
		{event: "PreToolUse", matcher: "Agent", script: "skopos-pre-tool.sh"},
		{event: "PreToolUse", matcher: "Bash", script: "skopos-pre-tool.sh"},
		{event: "PostToolUse", matcher: "Edit|Write", script: "skopos-post-tool.sh"},
		{event: "Stop", script: "skopos-stop.sh"},
	}
	return mergeHookSettings(scopePath(o.Scope, filepath.Join(home, ".claude", "settings.json"), filepath.Join(".claude", "settings.json")), hooksDir, events, o, actions)
}

// mergeHookSettings adds the hook entries to settings.json, preserving all
// existing hooks and other keys. Idempotent per (event, matcher, command).
func mergeHookSettings(path, hooksDir string, events []hookEvent, o Options, actions *[]string) error {
	data, existed, err := readJSONMap(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	hooksAny, ok := data["hooks"]
	var hooks map[string]any
	if ok {
		hooks, ok = hooksAny.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: hooks key is not an object", path)
		}
	} else {
		hooks = map[string]any{}
		data["hooks"] = hooks
	}

	added := 0
	for _, ev := range events {
		command := filepath.Join(hooksDir, ev.script)
		entryListAny, ok := hooks[ev.event].([]any)
		if !ok {
			entryListAny = []any{}
		}
		// Idempotency: skip only an identical (matcher, command) pair under
		// this event — the same script legitimately registers under several
		// matchers (Grep + Agent + Bash).
		found := false
		for _, eAny := range entryListAny {
			e, ok := eAny.(map[string]any)
			if !ok {
				continue
			}
			if m, _ := e["matcher"].(string); m != ev.matcher {
				continue
			}
			hs, ok := e["hooks"].([]any)
			if !ok {
				continue
			}
			for _, hAny := range hs {
				h, ok := hAny.(map[string]any)
				if !ok {
					continue
				}
				if c, _ := h["command"].(string); c == command {
					found = true
				}
			}
		}
		if found {
			continue
		}
		entry := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		}
		if ev.matcher != "" {
			entry["matcher"] = ev.matcher
		}
		hooks[ev.event] = append(entryListAny, entry)
		added++
	}

	if added == 0 && existed {
		*actions = append(*actions, "hook registrations already up-to-date in "+path)
		return nil
	}
	if o.DryRun {
		verb := "would register"
		if !existed {
			verb = "would create"
		}
		*actions = append(*actions, fmt.Sprintf("%s skopos hooks in %s (%d entries)", verb, path, added))
		return nil
	}
	if existed {
		if err := backup(path); err != nil {
			return fmt.Errorf("backing up %s: %w", path, err)
		}
	}
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	*actions = append(*actions, fmt.Sprintf("registered %d skopos hook entries in %s", added, path))
	return nil
}
