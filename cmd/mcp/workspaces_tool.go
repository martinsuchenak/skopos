package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/workspaces"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterWorkspacesTool(registerSkoposWorkspacesTool)
}

func registerSkoposWorkspacesTool(server *mcplib.Server, registry *workspaces.Service) {
	server.RegisterTool(
		mcplib.NewTool(
			"skopos_workspaces",
			"List the workspaces this credential can access (with the git_url when registered). Use it to discover valid workspace_id values for the other tools.",
		),
		func(ctx context.Context, _ *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			list, err := registry.List(ctx)
			if err != nil {
				return nil, toolError(err)
			}
			out := make([]map[string]any, 0, len(list))
			for _, ws := range list {
				entry := map[string]any{"id": ws.ID}
				if ws.Name != "" {
					entry["name"] = ws.Name
				}
				if ws.GitURL != "" {
					entry["git_url"] = ws.GitURL
				}
				out = append(out, entry)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"workspaces": out, "count": len(out)}), nil
		},
	)
}
