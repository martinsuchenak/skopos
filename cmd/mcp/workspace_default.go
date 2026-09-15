package mcp

import (
	"context"
	"fmt"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	mcplib "github.com/paularlott/mcp"
)

// wsParamDecl is the workspace_id parameter declaration shared by tools
// that accept it. It is optional: a key scoped to exactly one workspace
// defaults to it, so single-project agents never burn a discovery turn on
// skopos_workspaces before their first query.
const wsParamDoc = "Workspace ID. Optional when your key is scoped to exactly one workspace (then it defaults to that workspace); required otherwise. Derive it with `skopos workspace` or the git remote (e.g. github.com/owner/repo)."

func wsParam() mcplib.Parameter {
	return mcplib.String("workspace_id", wsParamDoc)
}

// resolveWorkspace returns the request's workspace_id, defaulting to the
// principal's sole workspace when the parameter is omitted. Fails with an
// actionable error when omitted and ambiguous (zero or multiple scopes).
func resolveWorkspace(ctx context.Context, req *mcplib.ToolRequest) (string, error) {
	if ws := req.StringOr("workspace_id", ""); ws != "" {
		return ws, nil
	}
	p := auth.PrincipalFromContext(ctx)
	if p == nil || p.Root || p.AllWorkspaces {
		return "", fmt.Errorf("%w: workspace_id is required (your credential covers every workspace; derive it with `skopos workspace` or the git remote, e.g. github.com/owner/repo)", blackboard.ErrInvalidInput)
	}
	if list := p.WorkspaceList(); len(list) == 1 {
		return list[0], nil
	}
	return "", fmt.Errorf("%w: workspace_id is required (your key spans %d workspaces; pass one explicitly)", blackboard.ErrInvalidInput, len(p.WorkspaceList()))
}
