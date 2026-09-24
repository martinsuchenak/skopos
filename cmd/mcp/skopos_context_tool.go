package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterContextTool(registerSkoposContextTool)
}

func registerSkoposContextTool(server *mcplib.Server, statusSvc *status.Service, bbSvc *blackboard.Service, plansSvc *plans.Service, inboxSvc *inbox.Service) {
	registerTool(server,
		mcplib.NewTool(
			"skopos_context",
			"Load structural context for the current task: the branch's blackboard (memory), active plans with blocked items (todos), open inbox items (unprocessed work), and in-flight sessions. Call once at the start of a task.",
			mcplib.String("branch", "Current git branch name (recommended)"),
			wsParam(),
			mcplib.String("session_id", "Session id to scope the blackboard to"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			workspace, err := resolveWorkspace(ctx, req)
			if err != nil {
				return nil, toolError(err)
			}
			snapshot := buildSnapshot(ctx, statusSvc, bbSvc, plansSvc, inboxSvc,
				req.StringOr("branch", ""),
				workspace,
				req.StringOr("session_id", ""),
			)
			return mcplib.NewToolResponseJSON(snapshot), nil
		},
	)
}

// buildSnapshot composes the front-loaded context from the domains. Each
// section is gathered independently so a failure in one doesn't blank the
// whole snapshot.
func buildSnapshot(
	ctx context.Context,
	statusSvc *status.Service,
	bbSvc *blackboard.Service,
	plansSvc *plans.Service,
	inboxSvc *inbox.Service,
	branch, workspace, sessionID string,
) map[string]any {
	snapshot := map[string]any{"branch": branch}

	// --- blackboard (memory) ---
	bundle, err := bbSvc.Bundle(ctx, workspace, branch, sessionID)
	if err != nil {
		snapshot["blackboard"] = map[string]any{"error": err.Error()}
	} else {
		byType := map[string]int{}
		for _, e := range bundle.Entries {
			byType[string(e.EntryType)]++
		}
		snapshot["blackboard"] = map[string]any{
			"total":    len(bundle.Entries),
			"by_type":  byType,
			"markdown": bundle.MarkdownBundle,
		}
	}

	// --- plans / todos (active or blocked, with item progress) ---
	plansList, err := plansSvc.ListPlans(ctx, workspace, branch)
	if err != nil {
		snapshot["plans"] = map[string]any{"error": err.Error()}
	} else {
		out := make([]map[string]any, 0, len(plansList))
		for _, p := range plansList {
			if p.Status != plans.PlanActive && p.Status != plans.PlanBlocked {
				continue
			}
			entry := map[string]any{
				"id":            p.ID,
				"name":          p.Name,
				"status":        string(p.Status),
				"done":          0,
				"in_progress":   0,
				"pending":       0,
				"blocked":       0,
				"blocked_items": []string{},
			}
			if detail, derr := plansSvc.GetPlan(ctx, p.ID); derr == nil {
				blocked := []string{}
				for _, it := range detail.Items {
					switch it.Status {
					case plans.ItemDone:
						entry["done"] = entry["done"].(int) + 1
					case plans.ItemInProgress:
						entry["in_progress"] = entry["in_progress"].(int) + 1
					case plans.ItemBlocked:
						entry["blocked"] = entry["blocked"].(int) + 1
						blocked = append(blocked, fmt.Sprintf("%s (#%d)", flattenText(it.Title), it.Position))
					default:
						entry["pending"] = entry["pending"].(int) + 1
					}
				}
				entry["blocked_items"] = blocked
				// Find the first unclaimed pending item (ready to work on).
				nextReady := ""
				for _, it := range detail.Items {
					if it.Status == plans.ItemPending && it.ClaimedByAgentID == "" {
						nextReady = fmt.Sprintf("%s (#%d)", flattenText(it.Title), it.Position)
						break
					}
				}
				entry["next_ready"] = nextReady
			}
			out = append(out, entry)
			if len(out) >= 10 {
				break
			}
		}
		snapshot["plans"] = out
	}

	// --- inbox (the user's captured, unprocessed work) ---
	if inboxSvc != nil {
		items, err := inboxSvc.ListItems(ctx, workspace, string(inbox.StatusOpen), "", "")
		if err != nil {
			snapshot["inbox"] = map[string]any{"error": err.Error()}
		} else {
			out := make([]map[string]any, 0, len(items))
			for i, it := range items {
				if i >= 5 {
					break
				}
				out = append(out, map[string]any{
					"id":       it.ID,
					"title":    flattenText(it.Title),
					"tags":     it.Tags,
					"priority": it.Priority,
					"age":      coarseAge(it.CreatedAt),
				})
			}
			snapshot["inbox"] = map[string]any{"open": len(items), "items": out}
		}
	}

	// --- in-flight sessions ---
	sessions, err := statusSvc.ListSessions(ctx, workspace)
	if err != nil {
		snapshot["sessions"] = map[string]any{"error": err.Error()}
	} else {
		out := make([]map[string]any, 0)
		for _, s := range sessions {
			if isTerminalStatus(string(s.Status)) {
				continue
			}
			out = append(out, map[string]any{
				"id":          s.ID,
				"title":       s.Title,
				"status":      string(s.Status),
				"agent_count": s.AgentCount,
				"updated_at":  s.UpdatedAt,
			})
			if len(out) >= 8 {
				break
			}
		}
		snapshot["sessions"] = out
	}

	// --- active agents ---
	agents, err := statusSvc.ListActiveAgents(ctx)
	if err != nil {
		snapshot["agents"] = map[string]any{"error": err.Error()}
	} else {
		out := make([]map[string]any, 0, len(agents))
		for _, a := range agents {
			out = append(out, map[string]any{
				"agent_id":   a.AgentID,
				"agent_type": a.AgentType,
				"workspace":  a.Workspace,
				"status":     string(a.Status),
				"updated_at": a.UpdatedAt,
			})
		}
		snapshot["agents"] = out
	}

	return snapshot
}

func isTerminalStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "cancelled", "orphaned":
		return true
	}
	return false
}

// flattenText collapses untrusted plan-item titles to single-line plain text
// before they are interpolated into the snapshot strings an agent ingests —
// a title cannot then forge line structure in the tool result.
func flattenText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
