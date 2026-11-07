package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Pipeline agents reach skopos over MCP with the worker's agent key
// (tmp/agent-mcp-and-indexing — design.md §2): a capped, MCP-only key, so
// the agent gets the code index, the blackboard and its plan's steps —
// never the REST API (item transitions, runs, keys) the worker's own key
// can use.

// agentMCPServer is the server name runs see the skopos tools under
// (mcp__skopos__<tool>).
const agentMCPServer = "skopos"

// skoposReadTools are the tools every stage gets.
var skoposReadTools = []string{
	"skopos_context", "blackboard_read", "blackboard_write", "plan_read",
	"code_search", "code_find", "code_symbol", "code_outline", "code_callers", "code_callees",
	"code_call_tree", "code_impact", "code_dependencies", "code_branch_diff", "code_index_status",
}

// skoposTools are the mcp__skopos__* names a stage may call: every stage
// reads the index and the blackboard and records findings; implement also
// marks its plan's steps.
func skoposTools(phase string) []string {
	names := skoposReadTools
	if phase == "implement" {
		names = append(slices.Clone(names), "plan_update_item")
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "mcp__" + agentMCPServer + "__" + n
	}
	return out
}

// runnerHasSkopos reports whether a runner gets the skopos MCP server:
// claude and opencode load the run's MCP config; codex runs with the network
// off and no MCP (read-only stages, design §2).
func runnerHasSkopos(runner string) bool {
	return runner == "" || runner == "claude" || runner == "opencode"
}

// CheckAgentKey asks the server what the agent key may do and refuses one
// that would hand an agent more than MCP: it must be a capped key with the
// mcp surface, without rest, without the approver permission, and able to
// read the code index.
func CheckAgentKey(ctx context.Context, base, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/whoami", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("agent key: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent key: whoami answered %s", resp.Status)
	}
	var who struct {
		Root bool `json:"root"`
		Key  *struct {
			Approver     bool     `json:"approver"`
			Capabilities []string `json:"capabilities"`
		} `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		return fmt.Errorf("agent key: whoami: %w", err)
	}
	const mint = "mint one with: skopos key create --name <host>-agent --workspace <id> --preset agent"
	switch {
	case who.Root || who.Key == nil:
		return fmt.Errorf("agent key is the root key — %s", mint)
	case who.Key.Capabilities == nil:
		return fmt.Errorf("agent key has full access (no capabilities) — an agent would get the REST API; %s", mint)
	case who.Key.Approver:
		return fmt.Errorf("agent key holds the approver permission — %s", mint)
	case slices.Contains(who.Key.Capabilities, "rest"):
		return fmt.Errorf("agent key may use the REST API — %s", mint)
	case !slices.Contains(who.Key.Capabilities, "mcp"):
		return fmt.Errorf("agent key may not use MCP — %s", mint)
	case !slices.Contains(who.Key.Capabilities, "code:read") && !slices.Contains(who.Key.Capabilities, "code:write"):
		return fmt.Errorf("agent key cannot read the code index — %s", mint)
	}
	return nil
}

// WriteAgentMCPConfig writes the MCP config every run gets (0600, in the
// state dir, outside any worktree): the operator's mcp_config servers plus
// the skopos server with the agent key. It returns the file's path.
func WriteAgentMCPConfig(stateDir, base, key, operatorConfig string) (string, error) {
	servers := map[string]any{}
	if operatorConfig != "" {
		raw, err := os.ReadFile(operatorConfig)
		if err != nil {
			return "", fmt.Errorf("mcp_config: %w", err)
		}
		var in struct {
			MCPServers map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("mcp_config %s: %w", operatorConfig, err)
		}
		for name, s := range in.MCPServers {
			if name == agentMCPServer {
				return "", fmt.Errorf("mcp_config %s defines a %q server; with an agent key the worker provides it — remove it from mcp_config", operatorConfig, agentMCPServer)
			}
			servers[name] = s
		}
	}
	servers[agentMCPServer] = map[string]any{
		"type":    "http",
		"url":     strings.TrimRight(base, "/") + "/mcp",
		"headers": map[string]string{"Authorization": "Bearer " + key},
	}
	b, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, "agent-mcp.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// EnableAgentMCP gives every run of a claude or opencode agent the MCP
// config at path (WriteAgentMCPConfig) and the skopos tools of its stage.
func (w *Worker) EnableAgentMCP(path string) { w.agentMCP = path }

// skoposPrompt is what a stage prompt says about skopos: nil when the run
// has no skopos tools (no agent key, or a codex run). branch is the branch
// whose index fits the stage (the item branch for review, the base
// otherwise); planID and steps let implement mark its steps.
func (w *Worker) skoposPrompt(run *Run, account *Account, base, branch, phase, planID string, steps []Step) map[string]any {
	if w.agentMCP == "" || !runnerHasSkopos(account.Runner) {
		return nil
	}
	runner := account.Runner
	if runner == "" {
		runner = "claude"
	}
	return map[string]any{
		"Workspace": run.WorkspaceID, "Base": base, "Branch": branch,
		"Author":    runner + "@" + run.ID,
		"Implement": phase == "implement", "PlanID": planID, "Steps": steps,
	}
}
