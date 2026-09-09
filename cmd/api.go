package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/workspace"
)

// apiErrorMessage builds a CLI error for a non-2xx API response, surfacing the
// server's validation message when present instead of only the status code.
func apiErrorMessage(action string, resp *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && body.Error != "" {
		return fmt.Sprintf("%s: %s", action, body.Error)
	}
	return fmt.Sprintf("%s: unexpected status %s", action, resp.Status)
}

// workspaceOrDefault resolves the workspace to use: an explicit flag value
// wins; otherwise the git remote of the current directory.
func workspaceOrDefault(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if id, err := workspace.Resolve("."); err == nil {
		return id
	}
	return ""
}
