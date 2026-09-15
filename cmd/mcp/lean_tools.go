package mcp

import (
	mcplib "github.com/paularlott/mcp"
)

// leanToolset moves every non-core tool behind the MCP tool_search /
// execute_tool meta-tools: hidden from tools/list (so their schemas stop
// riding in every step's re-sent context) but discoverable and callable on
// demand. Measured motivation (224-run index-effectiveness series): schema
// bytes are step-level weight — 28 tools ≈ 20 KB re-sent on every one of
// ~11 turns; lean keeps ~10 native schemas and halves the bill.
var leanToolset bool

// SetLeanToolset enables lean mode (deployed via [mcp] lean_tools = true).
func SetLeanToolset(on bool) { leanToolset = on }

// LeanToolset reports whether lean mode is active (wired in serve).
func LeanToolset() bool { return leanToolset }

// coreTools stay natively listed even in lean mode: the daily-driver and
// benchmark-measured workhorses. Everything else (outline, callees,
// call_tree, dependencies, cycles, dead, branch_diff, index_status,
// blackboard_delete, skopos_workspaces, and the full plan suite) goes
// discoverable — one tool_search away.
var coreTools = map[string]bool{
	"code_search":      true,
	"code_find":        true,
	"code_symbol":      true,
	"code_callers":     true,
	"code_impact":      true,
	"skopos_context":   true,
	"blackboard_read":  true,
	"blackboard_write": true,
	"report_status":    true,
}

// registerTool routes every tool registration through the lean filter.
// Keywords for discovery are the tool's own name plus its description
// words, so tool_search finds "call tree" as readily as "code_call_tree".
func registerTool(server *mcplib.Server, tool *mcplib.ToolBuilder, handler mcplib.ToolHandler) {
	if leanToolset && !coreTools[tool.Name()] {
		tool = tool.Discoverable(tool.Name())
	}
	server.RegisterTool(tool, handler)
}
