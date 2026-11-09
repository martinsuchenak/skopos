package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/paularlott/cli"
)

// `skopos questions` is the decision log across items (GET
// /api/inbox/questions); `skopos questions summary` its counts.

func init() {
	Register(questionsCmd())
}

func questionFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}, ConfigPath: []string{"client.server_url"}},
		&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}, ConfigPath: []string{"client.api_key"}},
		&cli.StringFlag{Name: "workspace", Usage: "Only this workspace"},
		&cli.StringFlag{Name: "item", Usage: "Only this item"},
		&cli.StringFlag{Name: "state", Usage: "open, answered, answered_free_text, skipped, superseded, or free (answers to no question)"},
		&cli.StringFlag{Name: "asker", Usage: "planner, implementer or worker"},
		&cli.StringFlag{Name: "answered-by", Usage: `Who answered, as recorded ("key martin", "root")`},
		&cli.StringFlag{Name: "from", Usage: "Asked from this local date (2026-11-01) or RFC 3339 time"},
		&cli.StringFlag{Name: "to", Usage: "Asked up to the end of this local date, or before this RFC 3339 time"},
	}
}

func questionQuery(cmd *cli.Command) url.Values {
	v := url.Values{}
	for flag, param := range map[string]string{"workspace": "workspace_id", "item": "item_id", "state": "state",
		"asker": "asker", "answered-by": "answered_by"} {
		if s := strings.TrimSpace(cmd.GetString(flag)); s != "" {
			v.Set(param, s)
		}
	}
	// Dates are this machine's days: sent as the local day's start (and,
	// for --to, the next day's), not as UTC days.
	for _, flag := range []string{"from", "to"} {
		if s := strings.TrimSpace(cmd.GetString(flag)); s != "" {
			v.Set(flag, localDay(s, flag == "to"))
		}
	}
	return v
}

// localDay turns a date into the RFC 3339 start of that local day (end: of
// the next day); anything else is passed on as given.
func localDay(s string, end bool) string {
	d, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return s
	}
	if end {
		d = d.AddDate(0, 0, 1)
	}
	return d.Format(time.RFC3339)
}

func questionsClient(cmd *cli.Command) *apiClient {
	return &apiClient{base: strings.TrimRight(cmd.GetString("server-url"), "/"), key: cmd.GetString("api-key")}
}

func questionsCmd() *cli.Command {
	return &cli.Command{
		Name:  "questions",
		Usage: "The decision log across items: one row per question with its state and current answer (who, how, with which approval, the wait); --item narrows it to one item (its full record: skopos inbox questions --id)",
		Flags: append(questionFlags(), &cli.StringFlag{Name: "format", DefaultValue: "table", Usage: "table, csv or json"}),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			c := questionsClient(cmd)
			q := questionQuery(cmd)
			switch cmd.GetString("format") {
			case "csv":
				q.Set("format", "csv")
				return c.raw(ctx, "/api/inbox/questions?"+q.Encode(), os.Stdout)
			case "json", "table":
			default:
				return fmt.Errorf("--format: table, csv or json")
			}
			var rows []qa.Row
			q.Set("limit", "1000")
			for offset := 0; ; {
				q.Set("offset", fmt.Sprint(offset))
				var page struct {
					Rows  []qa.Row `json:"rows"`
					Total int      `json:"total"`
				}
				if err := c.call(ctx, http.MethodGet, "/api/inbox/questions?"+q.Encode(), nil, &page, "reading the decision log"); err != nil {
					return err
				}
				rows = append(rows, page.Rows...)
				offset += len(page.Rows)
				if len(page.Rows) == 0 || offset >= page.Total {
					break
				}
			}
			if cmd.GetString("format") == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			printDecisionLog(os.Stdout, rows)
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:  "summary",
				Usage: "Counts over the decision log: answered, in free text, skipped, open, corrections, median wait — in total, per workspace and per person",
				Flags: append(questionFlags(), &cli.StringFlag{Name: "format", DefaultValue: "table", Usage: "table or json"}),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					var sum qa.Summary
					if err := questionsClient(cmd).call(ctx, http.MethodGet, "/api/inbox/questions/summary?"+questionQuery(cmd).Encode(), nil, &sum, "reading the summary"); err != nil {
						return err
					}
					if cmd.GetString("format") == "json" {
						enc := json.NewEncoder(os.Stdout)
						enc.SetIndent("", "  ")
						return enc.Encode(sum)
					}
					printSummary(os.Stdout, sum)
					return nil
				},
			},
		},
	}
}

// raw writes a response body as it comes (the CSV export).
func (c *apiClient) raw(ctx context.Context, path string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Skopos-Via", "cli")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s", apiErrorMessage("exporting the decision log", resp))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func printDecisionLog(w io.Writer, rows []qa.Row) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "no questions")
		return
	}
	defer func() {
		_, _ = fmt.Fprintln(w, "\nAn item's full record, with the ids to correct an answer: skopos inbox questions --id <item>")
	}()
	item := ""
	for _, r := range rows {
		if r.ItemID != item {
			item = r.ItemID
			title := r.ItemTitle
			if title == "" {
				title = "(deleted item)"
			}
			_, _ = fmt.Fprintf(w, "\n%s  %s  %s\n", title, r.ItemID, r.WorkspaceID)
		}
		if r.Kind == "free" {
			_, _ = fmt.Fprintf(w, "  free answer: %s — %s via %s, %s\n", r.Answer, r.AnsweredBy, r.Via, when(r.AnsweredAt))
			continue
		}
		who := r.Label
		if who == "" {
			who = r.Asker
		}
		_, _ = fmt.Fprintf(w, "  %s %d. %s  [%s]\n", who, r.N, r.Question, strings.ReplaceAll(r.State, "_", " "))
		if r.AnsweredBy != "" {
			ans := r.Answer
			if r.Option != "" {
				ans = strings.TrimSpace("(" + r.Option + ") " + r.OptionText)
				if r.Answer != "" {
					ans += " — " + r.Answer
				}
			}
			if r.State == qa.StateSkipped {
				ans = "left unanswered"
			}
			extra := ""
			if r.Corrections > 0 {
				extra = fmt.Sprintf(", %d correction(s)", r.Corrections)
			}
			if r.AfterUse {
				extra += " after use"
			}
			_, _ = fmt.Fprintf(w, "     %s — %s via %s, %s, %s%s\n", ans, r.AnsweredBy, r.Via, strings.ReplaceAll(r.Action, "_", " "), when(r.AnsweredAt), extra)
		}
	}
}

func when(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

func printSummary(w io.Writer, s qa.Summary) {
	line := func(name string, c qa.Counts, person bool) {
		if person {
			_, _ = fmt.Fprintf(w, "  %-40s answered %d, free text %d, skipped %d, free answers %d, corrections %d (%d after use), median wait %s\n",
				name, c.Answered, c.AnsweredFreeText, c.Skipped, c.FreeAnswers, c.Corrections, c.CorrectionsAfterUse, wait(c))
			return
		}
		_, _ = fmt.Fprintf(w, "  %-40s asked %d: answered %d, free text %d, skipped %d, open %d, superseded %d; free answers %d; corrections %d (%d after use); median wait %s\n",
			name, c.Asked, c.Answered, c.AnsweredFreeText, c.Skipped, c.Open, c.Superseded, c.FreeAnswers, c.Corrections, c.CorrectionsAfterUse, wait(c))
	}
	_, _ = fmt.Fprintln(w, "Total")
	line("", s.Total, false)
	_, _ = fmt.Fprintln(w, "\nBy workspace")
	for _, k := range sortedKeys(s.ByWorkspace) {
		line(k, s.ByWorkspace[k], false)
	}
	_, _ = fmt.Fprintln(w, "\nBy person")
	for _, k := range sortedKeys(s.ByPerson) {
		line(k, s.ByPerson[k], true)
	}
}

// wait is the median wait, "-" when no live answer measured one.
func wait(c qa.Counts) string {
	if c.Measured == 0 {
		return "-"
	}
	return (time.Duration(c.MedianWaitSeconds) * time.Second).String() + fmt.Sprintf(" (%d)", c.Measured)
}

func sortedKeys(m map[string]qa.Counts) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
