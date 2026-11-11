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

	"github.com/martinsuchenak/skopos/internal/srvurl"
)

// Pipeline agents reach skopos over MCP: the code index, the blackboard and
// their plan's steps — never the REST API (item transitions, runs, keys)
// the worker's own key can use.
//
// The credential is the run's own agent token, minted by the claim
// (tmp/central-config — design.md, phase 1b): valid while this attempt
// runs, scoped to the run's workspace. The worker writes it into a per-run
// MCP config (prepareRunMCP). A server from before run tokens sends none;
// then the worker's agent key is used, if it has one
// (tmp/agent-mcp-and-indexing — design.md §2): a capped, MCP-only key
// shared by every run on the worker.

// agentMCPServer is the server name runs see the skopos tools under
// (mcp__skopos__<tool>).
const agentMCPServer = "skopos"

// skoposReadTools are the tools every stage gets.
var skoposReadTools = []string{
	"skopos_context", "blackboard_read", "blackboard_write", "plan_read",
	"code_search", "code_find", "code_symbol", "code_outline", "code_callers", "code_callees",
	"code_call_tree", "code_impact", "code_dependencies", "code_branch_diff", "code_index_status",
}

// AgentToolNames are the skopos tools (bare names) a stage's agent may
// call. The server allows a run's agent token exactly these (cmd/mcp
// runTools; a test there keeps the two in step).
func AgentToolNames(phase string) []string {
	names := slices.Clone(skoposReadTools)
	if phase == "implement" {
		names = append(names, "plan_update_item")
	}
	return names
}

// skoposTools are the mcp__skopos__* names a stage may call: every stage
// reads the index and the blackboard and records findings; implement also
// marks its plan's steps.
func skoposTools(phase string) []string {
	names := AgentToolNames(phase)
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "mcp__" + agentMCPServer + "__" + n
	}
	return out
}

// runnerHasSkopos reports whether a runner gets the skopos MCP server, by
// its kind (a runner named "claude-max" with kind = "claude" does): claude
// and opencode load the run's MCP config; codex runs with the network off
// and no MCP (read-only stages, design §2).
func (c *Config) runnerHasSkopos(runner string) bool {
	_, kind := c.runnerSpec(runner)
	return kind == "claude" || kind == "opencode"
}

// whoamiAnswer is what GET /api/whoami says about a key.
type whoamiAnswer struct {
	Root bool `json:"root"`
	Key  *struct {
		Approver     bool     `json:"approver"`
		Admin        bool     `json:"admin"`
		Capabilities []string `json:"capabilities"`
	} `json:"key"`
}

// whoami asks the server what key may do.
func whoami(ctx context.Context, base, key string) (*whoamiAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/whoami", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("whoami answered %s", resp.Status)
	}
	var who whoamiAnswer
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		return nil, fmt.Errorf("whoami: %w", err)
	}
	return &who, nil
}

// CheckAgentKey asks the server what the agent key may do and refuses one
// that would hand an agent more than MCP: it must be a capped key with the
// mcp surface, without rest, without the approver or admin permission, and
// able to read the code index.
func CheckAgentKey(ctx context.Context, base, key string) error {
	who, err := whoami(ctx, base, key)
	if err != nil {
		return fmt.Errorf("agent key: %w", err)
	}
	const mint = "mint one with: skopos key create --name <host>-agent --workspace <id> --preset agent"
	switch {
	case who.Root || who.Key == nil:
		return fmt.Errorf("agent key is the root key — %s", mint)
	case who.Key.Capabilities == nil:
		return fmt.Errorf("agent key has full access (no capabilities) — an agent would get the REST API; %s", mint)
	case who.Key.Approver:
		return fmt.Errorf("agent key holds the approver permission — %s", mint)
	case who.Key.Admin:
		return fmt.Errorf("agent key holds the admin permission — %s", mint)
	case slices.Contains(who.Key.Capabilities, "rest"):
		return fmt.Errorf("agent key may use the REST API — %s", mint)
	case !slices.Contains(who.Key.Capabilities, "mcp"):
		return fmt.Errorf("agent key may not use MCP — %s", mint)
	case !slices.Contains(who.Key.Capabilities, "code:read") && !slices.Contains(who.Key.Capabilities, "code:write"):
		return fmt.Errorf("agent key cannot read the code index — %s", mint)
	}
	return nil
}

// CheckWorkerKey asks the server what the worker's own key may do and
// refuses one that holds the admin permission: a worker executes what
// policy and configuration say, so the credential on its host must not be
// able to change them. The root key and an approver key still work, with a
// warning each (the returned strings) — existing workers run with them, and
// a scoped key without either is what a worker should have. A server that
// cannot be asked is not a reason to refuse; the claim loop reports that.
func CheckWorkerKey(ctx context.Context, base, key string) (warnings []string, err error) {
	if strings.TrimSpace(key) == "" {
		return nil, nil // a server without authentication
	}
	who, werr := whoami(ctx, base, key)
	if werr != nil {
		return nil, nil
	}
	switch {
	case who.Root || who.Key == nil:
		return []string{"the worker runs with the root key; give it a scoped key for its workspaces (skopos key create --workspace <id>)"}, nil
	case who.Key.Admin:
		return nil, fmt.Errorf("the worker's key holds the admin permission; a worker must not be able to change policy or configuration — remove it (skopos key edit <id> --no-admin) or mint a plain scoped key")
	case who.Key.Approver:
		return []string{"the worker's key holds the approver permission; a worker key should not (skopos key edit <id> --no-approver)"}, nil
	}
	return nil, nil
}

// mcpConfigJSON builds the MCP config a run gets: the operator's mcp_config
// servers plus the skopos server with bearer as its credential. strict
// refuses an operator file that defines a server named skopos (the agent
// key path, where the worker has always provided it); otherwise the
// worker's entry replaces it — a run's own token is the narrower
// credential.
func mcpConfigJSON(base, bearer, operatorConfig string, strict bool) ([]byte, error) {
	servers := map[string]any{}
	if operatorConfig != "" {
		raw, err := os.ReadFile(operatorConfig)
		if err != nil {
			return nil, fmt.Errorf("mcp_config: %w", err)
		}
		var in struct {
			MCPServers map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("mcp_config %s: %w", operatorConfig, err)
		}
		for name, s := range in.MCPServers {
			if name == agentMCPServer && strict {
				return nil, fmt.Errorf("mcp_config %s defines a %q server; with an agent key the worker provides it — remove it from mcp_config", operatorConfig, agentMCPServer)
			}
			servers[name] = s
		}
	}
	// Agents can't look SRV up: a srv+ server URL is resolved now (the
	// file is written per run, or rewritten before each run).
	mcpURL, err := srvurl.Resolve(context.Background(), strings.TrimRight(base, "/")+"/mcp")
	if err != nil {
		return nil, fmt.Errorf("agent mcp config: %w", err)
	}
	servers[agentMCPServer] = map[string]any{
		"type":    "http",
		"url":     mcpURL,
		"headers": map[string]string{"Authorization": "Bearer " + bearer},
	}
	return json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
}

// writeFile0600 replaces path atomically with mode 0600; a failed write
// leaves no temporary file behind (it would hold a credential).
func writeFile0600(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := writeNoFollow(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// WriteAgentMCPConfig writes the MCP config runs get when the server issues
// no run tokens (0600, in the state dir, outside any worktree): the
// operator's mcp_config servers plus the skopos server with the agent key.
// It returns the file's path.
func WriteAgentMCPConfig(stateDir, base, key, operatorConfig string) (string, error) {
	b, err := mcpConfigJSON(base, key, operatorConfig, true)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, "agent-mcp.json")
	return path, writeFile0600(path, b)
}

// prepareRunMCP gives a run that came with an agent token its own MCP
// config: <state_dir>/runs/<run>/mcp.json (the directory 0700, the file
// 0600), holding the operator's servers plus skopos with the run's token.
// The token joins the redacted values for the run. The returned func
// removes the file and forgets the token; it is never nil.
//
// A run that came with a token never uses the shared agent key: when its
// own config cannot be written, its agents get no skopos tools (mcpFor).
// Without a token (a server from before run tokens) nothing is written,
// and the run falls back to the agent-key config, if the worker has one.
func (w *Worker) prepareRunMCP(run *Run) (func(), error) {
	if run.AgentToken == "" {
		return func() {}, nil
	}
	w.tokenRun, w.runMCP, w.runSecrets = run.ID, "", []string{run.AgentToken}
	forget := func() { w.tokenRun, w.runMCP, w.runSecrets = "", "", nil }
	if !w.runTokensSeen {
		w.runTokensSeen = true
		if w.agentMCP != "" {
			w.log.Info("this server issues run tokens: agent runs use them, and SKOPOS_AGENT_KEY is no longer needed")
		}
	}
	// The id becomes a directory name that is chmodded and holds a
	// credential: only a plain name will do, whatever the server sent.
	if !plainRunID(run.ID) {
		return forget, fmt.Errorf("run id %q is not usable as a directory name", run.ID)
	}
	b, err := mcpConfigJSON(w.client.Base(), run.AgentToken, w.cfg.MCPConfig, false)
	if err != nil {
		return forget, err
	}
	dir := filepath.Join(w.cfg.StateDir, "runs", run.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return forget, err
	}
	// MkdirAll leaves an existing directory's mode alone (an earlier
	// attempt's log made it): the file below holds a credential.
	if err := os.Chmod(dir, 0o700); err != nil {
		return forget, err
	}
	path := filepath.Join(dir, "mcp.json")
	if err := writeFile0600(path, b); err != nil {
		return forget, err
	}
	w.runMCP = path
	return func() {
		forget()
		_ = os.Remove(path)
	}, nil
}

// mcpFor is the MCP config a run's agent gets its skopos tools from. A run
// that came with a token uses its own config and nothing else — "" when
// that could not be written. Only a run without a token (a server from
// before run tokens) uses the agent-key config, when the worker has one.
// "" = no skopos tools; the operator's mcp_config still applies.
func (w *Worker) mcpFor(run *Run) string {
	if w.tokenRun != "" && w.tokenRun == run.ID {
		return w.runMCP
	}
	return w.agentMCP
}

// EnableAgentMCP gives every run of a claude or opencode agent that came
// without a run token the MCP config at path (WriteAgentMCPConfig) and the
// skopos tools of its stage.
// refresh, when set, rewrites the file before each run (a srv+ server URL:
// the service may have moved since).
func (w *Worker) EnableAgentMCP(path string, refresh func() error) {
	w.agentMCP, w.agentMCPRefresh = path, refresh
}

// skoposPrompt is what a stage prompt says about skopos: nil when the run
// has no skopos tools (no run token and no agent key, or a codex run). branch is the item
// branch; head is the commit a review stage reviews ("" for plan and
// implement); planID and steps let implement mark its steps.
//
// The code tools appear only when the workspace's code_index switch is on
// and the server holds a usable index, and the prompt names exactly which
// branch that is (indexState): the item branch when its index is at the
// reviewed commit, else the default branch — with what it does not show.
func (w *Worker) skoposPrompt(ctx context.Context, run *Run, account *Account, base, branch, phase, planID string, steps []Step, head string) map[string]any {
	if w.mcpFor(run) == "" || !w.cfg.runnerHasSkopos(account.Runner) {
		return nil
	}
	runner := account.Runner
	if runner == "" {
		runner = "claude"
	}
	out := map[string]any{
		"Workspace": run.WorkspaceID, "Base": base, "Branch": branch,
		"Author":    runner + "@" + run.ID,
		"Implement": phase == "implement", "PlanID": planID, "Steps": steps,
	}
	if ib, note, semantic := w.indexState(ctx, run.WorkspaceID, base, branch, phase, head); ib != "" {
		out["IndexBranch"], out["IndexNote"], out["Semantic"] = ib, note, semantic
	}
	return out
}

// indexState picks the branch whose index fits the stage, the sentence
// that says what it does and does not show, and whether its symbols are
// embedded (code_find answers). "" = no usable index (the switch is off,
// the server has none yet, or it could not be asked).
func (w *Worker) indexState(ctx context.Context, ws, base, branch, phase, head string) (string, string, bool) {
	if !w.indexEnabled(ws) {
		return "", "", false
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	indexed, err := w.client.IndexedBranches(qctx, ws)
	if err != nil {
		return "", "", false
	}
	if b, ok := indexed[branch]; head != "" && branch != "" && ok && b.Head == head {
		return branch, "It is indexed at exactly the commit you are reviewing.", b.Semantic
	}
	baseIndex, ok := indexed[base]
	if !ok {
		return "", "", false
	}
	switch phase {
	case "plan":
		return base, "It may lag the newest commits on " + base + ".", baseIndex.Semantic
	case "implement":
		return base, "Your own changes are not in it: for code you changed or added, read the files.", baseIndex.Semantic
	default:
		return base, "The branch under review is not indexed at this commit, so the index shows " + base + ": use the diff for what changed.", baseIndex.Semantic
	}
}
