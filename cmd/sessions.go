package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/paularlott/cli"
)

func init() {
	Register(sessionsCmd())
}

func sessionsCmd() *cli.Command {
	return &cli.Command{
		Name:  "sessions",
		Usage: "Inspect and maintain reported sessions",
		Commands: []*cli.Command{
			sessionsPurgeCmd(),
		},
	}
}

func sessionsPurgeCmd() *cli.Command {
	return &cli.Command{
		Name:  "purge",
		Usage: "Bulk-delete sessions. With --workspace, only that workspace's sessions (any key scoped to it); without it, EVERY session on the server (root key only)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
			// Deliberately NOT defaulted from the git remote: an empty value
			// is how purge-everything is expressed.
			&cli.StringFlag{Name: "workspace", Usage: "Limit the purge to one workspace (omit for all workspaces, root key only)"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := strings.TrimSpace(cmd.GetString("workspace"))
			deleted, err := sessionsDoPurge(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), ws)
			if err != nil {
				return err
			}
			if ws != "" {
				fmt.Printf("deleted %d sessions in %s\n", deleted, ws)
			} else {
				fmt.Printf("deleted %d sessions (all workspaces)\n", deleted)
			}
			return nil
		},
	}
}

func sessionsDoPurge(ctx context.Context, serverURL, apiKey, workspaceID string) (int, error) {
	u := strings.TrimRight(serverURL, "/") + "/api/sessions"
	if workspaceID != "" {
		u += "?" + url.Values{"workspace_id": []string{workspaceID}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("purging sessions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s", apiErrorMessage("purging sessions", resp))
	}
	var out struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decoding response: %w", err)
	}
	return out.Deleted, nil
}
