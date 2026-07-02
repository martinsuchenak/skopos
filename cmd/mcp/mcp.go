package mcp

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	mcplib "github.com/paularlott/mcp"
)

var toolRegistrations []func(*mcplib.Server, *status.Service)
var blackboardToolRegistrations []func(*mcplib.Server, *blackboard.Service)
var plansToolRegistrations []func(*mcplib.Server, *plans.Service)
var contextToolRegistrations []func(*mcplib.Server, *status.Service, *blackboard.Service, *plans.Service)

func RegisterTool(fn func(*mcplib.Server, *status.Service)) {
	toolRegistrations = append(toolRegistrations, fn)
}

func RegisterBlackboardTool(fn func(*mcplib.Server, *blackboard.Service)) {
	blackboardToolRegistrations = append(blackboardToolRegistrations, fn)
}

func RegisterPlansTool(fn func(*mcplib.Server, *plans.Service)) {
	plansToolRegistrations = append(plansToolRegistrations, fn)
}

func RegisterContextTool(fn func(*mcplib.Server, *status.Service, *blackboard.Service, *plans.Service)) {
	contextToolRegistrations = append(contextToolRegistrations, fn)
}

// instructions is returned to MCP clients on initialize. Clients surface it as
// system context, so it front-loads orientation and tool-selection guidance
// without the user having to prompt for it.
const instructions = `You are connected to **skopos**, a shared memory and coordination service for AI agents across sessions and git branches. Three capabilities:

1. Blackboard — durable knowledge entries (your memory). Scopes: project (all agents/branches), branch, session. Types: finding, decision, bug, debt, warning, context. "bug" and "debt" float: always returned regardless of branch. Use it as a notebook — read what others learned, record what you learn.
2. Plans & items — shared to-do lists with dependencies. Item statuses: pending, in_progress, done, blocked. Adding a dependency auto-blocks the dependent item; finishing a dependency auto-unblocks; finishing every item auto-completes the plan.
3. Status — agent status reports powering the dashboard.

At the start of every task, call ` + "`skopos_context`" + ` once (pass ` + "`workspace_id`" + ` and ` + "`branch`" + `) to load the relevant blackboard, active plans/blocked items, and in-flight sessions. Then:
- recall prior notes -> ` + "`blackboard_read`" + ` (pass ` + "`workspace_id`" + ` and ` + "`branch`" + `).
- record something worth keeping -> ` + "`blackboard_write`" + ` (scope ` + "`branch`" + ` by default, ` + "`project`" + ` for repo-wide; type ` + "`bug`" + `/` + "`debt`" + ` for issues that must be seen across branches). Always set ` + "`author_agent_id`" + `. Remove an obsolete entry with ` + "`blackboard_delete`" + `.
- multi-step work -> ` + "`plan_create`" + ` + ` + "`plan_add_item`" + `; mark items ` + "`done`" + ` as you finish. Sequence work with ` + "`plan_add_item_dependency`" + ` / ` + "`plan_add_plan_dependency`" + `. If an item was blocked, check if it is ready with ` + "`plan_read`" + ` (pass ` + "`item_id`" + ` for a single-item check). When a plan is done or abandoned, archive it with ` + "`plan_archive`" + `.
- checkpoint progress -> ` + "`report_status`" + ` (status, progress, message). Never report ` + "`stuck`" + ` or ` + "`orphaned`" + ` — those are server-set.

Keep entries concise, prefer the narrowest scope, and pass a stable ` + "`author_agent_id`" + ` (e.g. "<tool>-<hostname>").`

// NewMCPHandler builds the MCP server with all registered tools and returns
// the http.Handler that serves the MCP protocol. The caller mounts it at /mcp
// (see cmd.serve). Authentication and lifecycle are the caller's responsibility.
func NewMCPHandler(statusService *status.Service, blackboardService *blackboard.Service, plansService *plans.Service) http.Handler {
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
	for _, fn := range contextToolRegistrations {
		fn(server, statusService, blackboardService, plansService)
	}

	return http.HandlerFunc(server.HandleRequest)
}
