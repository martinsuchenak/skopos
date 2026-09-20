package mcp

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	mcplib "github.com/paularlott/mcp"
)

var toolRegistrations []func(*mcplib.Server, *status.Service)
var blackboardToolRegistrations []func(*mcplib.Server, *blackboard.Service)
var plansToolRegistrations []func(*mcplib.Server, *plans.Service)
var inboxToolRegistrations []func(*mcplib.Server, *inbox.Service)
var contextToolRegistrations []func(*mcplib.Server, *status.Service, *blackboard.Service, *plans.Service, *inbox.Service)
var codeIndexToolRegistrations []func(*mcplib.Server, *codeindex.Service)
var workspacesToolRegistrations []func(*mcplib.Server, *workspaces.Service)

func RegisterTool(fn func(*mcplib.Server, *status.Service)) {
	toolRegistrations = append(toolRegistrations, fn)
}

func RegisterBlackboardTool(fn func(*mcplib.Server, *blackboard.Service)) {
	blackboardToolRegistrations = append(blackboardToolRegistrations, fn)
}

func RegisterPlansTool(fn func(*mcplib.Server, *plans.Service)) {
	plansToolRegistrations = append(plansToolRegistrations, fn)
}

func RegisterInboxTool(fn func(*mcplib.Server, *inbox.Service)) {
	inboxToolRegistrations = append(inboxToolRegistrations, fn)
}

func RegisterContextTool(fn func(*mcplib.Server, *status.Service, *blackboard.Service, *plans.Service, *inbox.Service)) {
	contextToolRegistrations = append(contextToolRegistrations, fn)
}

// RegisterCodeIndexTool registers a code-index MCP tool. The service may be
// nil (feature disabled); registrations skip themselves in that case.
func RegisterCodeIndexTool(fn func(*mcplib.Server, *codeindex.Service)) {
	codeIndexToolRegistrations = append(codeIndexToolRegistrations, fn)
}

// RegisterWorkspacesTool registers a workspace-registry MCP tool.
func RegisterWorkspacesTool(fn func(*mcplib.Server, *workspaces.Service)) {
	workspacesToolRegistrations = append(workspacesToolRegistrations, fn)
}

// instructions is returned to MCP clients on initialize. Clients surface it as
// system context, so it front-loads orientation and tool-selection guidance
// without the user having to prompt for it.
const instructions = `You are connected to **skopos**, a shared memory and coordination service for AI agents across sessions and git branches. Three capabilities:

1. Blackboard — durable knowledge entries (your memory). Scopes: project (all agents/branches), branch, session. Types: finding, decision, bug, debt, warning, context. "bug" and "debt" float: always returned regardless of branch. Use it as a notebook — read what others learned, record what you learn.
2. Plans & items — shared to-do lists with dependencies. Item statuses: pending, in_progress, done, blocked. Adding a dependency auto-blocks the dependent item; finishing a dependency auto-unblocks; finishing every item auto-completes the plan.
3. Status — agent status reports powering the dashboard.
4. Code index — symbols, call graph, and dependencies of indexed repos. ` + "`code_search`" + ` (add ` + "`semantic: true`" + ` for meaning-based search, ` + "`path`" + ` to scope to a subtree), ` + "`code_symbol`" + ` for exact definitions with their docs, ` + "`code_callers`" + `/` + "`code_callees`" + ` / ` + "`code_impact`" + ` for the call graph (call sites include instantiations, subclasses, and type references), ` + "`code_dependencies`" + ` for module imports. Search visibility/attributes as text ("private cache", route names).
5. Inbox — the user's captured, unprocessed work (rough ideas in markdown). When the user asks you to work an inbox item: ` + "`inbox_list`" + `/` + "`inbox_read`" + ` it, ` + "`inbox_claim`" + ` it, enrich the content with ` + "`inbox_update`" + ` (preserve the original, append an Enrichment section), build a plan with the plan tools, then ` + "`inbox_convert`" + ` with the new plan id.

At the start of every task, call ` + "`skopos_context`" + ` once (pass ` + "`workspace_id`" + ` and ` + "`branch`" + `) to load the relevant blackboard, active plans/blocked items, and in-flight sessions. Then:
- recall prior notes -> ` + "`blackboard_read`" + ` (pass ` + "`workspace_id`" + ` and ` + "`branch`" + `).
- record something worth keeping -> ` + "`blackboard_write`" + ` (scope ` + "`branch`" + ` by default, ` + "`project`" + ` for repo-wide; type ` + "`bug`" + `/` + "`debt`" + ` for issues that must be seen across branches). Always set ` + "`author_agent_id`" + `. Remove an obsolete entry with ` + "`blackboard_delete`" + `.
- multi-step work -> ` + "`plan_create`" + ` + ` + "`plan_add_item`" + `; mark items ` + "`done`" + ` as you finish. Sequence work with ` + "`plan_add_item_dependency`" + ` / ` + "`plan_add_plan_dependency`" + `. If an item was blocked, check if it is ready with ` + "`plan_read`" + ` (pass ` + "`item_id`" + ` for a single-item check). When a plan is done or abandoned, archive it with ` + "`plan_archive`" + `.
- checkpoint progress -> ` + "`report_status`" + ` (status, progress, message). Never report ` + "`stuck`" + ` or ` + "`orphaned`" + ` — those are server-set.

Keep entries concise, prefer the narrowest scope, and pass a stable ` + "`author_agent_id`" + ` (e.g. "<tool>-<hostname>").`

// semanticSearcher supplies vector search to code_search's semantic mode;
// nil (the default) degrades semantic queries to plain full-text search.
var semanticSearcher codeindex.Embedder

// SetSemanticSearcher enables code_search semantic=true over MCP.
func SetSemanticSearcher(e codeindex.Embedder) { semanticSearcher = e }

// NewMCPHandler builds the MCP server with all registered tools and returns
// the http.Handler that serves the MCP protocol. The caller mounts it at /mcp
// (see cmd.serve). Authentication and lifecycle are the caller's responsibility.
func NewMCPHandler(statusService *status.Service, blackboardService *blackboard.Service, plansService *plans.Service, inboxService *inbox.Service, codeIndexService *codeindex.Service, workspacesService *workspaces.Service) http.Handler {
	server := mcplib.NewServer("skopos-mcp", "1.0.0")
	server.SetInstructions(instructions)

	for _, fn := range toolRegistrations {
		fn(server, statusService)
	}
	for _, fn := range blackboardToolRegistrations {
		fn(server, blackboardService)
	}
	for _, fn := range plansToolRegistrations {
		fn(server, plansService)
	}
	for _, fn := range inboxToolRegistrations {
		fn(server, inboxService)
	}
	for _, fn := range contextToolRegistrations {
		fn(server, statusService, blackboardService, plansService, inboxService)
	}
	for _, fn := range codeIndexToolRegistrations {
		if codeIndexService != nil {
			fn(server, codeIndexService)
		}
	}
	for _, fn := range workspacesToolRegistrations {
		if workspacesService != nil {
			fn(server, workspacesService)
		}
	}

	return http.HandlerFunc(server.HandleRequest)
}
