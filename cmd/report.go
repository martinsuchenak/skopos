package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspace"
	"github.com/paularlott/cli"
)

func init() {
	Register(reportCmd())
}

func reportCmd() *cli.Command {
	return &cli.Command{
		Name:  "report",
		Usage: "Report an AI agent status update to skopos",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "session-id", Usage: "Session identifier"},
			&cli.StringFlag{Name: "agent-id", Usage: "Stable agent identifier"},
			&cli.StringFlag{Name: "agent-type", Usage: "Agent type, such as codex, gemini, claude-code, opencode, kiro, or praxis"},
			&cli.StringFlag{Name: "workspace", Usage: "Workspace or repository path"},
			&cli.StringFlag{Name: "status", Usage: "Status value"},
			&cli.IntFlag{Name: "progress", DefaultValue: -1, Usage: "Optional progress percentage from 0 to 100"},
			&cli.IntFlag{Name: "step-current", DefaultValue: -1, Usage: "Optional current step"},
			&cli.IntFlag{Name: "step-total", DefaultValue: -1, Usage: "Optional total steps"},
			&cli.StringFlag{Name: "message", Usage: "Short status message"},
			&cli.StringFlag{Name: "snippet", Usage: "Short output snippet"},
			&cli.StringFlag{Name: "metadata", Usage: "Optional metadata JSON object"},
			&cli.StringFlag{Name: "git-branch", Usage: "Optional current git branch"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			input, err := reportInputFromCommand(cmd)
			if err != nil {
				return err
			}
			result, err := postReport(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), input)
			if err != nil {
				return err
			}
			fmt.Printf("reported session=%s event=%s\n", result.SessionID, result.EventID)
			return nil
		},
	}
}

func reportInputFromCommand(cmd *cli.Command) (status.ReportInput, error) {
	input := status.ReportInput{
		SessionID: cmd.GetString("session-id"),
		AgentID:   cmd.GetString("agent-id"),
		AgentType: cmd.GetString("agent-type"),
		Workspace: cmd.GetString("workspace"),
		Status:    status.Status(cmd.GetString("status")),
		Message:   cmd.GetString("message"),
		Snippet:   cmd.GetString("snippet"),
		Metadata:  map[string]any{},
	}
	if progress := cmd.GetInt("progress"); progress >= 0 {
		input.Progress = &progress
	}
	if stepCurrent := cmd.GetInt("step-current"); stepCurrent >= 0 {
		input.StepCurrent = &stepCurrent
	}
	if stepTotal := cmd.GetInt("step-total"); stepTotal >= 0 {
		input.StepTotal = &stepTotal
	}
	input.GitBranch = cmd.GetString("git-branch")
	if rawMetadata := strings.TrimSpace(cmd.GetString("metadata")); rawMetadata != "" {
		if err := json.Unmarshal([]byte(rawMetadata), &input.Metadata); err != nil {
			return input, fmt.Errorf("invalid metadata JSON: %w", err)
		}
	}
	if input.Workspace == "" {
		if ws, err := workspace.Resolve("."); err == nil {
			input.Workspace = ws
		}
	}
	return input, nil
}

func postReport(ctx context.Context, serverURL, apiKey string, input status.ReportInput) (*status.ReportResult, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding report: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/api/reports", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("posting report: unexpected status %s", resp.Status)
	}

	var result status.ReportResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}
