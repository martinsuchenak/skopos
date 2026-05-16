package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/workspace"
	"github.com/paularlott/cli"
)

func init() {
	Register(blackboardCmd())
}

func blackboardCmd() *cli.Command {
	return &cli.Command{
		Name:  "blackboard",
		Usage: "Manage shared agent memory on the blackboard",
		Commands: []*cli.Command{
			blackboardWriteCmd(),
			blackboardReadCmd(),
			blackboardListCmd(),
			blackboardPromoteCmd(),
			blackboardDeleteCmd(),
		},
	}
}

func blackboardWriteCmd() *cli.Command {
	return &cli.Command{
		Name:  "write",
		Usage: "Write an entry to the blackboard",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "scope", Usage: "session, branch, or project"},
			&cli.StringFlag{Name: "branch", Usage: "Branch name (required for branch scope)"},
			&cli.StringFlag{Name: "session-id", Usage: "Session ID (required for session scope)"},
			&cli.StringFlag{Name: "type", Usage: "Entry type: finding, decision, bug, debt, warning, context"},
			&cli.StringFlag{Name: "title", Usage: "Short descriptive title"},
			&cli.StringFlag{Name: "content", Usage: "Detailed content"},
			&cli.StringFlag{Name: "code-ref", Usage: "Code reference, e.g. auth/jwt.go:45"},
			&cli.StringFlag{Name: "agent-id", Usage: "Agent identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := cmd.GetString("workspace")
			if ws == "" {
				if id, err := workspace.Resolve("."); err == nil {
					ws = id
				}
			}
			input := blackboard.WriteInput{
				Scope:         blackboard.Scope(cmd.GetString("scope")),
				BranchName:    cmd.GetString("branch"),
				SessionID:     cmd.GetString("session-id"),
				EntryType:     blackboard.EntryType(cmd.GetString("type")),
				Title:         cmd.GetString("title"),
				Content:       cmd.GetString("content"),
				CodeRef:       cmd.GetString("code-ref"),
				AuthorAgentID: cmd.GetString("agent-id"),
				WorkspaceID:   ws,
			}
			result, err := blackboardPostEntry(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), input)
			if err != nil {
				return err
			}
			fmt.Printf("written id=%s scope=%s\n", result.ID, result.Scope)
			return nil
		},
	}
}

func blackboardReadCmd() *cli.Command {
	return &cli.Command{
		Name:  "read",
		Usage: "Print the Knowledge Bundle markdown to stdout",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "branch", Usage: "Branch name"},
			&cli.StringFlag{Name: "session-id", Usage: "Session ID"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := cmd.GetString("workspace")
			if ws == "" {
				if id, err := workspace.Resolve("."); err == nil {
					ws = id
				}
			}
			bundle, err := blackboardGetBundle(ctx, cmd.GetString("server-url"), cmd.GetString("branch"), cmd.GetString("session-id"), ws)
			if err != nil {
				return err
			}
			fmt.Print(bundle.MarkdownBundle)
			return nil
		},
	}
}

func blackboardListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List blackboard entries in tabular form",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "branch", Usage: "Branch name"},
			&cli.StringFlag{Name: "session-id", Usage: "Session ID"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := cmd.GetString("workspace")
			if ws == "" {
				if id, err := workspace.Resolve("."); err == nil {
					ws = id
				}
			}
			bundle, err := blackboardGetBundle(ctx, cmd.GetString("server-url"), cmd.GetString("branch"), cmd.GetString("session-id"), ws)
			if err != nil {
				return err
			}
			if len(bundle.Entries) == 0 {
				fmt.Println("no entries")
				return nil
			}
			fmt.Printf("%-36s  %-10s  %-8s  %s\n", "ID", "TYPE", "SCOPE", "TITLE")
			for _, e := range bundle.Entries {
				fmt.Printf("%-36s  %-10s  %-8s  %s\n", e.ID, e.EntryType, e.Scope, e.Title)
			}
			return nil
		},
	}
}

func blackboardPromoteCmd() *cli.Command {
	return &cli.Command{
		Name:  "promote",
		Usage: "Promote an entry to a wider scope",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "id", Usage: "Entry ID to promote"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.GetString("id")
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			return blackboardPatchPromote(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id)
		},
	}
}

func blackboardDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:  "delete",
		Usage: "Delete a blackboard entry",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "id", Usage: "Entry ID to delete"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.GetString("id")
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			return blackboardDoDelete(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id)
		},
	}
}

func blackboardPostEntry(ctx context.Context, serverURL, apiKey string, input blackboard.WriteInput) (*blackboard.WriteResult, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/api/blackboard/entries",
		bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting entry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("posting entry: unexpected status %s", resp.Status)
	}
	var result blackboard.WriteResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

func blackboardGetBundle(ctx context.Context, serverURL, branch, sessionID, workspaceID string) (*blackboard.Bundle, error) {
	base := strings.TrimRight(serverURL, "/") + "/api/blackboard/entries"
	q := url.Values{}
	if branch != "" {
		q.Set("branch", branch)
	}
	if sessionID != "" {
		q.Set("session_id", sessionID)
	}
	if workspaceID != "" {
		q.Set("workspace", workspaceID)
	}
	u := base
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching bundle: unexpected status %s", resp.Status)
	}
	var bundle blackboard.Bundle
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		return nil, fmt.Errorf("decoding bundle: %w", err)
	}
	return &bundle, nil
}

func blackboardPatchPromote(ctx context.Context, serverURL, apiKey, id string) error {
	u := strings.TrimRight(serverURL, "/") + "/api/blackboard/entries/" + url.PathEscape(id) + "/promote"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("promoting entry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("promoting entry: unexpected status %s", resp.Status)
	}
	fmt.Printf("promoted %s\n", id)
	return nil
}

func blackboardDoDelete(ctx context.Context, serverURL, apiKey, id string) error {
	u := strings.TrimRight(serverURL, "/") + "/api/blackboard/entries/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleting entry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("deleting entry: unexpected status %s", resp.Status)
	}
	fmt.Printf("deleted %s\n", id)
	return nil
}
