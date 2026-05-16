package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanReadTool)
}

func registerPlanReadTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_read", "Get a plan with all its items",
			mcplib.String("id", "Plan ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			plan, err := service.GetPlan(ctx, req.StringOr("id", ""))
			if err != nil {
				return nil, mcplib.NewToolErrorInternal(err.Error())
			}
			return mcplib.NewToolResponseJSON(plan), nil
		},
	)
}
