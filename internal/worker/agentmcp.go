package worker

import (
	"bytes"
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
	"github.com/martinsuchenak/skopos/internal/wsconfig"
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

// mcpConfigJSON builds the MCP config a run gets: its other servers plus,
// when bearer is set, the skopos server with bearer as its credential. The
// other servers are delivered (the ones the server sent with the claim)
// when the server holds the run's MCP servers (owned), else the operator's
// mcp_config file. strict refuses an operator file that defines a server
// named skopos (the agent key path, where the worker has always provided
// it); otherwise the worker's entry replaces it — a run's own token is the
// narrower credential.
func mcpConfigJSON(base, bearer, operatorConfig string, delivered map[string]json.RawMessage, owned, strict bool) ([]byte, error) {
	servers := map[string]any{}
	switch {
	case owned:
		for name, s := range delivered {
			servers[name] = s
		}
	case operatorConfig != "":
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
	delete(servers, agentMCPServer)
	if bearer != "" {
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
	}
	return json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
}

// deliveredMCP checks the MCP servers a claim answer carries for use on
// this host and fills in the host's variables. A server this worker will
// not run is left out, with a note: a command server on a worker that does
// not take commands from the server, one whose command or env names a
// variable the worker does not declare, one under the worker's own name.
func (c *Config) deliveredMCP(in *RunMCP) (servers map[string]json.RawMessage, notes []string) {
	servers = map[string]json.RawMessage{}
	if in == nil {
		return servers, nil
	}
	names := make([]string, 0, len(in.Servers))
	for name := range in.Servers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		var entry struct {
			Type    string            `json:"type,omitempty"`
			URL     string            `json:"url,omitempty"`
			Headers map[string]string `json:"headers,omitempty"`
			Command string            `json:"command,omitempty"`
			Args    []string          `json:"args,omitempty"`
			Env     map[string]string `json:"env,omitempty"`
		}
		dec := json.NewDecoder(bytes.NewReader(in.Servers[name]))
		dec.DisallowUnknownFields()
		switch {
		case name == agentMCPServer || !plainRunID(name):
			notes = append(notes, fmt.Sprintf("mcp server %q: not a name this worker takes from the server: left out", name))
			continue
		case dec.Decode(&entry) != nil:
			// The decoder's message could quote a credential: not repeated.
			notes = append(notes, fmt.Sprintf("mcp server %s: its definition is not one this worker reads: left out", name))
			continue
		case (entry.URL == "") == (entry.Command == ""):
			notes = append(notes, fmt.Sprintf("mcp server %s: neither an http server nor a command: left out", name))
			continue
		}
		if entry.Command != "" {
			if !c.ServerCommands {
				notes = append(notes, fmt.Sprintf("mcp server %s runs a command on this host, and worker.server_commands is off: left out", name))
				continue
			}
			var err error
			fill := func(v string) string {
				out, e := wsconfig.Expand(v, c.Vars)
				if e != nil && err == nil {
					err = e
				}
				return out
			}
			entry.Command = fill(entry.Command)
			for i := range entry.Args {
				entry.Args[i] = fill(entry.Args[i])
			}
			for k, v := range entry.Env {
				entry.Env[k] = fill(v)
			}
			if err != nil {
				notes = append(notes, fmt.Sprintf("mcp server %s: %v: left out", name, err))
				continue
			}
		}
		buf, _ := json.Marshal(entry)
		servers[name] = buf
	}
	return servers, notes
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
	b, err := mcpConfigJSON(base, key, operatorConfig, nil, false, true)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, "agent-mcp.json")
	return path, writeFile0600(path, b)
}

// prepareRunMCP gives a run its own MCP config:
// <state_dir>/runs/<run>/mcp.json (the directory 0700, the file 0600). It
// holds skopos with the run's token, when the claim came with one, and the
// run's other servers: the ones the claim answer carries when the server
// holds the workspace's MCP servers (run.MCP — then the operator's
// mcp_config is not used for the run at all), else the operator's. The
// token and the servers' credentials join the redacted values for the run.
// The returned func removes the file and forgets them; it is never nil.
//
// A run that came with a token never uses the shared agent key: when its
// own config cannot be written, its agents get no skopos tools (mcpFor)
// and, where the server holds its MCP servers, no MCP server at all.
// Without a token or servers (a server from before run tokens) nothing is
// written, and the run falls back to the agent-key config, if the worker
// has one.
func (w *Worker) prepareRunMCP(run *Run) (func(), error) {
	if run.AgentToken == "" && run.MCP == nil {
		return func() {}, nil
	}
	w.tokenRun, w.runMCP, w.runSecrets = run.ID, "", nil
	w.runOwned, w.runSkopos, w.runMCPNotes, w.runMCPNames = run.MCP != nil, run.AgentToken != "", nil, nil
	if run.AgentToken != "" {
		w.runSecrets = append(w.runSecrets, run.AgentToken)
	}
	if run.MCP != nil {
		w.runSecrets = append(w.runSecrets, run.MCP.Secrets...)
	}
	forget := func() {
		w.tokenRun, w.runMCP, w.runSecrets = "", "", nil
		w.runOwned, w.runSkopos, w.runMCPNotes, w.runMCPNames = false, false, nil, nil
	}
	if run.AgentToken != "" && !w.runTokensSeen {
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
	delivered, notes := w.cfg.deliveredMCP(run.MCP)
	if run.MCP != nil {
		w.runMCPNotes = append(slices.Clone(run.MCP.Notes), notes...)
		names := make([]string, 0, len(delivered))
		for name := range delivered {
			names = append(names, name)
		}
		slices.Sort(names)
		w.runMCPNames = names
	}
	// Until the file is written the run has none of them.
	gave := w.runMCPNames
	w.runMCPNames = nil
	b, err := mcpConfigJSON(w.client.Base(), run.AgentToken, w.cfg.MCPConfig, delivered, run.MCP != nil, false)
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
	w.runMCP, w.runMCPNames = path, gave
	return func() {
		forget()
		_ = os.Remove(path)
	}, nil
}

// sweepRunMCP removes the MCP files earlier runs left in the state
// directory: a run's file is removed when it ends, but not when the worker
// was killed in the middle of one — and it holds that workspace's
// credentials. Called at start, before any run.
func (w *Worker) sweepRunMCP() {
	if w.cfg.StateDir == "" {
		return
	}
	left, _ := filepath.Glob(filepath.Join(w.cfg.StateDir, "runs", "*", "mcp.json*"))
	for _, path := range left {
		if err := os.Remove(path); err == nil {
			w.log.Info("removed a run's MCP config a stopped worker left behind", "file", path)
		}
	}
}

// mcpFor is the MCP config a run's agent gets its skopos tools from. A run
// that came with a token uses its own config and nothing else — "" when
// that could not be written. Only a run without a token (a server from
// before run tokens) uses the agent-key config, when the worker has one.
// "" = no skopos tools.
func (w *Worker) mcpFor(run *Run) string {
	if w.tokenRun != "" && w.tokenRun == run.ID {
		if !w.runSkopos {
			return ""
		}
		return w.runMCP
	}
	return w.agentMCP
}

// mcpConfigFor is the MCP config file an agent of the run gets on a
// runner, and whether it carries the skopos tools.
//
//   - The server holds the run's MCP servers: the run's own file, for a
//     runner that takes one (claude, opencode), or nothing. Never the
//     operator's mcp_config — also when the run's file could not be
//     written.
//   - Otherwise: the run's own file (skopos plus the operator's servers),
//     else the agent-key file, else the operator's mcp_config.
func (w *Worker) mcpConfigFor(run *Run, runner string) (path string, skopos bool) {
	own := w.tokenRun != "" && w.tokenRun == run.ID
	takes := w.cfg.runnerHasSkopos(runner)
	if own && w.runOwned {
		if w.runMCP == "" || !takes {
			return "", false
		}
		return w.runMCP, w.runSkopos
	}
	if f := w.mcpFor(run); f != "" && takes {
		return f, true
	}
	return w.cfg.MCPConfig, false
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
