package mcp

import (
	"context"
	"time"

	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/qa"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterInboxTool(registerQuestionsTool)
}

// maxQuestionRows caps the decision log an agent reads in one call.
const maxQuestionRows = 200

// registerQuestionsTool: the questions-and-answers record, read only. With
// an item (item_id, or workspace_id + priority): its sets, each question's
// state and answers, and its free answers. Without: the decision log across
// items in scope, filtered.
func registerQuestionsTool(server *mcplib.Server, service *inbox.Service) {
	registerTool(server,
		mcplib.NewTool("inbox_questions",
			"Read the questions the planner and implementer asked and the answers given. With item_id (or workspace_id + priority): that item's record — each question's state (open, answered, answered_free_text, skipped, superseded) and its answers with who answered, how and with which approval. Without an item: the decision log across items, newest last, filtered by workspace_id, state, asker and a from/to date (at most 200 rows).",
			itemRefToolParams(
				mcplib.String("state", "Decision log filter: open, answered, answered_free_text, skipped, superseded, or free (answers to no question)"),
				mcplib.String("asker", "Decision log filter: planner, implementer or worker"),
				mcplib.String("from", "Decision log filter: asked from this UTC date (2026-11-01)"),
				mcplib.String("to", "Decision log filter: asked up to and including this UTC date"),
			)...,
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			if req.StringOr("item_id", "") != "" || req.IntOr("priority", 0) > 0 {
				itemID, err := resolveItemID(ctx, service, req)
				if err != nil {
					return nil, toolError(err)
				}
				rec, err := service.Questions(ctx, itemID)
				if err != nil {
					return nil, toolError(err)
				}
				return mcplib.NewToolResponseJSON(rec), nil
			}
			f := qa.Filter{WorkspaceID: req.StringOr("workspace_id", ""), State: req.StringOr("state", ""), Asker: req.StringOr("asker", "")}
			var err error
			if f.From, err = toolDate(req.StringOr("from", ""), false); err != nil {
				return nil, err // an input error already
			}
			if f.To, err = toolDate(req.StringOr("to", ""), true); err != nil {
				return nil, err
			}
			rows, err := service.SearchQuestions(ctx, f)
			if err != nil {
				return nil, toolError(err)
			}
			total := len(rows)
			if total > maxQuestionRows {
				rows = rows[total-maxQuestionRows:]
			}
			return mcplib.NewToolResponseJSON(map[string]any{"rows": rows, "total": total, "returned": len(rows)}), nil
		},
	)
}

// toolDate reads a YYYY-MM-DD date; end: the start of the next day.
func toolDate(v string, end bool) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	d, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, mcplib.NewToolErrorInvalidParams("dates are YYYY-MM-DD: " + v)
	}
	if end {
		d = d.AddDate(0, 0, 1)
	}
	return d, nil
}
