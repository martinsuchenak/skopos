package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanCreateTool)
}

func registerPlanCreateTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_create", "Create a named plan for tracking work items",
			mcplib.String("workspace_id", "Workspace ID to scope the plan to"),
			mcplib.String("name", "Plan name", mcplib.Required()),
			mcplib.String("author_agent_id", "Identifier of the creating agent", mcplib.Required()),
			mcplib.String("branch_name", "Branch this plan belongs to (omit for project-wide)"),
			mcplib.String("description", "Optional description of the plan"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			plan, err := service.CreatePlan(ctx, plans.CreatePlanInput{
				WorkspaceID:   req.StringOr("workspace_id", ""),
				Name:          req.StringOr("name", ""),
				AuthorAgentID: req.StringOr("author_agent_id", ""),
				BranchName:    req.StringOr("branch_name", ""),
				Description:   req.StringOr("description", ""),
			})
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"id": plan.ID, "name": plan.Name}), nil
		},
	)
}
