package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/paularlott/cli"
)

// `skopos inbox findings` reads an item's record of review findings;
// `skopos inbox waive` accepts one as it is. Findings are sent back to be
// fixed with `request-changes --finding` (or `retry --finding`).

func inboxFindingCmds() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "findings",
			Usage: "An item's review findings: what each review found, its state (open, fixed, addressed, still open, waived, noted) and what happened to it, with the ids request-changes --finding and waive take",
			Flags: workflowFlags(
				&cli.StringFlag{Name: "state", Usage: "Only findings in this state: open, fixed, addressed, still_open, waived or noted"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				var out struct {
					Findings []findings.Finding `json:"findings"`
				}
				if err := c.call(ctx, http.MethodGet, "/api/inbox/"+url.PathEscape(id)+"/findings", nil, &out, "reading the findings"); err != nil {
					return err
				}
				printFindings(os.Stdout, out.Findings, strings.TrimSpace(cmd.GetString("state")))
				return nil
			},
		},
		{
			Name:  "waive",
			Usage: "Accept a review finding as it is: the implementer is not asked to fix it and no review checks it or blocks on it again",
			Flags: workflowFlags(
				&cli.StringFlag{Name: "finding", Usage: "The finding's id (skopos inbox findings lists them)"},
				&cli.StringFlag{Name: "note", Usage: "Why it is accepted (reviewers read it; required for a blocking finding)"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				fid := strings.TrimSpace(cmd.GetString("finding"))
				if fid == "" {
					return fmt.Errorf("--finding is required (skopos inbox findings lists the ids)")
				}
				var f findings.Finding
				path := "/api/inbox/" + url.PathEscape(id) + "/findings/" + url.PathEscape(fid) + "/waive"
				if err := c.call(ctx, http.MethodPost, path, map[string]string{"note": strings.TrimSpace(cmd.GetString("note"))}, &f, "waiving the finding"); err != nil {
					return err
				}
				fmt.Printf("waived: %s\n", oneLine(f.Text, 100))
				return nil
			},
		},
	}
}

// findingIDs is the --finding flags of request-changes and retry.
func findingIDs(cmd *cli.Command) []string {
	var out []string
	for _, v := range cmd.GetStringSlice("finding") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}

// printFindings writes the record: a count by state, then each finding
// with what happened to it, oldest first.
func printFindings(w io.Writer, fs []findings.Finding, state string) {
	if len(fs) == 0 {
		_, _ = fmt.Fprintln(w, "No review findings recorded for this item.")
		return
	}
	counts := map[string]int{}
	for _, f := range fs {
		counts[f.State]++
	}
	var parts []string
	for _, s := range []string{findings.StateOpen, findings.StateStillOpen, findings.StateFixed, findings.StateAddressed, findings.StateWaived, findings.StateNoted} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], strings.ReplaceAll(s, "_", " ")))
		}
	}
	_, _ = fmt.Fprintf(w, "%d finding(s): %s\n", len(fs), strings.Join(parts, ", "))
	for _, f := range fs {
		if state != "" && f.State != state {
			continue
		}
		head := ""
		if len(f.HeadSHA) >= 7 {
			head = " @ " + f.HeadSHA[:7]
		}
		file := ""
		if f.File != "" {
			file = "  " + f.File
		}
		_, _ = fmt.Fprintf(w, "\n%s  [%s] %s, %s%s%s\n", f.ID, strings.ReplaceAll(f.State, "_", " "), f.Severity, f.By(), head, file)
		_, _ = fmt.Fprintf(w, "  %s\n", f.Text)
		for _, u := range f.Updates {
			_, _ = fmt.Fprintf(w, "  - %s\n", findingUpdateLine(u))
		}
	}
}

func findingUpdateLine(u findings.Update) string {
	note := ""
	if u.Note != "" {
		note = ": " + u.Note
	}
	when := u.CreatedAt.Local().Format("2006-01-02 15:04")
	switch u.Kind {
	case findings.KindSentBack:
		return fmt.Sprintf("%s  sent back to be fixed by %s", when, u.Actor)
	case findings.KindWaived:
		return fmt.Sprintf("%s  waived by %s%s", when, u.Actor, note)
	case findings.KindFixed:
		return fmt.Sprintf("%s  the implementer says fixed%s", when, note)
	case findings.KindNotFixed:
		return fmt.Sprintf("%s  the implementer left it%s", when, note)
	case findings.KindAddressed:
		return fmt.Sprintf("%s  a review confirmed it is fixed%s", when, note)
	case findings.KindStillOpen:
		return fmt.Sprintf("%s  a review found it not fixed%s", when, note)
	}
	return when + "  " + u.Kind + note
}
