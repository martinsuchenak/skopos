package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/inbox"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterInboxTool(registerInboxTools)
}

// registerInboxTools exposes the workspace inbox (docs/design/inbox.md):
// captured, unprocessed work that an agent claims, enriches, and converts
// into a plan. The tools are not in coreTools: in lean mode they stay behind
// tool_search — the inbox is an occasional workflow, not a daily driver.
func registerInboxTools(server *mcplib.Server, service *inbox.Service) {
	registerTool(server,
		mcplib.NewTool("inbox_create",
			"Capture a new inbox item: unprocessed work, a rough idea, or a task not yet ready to action. Content is markdown — rough notes, pointers to code/files are fine. workspace_id may be omitted ONLY with the root key (UNFILED capture, visible to root until filed); scoped keys must pass it.",
			mcplib.String("workspace_id", "Workspace ID to scope the item to (root may omit it to capture an UNFILED item — invisible to scoped keys until filed)"),
			mcplib.String("title", "Short item title", mcplib.Required()),
			mcplib.String("content", "Markdown body: rough description, pointers to code/files, links"),
			mcplib.StringArray("tags", "Optional tags (lowercase alphanumerics with . _ / -)"),
			mcplib.Integer("priority", "Optional explicit priority (>= 1; smaller first). Unprioritized items always sort after prioritized ones — set it only on the few items that matter."),
			mcplib.String("author_agent_id", "Identifier of the capturing agent", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			item, err := service.CreateItem(ctx, inbox.CreateInput{
				WorkspaceID:   req.StringOr("workspace_id", ""),
				Title:         req.StringOr("title", ""),
				Content:       req.StringOr("content", ""),
				Tags:          req.StringSliceOr("tags", nil),
				Priority:      req.IntOr("priority", 0),
				AuthorAgentID: req.StringOr("author_agent_id", ""),
			})
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"id": item.ID, "status": string(item.Status), "title": item.Title, "priority": item.Priority, "workspace_id": item.WorkspaceID}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_list",
			"List inbox items. Defaults to open (unprocessed) items; pass status=all for every item. With the root key, omit workspace_id to list every workspace's items including unfiled captures.",
			wsParam(),
			mcplib.String("status", "Filter: open (default), in_progress, converted, done, discarded, or all"),
			mcplib.String("tag", "Filter by tag"),
			mcplib.String("q", "Substring search over title and content"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			// resolveWorkspace demands an explicit workspace from root too;
			// for inbox_list root may omit it to see everything (unfiled
			// captures have no workspace to name).
			workspace := req.StringOr("workspace_id", "")
			if workspace == "" {
				p := auth.PrincipalFromContext(ctx)
				if p == nil || p.Root || p.AllWorkspaces {
					// Root/all-scoped: "" lists across workspaces (unfiled
					// items are included as workspace-less rows).
				} else if list := p.WorkspaceList(); len(list) == 1 {
					workspace = list[0]
				} else {
					return nil, toolError(fmt.Errorf("%w: workspace_id is required (your key spans %d workspaces; pass one explicitly)", blackboard.ErrInvalidInput, len(list)))
				}
			}
			status := req.StringOr("status", string(inbox.StatusOpen))
			if status == "all" {
				status = ""
			}
			items, err := service.ListItems(ctx, workspace, status, req.StringOr("tag", ""), req.StringOr("q", ""))
			if err != nil {
				return nil, toolError(err)
			}
			rows := make([]map[string]any, 0, len(items))
			for _, it := range items {
				rows = append(rows, map[string]any{
					"id":         it.ID,
					"title":      flattenText(it.Title),
					"status":     string(it.Status),
					"tags":       it.Tags,
					"priority":   it.Priority,
					"plan_id":    it.PlanID,
					"excerpt":    flattenText(it.Excerpt),
					"claimed_by": it.ClaimedByAgentID,
					"age":        coarseAge(it.CreatedAt),
				})
			}
			return mcplib.NewToolResponseJSON(map[string]any{"items": rows}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_read",
			"Read one inbox item in full: raw markdown content, tags, claim, and the linked plan when converted.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			item, err := service.GetItem(ctx, req.StringOr("item_id", ""))
			if err != nil {
				return nil, toolError(err)
			}
			out := map[string]any{
				"id":         item.ID,
				"title":      flattenText(item.Title),
				"status":     string(item.Status),
				"content":    item.Content,
				"tags":       item.Tags,
				"claimed_by": item.ClaimedByAgentID,
				"author":     item.AuthorAgentID,
				"created_at": item.CreatedAt,
			}
			if item.PlanID != "" {
				out["plan_id"] = item.PlanID
				if item.Plan != nil {
					out["plan"] = map[string]any{"id": item.Plan.ID, "name": flattenText(item.Plan.Name), "status": item.Plan.Status}
				}
			}
			return mcplib.NewToolResponseJSON(out), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_update",
			"Enrich an inbox item you are processing (title/content/tags). Convention: PRESERVE the original content and append an \"## Enrichment — <agent>, <date>\" section with your findings, code pointers, and the proposed plan outline — the raw capture must survive. Only open and in_progress items are editable.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
			mcplib.String("title", "New title (omit to keep)"),
			mcplib.String("content", "New markdown content (omit to keep; when set, include the original plus your Enrichment section)"),
			mcplib.StringArray("tags", "Replace tags (omit to keep)"),
			mcplib.Integer("priority", "Priority: >= 1 sets, -1 clears (back to unordered), 0 or omitted leaves unchanged. Prioritized items sort first, smallest first."),
			mcplib.String("workspace_id", "File (or re-file) an unfiled/misfiled item into this workspace; open/in_progress items only. Omit to keep the current workspace."),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := inbox.UpdateInput{
				Title:       req.StringOr("title", ""),
				Content:     req.StringOr("content", ""),
				WorkspaceID: req.StringOr("workspace_id", ""),
			}
			// An absent or empty tags array leaves the tags unchanged (the
			// ToolRequest API cannot distinguish nil from an empty slice, so
			// clearing tags is a REST/CLI-only operation).
			if tags := req.StringSliceOr("tags", nil); len(tags) > 0 {
				input.Tags = &tags
			}
			// Same presence constraint: -1 clears, 0 (also the absent
			// default) leaves the priority unchanged.
			if p := req.IntOr("priority", 0); p != 0 {
				if p == -1 {
					p = 0 // clear
				}
				input.Priority = &p
			}
			if err := service.UpdateItem(ctx, req.StringOr("item_id", ""), input); err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"updated": true}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_claim",
			"Claim an open item to start processing it (moves to in_progress; another agent's claim returns a conflict). Pass an empty agent_id to release back to open.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
			mcplib.String("agent_id", "Claiming agent ID; empty string releases the item"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			item, err := service.Claim(ctx, req.StringOr("item_id", ""), req.StringOr("agent_id", ""))
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"id": item.ID, "status": string(item.Status), "claimed_by": item.ClaimedByAgentID}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_convert",
			"Mark an item converted by linking the plan built from it. Author the plan FIRST with plan_create/plan_add_item (same workspace), then call this with the plan's ID. Completing the plan automatically completes the item.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
			mcplib.String("plan_id", "ID of the plan created from this item", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			item, err := service.Convert(ctx, req.StringOr("item_id", ""), inbox.ConvertInput{PlanID: req.StringOr("plan_id", "")})
			if err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"id": item.ID, "status": string(item.Status), "plan_id": item.PlanID}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_complete",
			"Manually mark an item done — for work that finished without a plan, or ahead of it. Allowed from open, in_progress, and converted; discarded items must be restored first. Done stays terminal.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			if err := service.Complete(ctx, req.StringOr("item_id", "")); err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"completed": true}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_reopen",
			"Bring a done item back to open (undo a wrong manual complete). Clears the claim, plan link, and priority — a fresh cycle; the item can be claimed and converted again.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			if err := service.Reopen(ctx, req.StringOr("item_id", "")); err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"reopened": true}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_restore",
			"Restore a discarded item back to open (undo an accidental discard; clears any stale claim). Done items are terminal — they belong to their plan.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			if err := service.Restore(ctx, req.StringOr("item_id", "")); err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"restored": true}), nil
		},
	)

	registerTool(server,
		mcplib.NewTool("inbox_discard",
			"Discard an item (won't do / superseded). Terminal.",
			mcplib.String("item_id", "Item ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			if err := service.Discard(ctx, req.StringOr("item_id", "")); err != nil {
				return nil, toolError(err)
			}
			return mcplib.NewToolResponseJSON(map[string]any{"discarded": true}), nil
		},
	)
}

// coarseAge renders a compact age for list rows.
func coarseAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
