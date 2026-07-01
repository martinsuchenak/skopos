package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterBlackboardTool(registerBlackboardWriteTool)
}

func registerBlackboardWriteTool(server *mcplib.Server, service *blackboard.Service) {
	server.RegisterTool(
		mcplib.NewTool("blackboard_write", "Write an entry to the Skopos blackboard memory",
			mcplib.String("scope", "Required. Entry scope: project (visible to all agents), branch (shared on a git branch), or session (this session only)", mcplib.Required()),
			mcplib.String("entry_type", "Required. Entry type: finding, decision, bug, debt, warning, or context. Bug and debt are floating (always visible regardless of branch filter).", mcplib.Required()),
			mcplib.String("title", "Required. Short descriptive title", mcplib.Required()),
			mcplib.String("author_agent_id", "Required. Stable agent identifier, e.g. codex-macbook", mcplib.Required()),
			mcplib.String("workspace_id", "Required for project and branch scope. The workspace this entry belongs to."),
			mcplib.String("branch_name", "Required when scope=branch. The git branch name."),
			mcplib.String("session_id", "Required when scope=session. Must be a valid session ID (call report_status first to create a session)."),
			mcplib.String("content", "Optional. Detailed content/body of the entry"),
			mcplib.String("code_ref", "Optional. Code reference, e.g. auth/jwt.go:45"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := blackboard.WriteInput{
				WorkspaceID:   req.StringOr("workspace_id", ""),
				Scope:         blackboard.Scope(req.StringOr("scope", "")),
				EntryType:     blackboard.EntryType(req.StringOr("entry_type", "")),
				Title:         req.StringOr("title", ""),
				AuthorAgentID: req.StringOr("author_agent_id", ""),
				BranchName:    req.StringOr("branch_name", ""),
				SessionID:     req.StringOr("session_id", ""),
				Content:       req.StringOr("content", ""),
				CodeRef:       req.StringOr("code_ref", ""),
			}
			result, err := service.Write(ctx, input)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(result), nil
		},
	)
}
