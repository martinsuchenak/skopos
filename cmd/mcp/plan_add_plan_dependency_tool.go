package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanAddPlanDependencyTool)
}

func registerPlanAddPlanDependencyTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_add_plan_dependency", "Make a plan depend on another plan; the plan is auto-blocked until its dependency is completed",
			mcplib.String("plan_id", "Plan ID that will be blocked", mcplib.Required()),
			mcplib.String("depends_on_plan_id", "Plan ID that must be completed first", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			err := service.AddPlanDependency(ctx,
				req.StringOr("plan_id", ""),
				req.StringOr("depends_on_plan_id", ""),
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"status": "plan_dependency_added"}), nil
		},
	)
}
