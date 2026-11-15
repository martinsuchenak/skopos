package cmd

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/paularlott/cli"
)

// `skopos inbox report` writes an item's report: what was asked, the
// approved plan, questions and answers, the decisions at each gate, the
// implementation, the reviews and what became of each finding.

func inboxReportCmds() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "report",
			Usage: "Write an item's report: the brief, the approved plan, questions and answers, the decisions at each gate, the implementation, the reviews, what became of each finding, and who worked on it",
			Flags: workflowFlags(
				&cli.StringFlag{Name: "level", Usage: "full (everything, verbatim) or summary (short enough for a comment); default: full"},
				&cli.StringFlag{Name: "format", Usage: "md, html or json (json is the whole record at either level); default: md"},
				&cli.StringFlag{Name: "out", Usage: "Write to this file instead of standard output"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				c, id, err := workflowClient(cmd)
				if err != nil {
					return err
				}
				q := url.Values{}
				if level := strings.ToLower(strings.TrimSpace(cmd.GetString("level"))); level != "" {
					if level != "full" && level != "summary" {
						return fmt.Errorf("--level is full or summary, not %q", level)
					}
					q.Set("level", level)
				}
				format := strings.ToLower(strings.TrimSpace(cmd.GetString("format")))
				switch format {
				case "":
					format = "md"
				case "md", "html", "json":
				default:
					return fmt.Errorf("--format is md, html or json, not %q", format)
				}
				q.Set("format", format)
				doc, err := c.text(ctx, "/api/inbox/"+url.PathEscape(id)+"/report?"+q.Encode(), "reading the report")
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
