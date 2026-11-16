package cmd

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/martinsuchenak/skopos/internal/itemreport"
	"github.com/paularlott/cli"
)

// `skopos inbox summary`: how many plans the server approved, how many of
// those items then needed no input until sign-off, and why plans were held.

func inboxSummaryCmds() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "summary",
			Usage: "Count the plans approved automatically: how many needed no input until sign-off, why plans were held, and items closed with nothing to do",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
				&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
				&cli.StringFlag{Name: "since", Usage: "The period: 30d, 12h or a date (2026-03-01); default 30d"},
				&cli.StringFlag{Name: "workspace", Usage: "One workspace (default: every workspace the key may read)"},
			},
			Run: func(ctx context.Context, cmd *cli.Command) error {
				q := url.Values{}
				if v := strings.TrimSpace(cmd.GetString("since")); v != "" {
					q.Set("since", v)
				}
				if v := strings.TrimSpace(cmd.GetString("workspace")); v != "" {
					q.Set("workspace", v)
				}
				var s itemreport.Summary
				if err := apiDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), "cli", "the summary", "GET", "/api/inbox/summary?"+q.Encode(), nil, &s); err != nil {
					return err
				}
				fmt.Printf("since %s\n\n", s.Since.Local().Format("2006-01-02 15:04"))
				fmt.Printf("%-40s %-12s %6s %8s %6s %5s %-28s %8s\n", "WORKSPACE", "TEMPLATE", "AUTO", "NO INPUT", "INPUT", "OPEN", "HELD", "NOTHING")
				line := func(r itemreport.SummaryRow, ws, tmpl string) {
					fmt.Printf("%-40s %-12s %6d %8d %6d %5d %-28s %8d\n", ws, tmpl, r.AutoApproved, r.NoInputAfter, r.InputAfter, r.Open, heldWords(r.Held), r.NothingToDo)
				}
				for _, r := range s.Rows {
					tmpl := r.Template
					if tmpl == "" {
						tmpl = "-"
					}
					line(r, r.WorkspaceID, tmpl)
				}
				line(s.Total, "total", "")
				if s.Cut {
					fmt.Println("\nThe period holds more entries than one summary reads: the oldest are left out.")
				}
				return nil
			},
		},
	}
}

func heldWords(m map[string]int) string {
	if len(m) == 0 {
		return "0"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
