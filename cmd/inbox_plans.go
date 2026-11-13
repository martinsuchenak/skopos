package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/paularlott/cli"
)

// `skopos inbox plans` writes an item's plans out as Markdown: every
// planner's plan of a planning round in full with its questions, the
// comparison and the synthesis.

func inboxPlansCmds() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "plans",
			Usage: "Write an item's plans as Markdown: every planner's plan in full with its questions, the comparison and the synthesis (the newest planning round; --round for an earlier one)",
			Flags: workflowFlags(
				&cli.IntFlag{Name: "round", Usage: "The planning round, from 1 (default: the newest)"},
				&cli.StringFlag{Name: "out", Usage: "Write to this file instead of standard output"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				path := "/api/inbox/" + url.PathEscape(id) + "/plans?format=md"
				if n := cmd.GetInt("round"); n > 0 {
					path += "&round=" + strconv.Itoa(n)
				}
				doc, err := c.text(ctx, path, "reading the plans")
				if err != nil {
					return err
				}
				out := strings.TrimSpace(cmd.GetString("out"))
				if out == "" {
					_, err = io.WriteString(os.Stdout, doc)
					return err
				}
				if err := os.WriteFile(out, []byte(doc), 0o644); err != nil {
					return err
				}
				fmt.Printf("wrote %s\n", out)
				return nil
			},
		},
	}
}

// text reads a response body as text (an export), with the same errors as
// call.
func (c *apiClient) text(ctx context.Context, path, verb string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Skopos-Via", "cli")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", verb, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s", apiErrorMessage(verb, resp))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return string(b), err
}
