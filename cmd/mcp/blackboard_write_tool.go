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
			mcplib.String("scope", "Memory scope: session, branch, or project", mcplib.Required()),
			mcplib.String("entry_type", "Entry type: finding, decision, bug, debt, warning, or context", mcplib.Required()),
			mcplib.String("title", "Short descriptive title", mcplib.Required()),
			mcplib.String("author_agent_id", "Identifier of the writing agent", mcplib.Required()),
			mcplib.String("branch_name", "Branch name (required when scope=branch)"),
			mcplib.String("session_id", "Session ID (required when scope=session)"),
			mcplib.String("content", "Detailed content"),
			mcplib.String("code_ref", "Optional code reference, e.g. auth/jwt.go:45"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := blackboard.WriteInput{
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
