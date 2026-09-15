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
	registerTool(server, 
		mcplib.NewTool(
			"skopos_workspaces",
			"Rarely needed: workspace_id is OPTIONAL on all skopos tools when your key has one workspace (the common case — just omit it). Only call this when you must choose between multiple workspaces and don't know the ids.",
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
