package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/paularlott/cli"
)

func init() {
	Register(inboxCmd())
}

func inboxCmd() *cli.Command {
	return &cli.Command{
		Name:  "inbox",
		Usage: "Manage the workspace inbox of unprocessed work",
		Commands: []*cli.Command{
			inboxAddCmd(),
			inboxListCmd(),
			inboxShowCmd(),
			inboxUpdateCmd(),
			inboxClaimCmd(),
			inboxConvertCmd(),
			inboxFileCmd(),
			inboxRestoreCmd(),
			inboxDiscardCmd(),
			inboxCompleteCmd(),
			inboxReopenCmd(),
			inboxPurgeCmd(),
			inboxDeleteCmd(),
		},
	}
}

// inboxContent resolves item content from --content, --file <path>, or
// --file - (stdin). An empty result means "leave unchanged" on updates.
func inboxContent(contentFlag, fileFlag string) (string, error) {
	if fileFlag == "" {
		return contentFlag, nil
	}
	if fileFlag == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(fileFlag)
	if err != nil {
		return "", fmt.Errorf("reading --file: %w", err)
	}
	return string(data), nil
}

func inboxAddCmd() *cli.Command {
	return &cli.Command{
		Name:  "add",
		Usage: "Capture an inbox item (markdown content via --content, --file <path>, or --file - for stdin)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "title", Usage: "Item title"},
			&cli.StringFlag{Name: "content", Usage: "Markdown content"},
			&cli.StringFlag{Name: "file", Usage: "Read markdown content from this file ('-' for stdin)"},
			&cli.StringSliceFlag{Name: "tag", Usage: "Tag (repeatable)"},
			&cli.IntFlag{Name: "priority", Usage: "Explicit priority (>= 1, smaller first; unprioritized items sort last)"},
			&cli.StringFlag{Name: "agent-id", Usage: "Author identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}, DefaultValue: "user"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
			&cli.BoolFlag{Name: "unfiled", Usage: "Capture without a workspace (decide later; root key only)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			content, err := inboxContent(cmd.GetString("content"), cmd.GetString("file"))
			if err != nil {
				return err
			}
			ws := workspaceOrDefault(cmd.GetString("workspace"))
			if cmd.GetBool("unfiled") {
				ws = ""
			}
			item, err := inboxPost(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), inbox.CreateInput{
				Title:         cmd.GetString("title"),
				Content:       content,
				Tags:          cmd.GetStringSlice("tag"),
				Priority:      cmd.GetInt("priority"),
				AuthorAgentID: cmd.GetString("agent-id"),
				WorkspaceID:   ws,
			})
			if err != nil {
				return err
			}
			if item.WorkspaceID == "" {
				fmt.Printf("captured id=%s status=%s UNFILED title=%q (file it later: skopos inbox file --id %s --workspace <id>)\n", item.ID, item.Status, item.Title, item.ID)
				return nil
			}
			fmt.Printf("captured id=%s status=%s title=%q\n", item.ID, item.Status, item.Title)
			return nil
		},
	}
}

func inboxListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List inbox items",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "status", Usage: "Filter by status: open, in_progress, converted, done, discarded", DefaultValue: "open"},
			&cli.StringFlag{Name: "tag", Usage: "Filter by tag"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			items, err := inboxGetList(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				cmd.GetString("status"), cmd.GetString("tag"), cmd.GetString("workspace"))
			if err != nil {
				return err
			}
			if len(items) == 0 {
				fmt.Println("no inbox items")
				return nil
			}
			fmt.Printf("%-36s  %-11s  %-6s  %s\n", "ID", "STATUS", "AGE", "TITLE")
			for _, it := range items {
				plan := ""
				if it.PlanID != "" {
					short := it.PlanID
					if len(short) > 8 {
						short = short[:8]
					}
					plan = " →" + short
				}
				rank := ""
				if it.Priority != nil {
					rank = fmt.Sprintf("#%d ", *it.Priority)
				}
				if it.WorkspaceID == "" {
					rank += "[unfiled] "
				}
				fmt.Printf("%-36s  %-11s  %-6s  %s%s%s\n", it.ID, it.Status, age(it.CreatedAt), rank, it.Title, plan)
			}
			return nil
		},
	}
}

func inboxShowCmd() *cli.Command {
	return &cli.Command{
		Name:  "show",
		Usage: "Show an inbox item (raw markdown)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			item, err := inboxGetOne(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id)
			if err != nil {
				return err
			}
			fmt.Printf("Item: %s (%s)\n", item.Title, item.Status)
			if item.WorkspaceID == "" {
				fmt.Println("Workspace: (unfiled)")
			} else {
				fmt.Printf("Workspace: %s\n", item.WorkspaceID)
			}
			if len(item.Tags) > 0 {
				fmt.Printf("Tags: %s\n", strings.Join(item.Tags, ", "))
			}
			if item.ClaimedByAgentID != "" {
				fmt.Printf("Claimed by: %s\n", item.ClaimedByAgentID)
			}
			if item.PlanID != "" {
				fmt.Printf("Plan: %s\n", item.PlanID)
				if item.Plan != nil {
					fmt.Printf("Plan name: %s (%s)\n", item.Plan.Name, item.Plan.Status)
				}
			}
			fmt.Printf("Created: %s\n", item.CreatedAt.Format("2006-01-02 15:04"))
			if item.Content != "" {
				fmt.Println()
				fmt.Println(item.Content)
			}
			return nil
		},
	}
}

func inboxUpdateCmd() *cli.Command {
	return &cli.Command{
		Name:  "update",
		Usage: "Enrich an item (title/content/tags; content via --content or --file)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
			&cli.StringFlag{Name: "title", Usage: "New title"},
			&cli.StringFlag{Name: "content", Usage: "New markdown content"},
			&cli.StringFlag{Name: "file", Usage: "Read new markdown content from this file ('-' for stdin)"},
			&cli.StringSliceFlag{Name: "tag", Usage: "Replace tags (repeatable)"},
			&cli.StringFlag{Name: "priority", Usage: "Set priority: a number >= 1, or 'clear' to unpin (smaller first; unprioritized items sort last)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			content, err := inboxContent(cmd.GetString("content"), cmd.GetString("file"))
			if err != nil {
				return err
			}
			input := inbox.UpdateInput{Title: cmd.GetString("title"), Content: content}
			if tags := cmd.GetStringSlice("tag"); tags != nil {
				input.Tags = &tags
			}
			switch p := strings.TrimSpace(cmd.GetString("priority")); p {
			case "":
				// unchanged
			case "clear", "none", "-":
				zero := 0
				input.Priority = &zero
			default:
				n, err := strconv.Atoi(p)
				if err != nil || n < 1 {
					return fmt.Errorf("--priority must be a number >= 1 or 'clear'")
				}
				input.Priority = &n
			}
			if err := inboxPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id, input); err != nil {
				return err
			}
			fmt.Println("updated")
			return nil
		},
	}
}

func inboxClaimCmd() *cli.Command {
	return &cli.Command{
		Name:  "claim",
		Usage: "Claim an item for processing (omit --agent-id to release)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
			&cli.StringFlag{Name: "agent-id", Usage: "Agent identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			payload, err := json.Marshal(map[string]string{"agent_id": cmd.GetString("agent-id")})
			if err != nil {
				return err
			}
			if err := inboxAction(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id, "claim", payload, "claiming item"); err != nil {
				return err
			}
			fmt.Println("done")
			return nil
		},
	}
}

func inboxConvertCmd() *cli.Command {
	return &cli.Command{
		Name:  "convert",
		Usage: "Link a plan and mark the item converted",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
			&cli.StringFlag{Name: "plan-id", Usage: "Plan ID (same workspace)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			planID := strings.TrimSpace(cmd.GetString("plan-id"))
			if id == "" || planID == "" {
				return fmt.Errorf("--id and --plan-id are required")
			}
			converted, err := inboxConvertItem(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id, planID)
			if err != nil {
				return err
			}
			fmt.Printf("converted plan=%s\n", converted.PlanID)
			return nil
		},
	}
}

func inboxFileCmd() *cli.Command {
	return &cli.Command{
		Name:  "file",
		Usage: "File an unfiled (or misfiled) item into a workspace",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
			&cli.StringFlag{Name: "workspace", Usage: "Target workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			ws := strings.TrimSpace(cmd.GetString("workspace"))
			if id == "" || ws == "" {
				return fmt.Errorf("--id and --workspace are required")
			}
			if err := inboxPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id,
				inbox.UpdateInput{WorkspaceID: ws}); err != nil {
				return err
			}
			fmt.Printf("filed into %s\n", ws)
			return nil
		},
	}
}

func inboxDiscardCmd() *cli.Command {
	return &cli.Command{
		Name:  "discard",
		Usage: "Discard an item (won't do)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			if err := inboxAction(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id, "discard", []byte("{}"), "discarding item"); err != nil {
				return err
			}
			fmt.Println("discarded")
			return nil
		},
	}
}

func inboxCompleteCmd() *cli.Command {
	return &cli.Command{
		Name:  "complete",
		Usage: "Manually mark an item done (work finished without a plan, or ahead of it)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			if err := inboxAction(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id, "complete", []byte("{}"), "completing item"); err != nil {
				return err
			}
			fmt.Println("done")
			return nil
		},
	}
}

func inboxReopenCmd() *cli.Command {
	return &cli.Command{
		Name:  "reopen",
		Usage: "Bring a done item back to open (undo a wrong complete; clears claim, plan link, priority)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			if err := inboxAction(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id, "reopen", []byte("{}"), "reopening item"); err != nil {
				return err
			}
			fmt.Println("reopened")
			return nil
		},
	}
}

func inboxPurgeCmd() *cli.Command {
	return &cli.Command{
		Name:  "purge",
		Usage: "Bulk-delete items in one workspace (optionally one status; omit for every status)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "status", Usage: "Limit the purge to one status: open, in_progress, converted, done, or discarded"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID (defaults to this checkout's workspace)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := workspaceOrDefault(cmd.GetString("workspace"))
			deleted, err := inboxDoPurge(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), ws, strings.TrimSpace(cmd.GetString("status")))
			if err != nil {
				return err
			}
			if strings.TrimSpace(cmd.GetString("status")) != "" {
				fmt.Printf("deleted %d %s items in %s\n", deleted, cmd.GetString("status"), ws)
			} else {
				fmt.Printf("deleted %d items (all statuses) in %s\n", deleted, ws)
			}
			return nil
		},
	}
}

func inboxDoPurge(ctx context.Context, serverURL, apiKey, workspaceID, status string) (int, error) {
	q := url.Values{}
	q.Set("workspace_id", workspaceID)
	if status != "" {
		q.Set("status", status)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/inbox?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("purging inbox items: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s", apiErrorMessage("purging inbox items", resp))
	}
	var out struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decoding response: %w", err)
	}
	return out.Deleted, nil
}

func inboxRestoreCmd() *cli.Command {
	return &cli.Command{
		Name:  "restore",
		Usage: "Restore a discarded item back to open",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			if err := inboxAction(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id, "restore", []byte("{}"), "restoring item"); err != nil {
				return err
			}
			fmt.Println("restored")
			return nil
		},
	}
}

func inboxDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:  "delete",
		Usage: "Hard-delete an item",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			&cli.StringFlag{Name: "id", Usage: "Item ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.GetString("id"))
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			if err := inboxDelete(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id); err != nil {
				return err
			}
			fmt.Println("deleted")
			return nil
		},
	}
}

// age renders a coarse "2h"-style age for list output.
func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func inboxPost(ctx context.Context, serverURL, apiKey string, input inbox.CreateInput) (*inbox.Item, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/api/inbox", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("capturing item: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", apiErrorMessage("capturing item", resp))
	}
	var item inbox.Item
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &item, nil
}

func inboxGetList(ctx context.Context, serverURL, apiKey, status, tag, workspaceID string) ([]inbox.Item, error) {
	q := url.Values{}
	if status != "" && status != "all" {
		q.Set("status", status)
	}
	if tag != "" {
		q.Set("tag", tag)
	}
	if workspaceID != "" {
		q.Set("workspace", workspaceID)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/inbox"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing inbox: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", apiErrorMessage("listing inbox", resp))
	}
	var items []inbox.Item
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("decoding inbox: %w", err)
	}
	return items, nil
}

func inboxGetOne(ctx context.Context, serverURL, apiKey, id string) (*inbox.Item, error) {
	u := strings.TrimRight(serverURL, "/") + "/api/inbox/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting item: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", apiErrorMessage("getting item", resp))
	}
	var item inbox.Item
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, fmt.Errorf("decoding item: %w", err)
	}
	return &item, nil
}

func inboxPatch(ctx context.Context, serverURL, apiKey, id string, input inbox.UpdateInput) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/inbox/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("updating item: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s", apiErrorMessage("updating item", resp))
	}
	return nil
}

// inboxAction POSTs a JSON payload to an item sub-resource (claim/convert/discard).
func inboxAction(ctx context.Context, serverURL, apiKey, id, action string, payload []byte, verb string) error {
	u := strings.TrimRight(serverURL, "/") + "/api/inbox/" + url.PathEscape(id) + "/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s", apiErrorMessage(verb, resp))
	}
	return nil
}

// inboxConvertItem links a plan and returns the converted item.
func inboxConvertItem(ctx context.Context, serverURL, apiKey, id, planID string) (*inbox.Item, error) {
	payload, err := json.Marshal(inbox.ConvertInput{PlanID: planID})
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/inbox/" + url.PathEscape(id) + "/convert"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("converting item: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", apiErrorMessage("converting item", resp))
	}
	var item inbox.Item
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, fmt.Errorf("decoding item: %w", err)
	}
	return &item, nil
}

func inboxDelete(ctx context.Context, serverURL, apiKey, id string) error {
	u := strings.TrimRight(serverURL, "/") + "/api/inbox/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleting item: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s", apiErrorMessage("deleting item", resp))
	}
	return nil
}
