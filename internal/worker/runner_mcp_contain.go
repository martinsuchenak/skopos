package worker

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
)

// An MCP command server is a process the agent CLI starts itself. A
// wrapper, or a host that is a container, holds it with the rest of the
// run. The runners' own sandboxes do not: Claude Code's sandbox settings
// and codex's sandbox confine the agent's shell, and the CLI starts its
// servers outside them. So where the run's only boundary is such a
// sandbox, a command server would undo it, and the run gets its http
// servers only. Where nothing claims to contain the run, the server runs
// as the agent itself does. Which of these a run gets is the configurator's
// choice: a wrapper or contained_by gives every runner its command servers.

// commandServersEscape says why the run's MCP command servers would run
// outside its boundary ("" = they would not).
func commandServersEscape(r Runner, ar AgentRun) string {
	if ar.wholeRunContained() {
		return ""
	}
	if _, ok := r.(codexRunner); ok {
		return "codex starts it outside the sandbox it keeps the agent's shell in"
	}
	if ar.Sandboxed && r.HonorsSettings() {
		return "the run's sandbox settings confine the agent's shell, and the CLI starts it outside them"
	}
	return ""
}

// withoutCommandServers writes a copy of an MCP config file without its
// command servers into dir, and says which were left out. With none to
// leave out it returns the file itself.
func withoutCommandServers(file, dir, why string) (string, []string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", nil, fmt.Errorf("mcp_config: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", nil, fmt.Errorf("mcp_config %s: %v", file, err)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(doc["mcpServers"], &servers); err != nil && doc["mcpServers"] != nil {
		return "", nil, fmt.Errorf("mcp_config %s: %v", file, err)
	}
	var notes []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		var s struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(servers[name], &s) == nil && s.Command != "" {
			delete(servers, name)
			notes = append(notes, fmt.Sprintf("mcp server %s: it is a command, and %s; nothing contains the whole run (a wrapper, or a host that is a container): left out", name, why))
		}
	}
	if len(notes) == 0 {
		return file, nil, nil
	}
	b, err := json.Marshal(servers)
	if err != nil {
		return "", nil, err
	}
	doc["mcpServers"] = b
	out, err := json.Marshal(doc)
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(dir, "skopos-mcp-*.json")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), notes, nil
}
