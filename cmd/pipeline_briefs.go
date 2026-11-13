package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/paularlott/cli"
)

// `skopos workspace|group pipeline brief …`: the briefs held on the server
// for a pipeline's members (focus@runner). One held for a workspace or its
// group is used in place of the repository's .skopos/focus/<name>.md.

func pipelineBriefCmd(kind string, flags func(extra ...cli.Flag) []cli.Flag) *cli.Command {
	name := &cli.StringFlag{Name: "name", Usage: "The brief's name: the focus of focus@runner"}
	return &cli.Command{
		Name:  "brief",
		Usage: "Briefs held on the server for the pipeline's members (used in place of the repository's .skopos/focus/<name>.md)",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List the briefs (for a workspace: its own and the ones it gets from a group)",
				Flags: flags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := pipelinePath(kind, cmd)
					if err != nil {
						return err
					}
					var out struct {
						Briefs []pipeline.Brief `json:"briefs"`
					}
					if err := briefCall(ctx, cmd, http.MethodGet, p+"/briefs", nil, &out); err != nil {
						return err
					}
					if len(out.Briefs) == 0 {
						fmt.Println("no briefs are held here: members with a focus read the repository's .skopos/focus/<name>.md")
						return nil
					}
					for _, b := range out.Briefs {
						from := ""
						if b.Source != "" {
							from = "  " + b.Source
						}
						fmt.Printf("%-24s %6d bytes  %s by %s%s\n", b.Name, b.Bytes, b.CreatedAt.Local().Format("2006-01-02 15:04"), b.CreatedBy, from)
					}
					return nil
				},
			},
			{
				Name:  "show",
				Usage: "Print a brief's text (--history lists its revisions instead)",
				Flags: flags(name, &cli.BoolFlag{Name: "history", Usage: "List the revisions of the brief instead of its text"}),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := briefPath(kind, cmd)
					if err != nil {
						return err
					}
					var v pipeline.BriefView
					if err := briefCall(ctx, cmd, http.MethodGet, p, nil, &v); err != nil {
						return err
					}
					if !cmd.GetBool("history") {
						fmt.Println(v.Body)
						return nil
					}
					if len(v.History) == 0 {
						fmt.Printf("%s comes from %s: its history is shown there\n", v.Name, v.Source)
						return nil
					}
					for _, h := range v.History {
						what := fmt.Sprintf("%d bytes", h.Bytes)
						if h.Deleted {
							what = "removed"
						}
						fmt.Printf("%s  %s  %-12s by %s\n", h.Rev, h.CreatedAt.Local().Format("2006-01-02 15:04"), what, h.CreatedBy)
					}
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Save a brief from a file (or standard input with --file -)",
				Flags: flags(name, &cli.StringFlag{Name: "file", Usage: "The brief's text: a file, or - for standard input"}),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := briefPath(kind, cmd)
					if err != nil {
						return err
					}
					file := strings.TrimSpace(cmd.GetString("file"))
					if file == "" {
						return fmt.Errorf("--file is required (a file, or - for standard input)")
					}
					var raw []byte
					if file == "-" {
						raw, err = io.ReadAll(io.LimitReader(os.Stdin, pipeline.MaxBriefBytes+1))
					} else {
						raw, err = os.ReadFile(file)
					}
					if err != nil {
						return fmt.Errorf("reading the brief: %w", err)
					}
					var b pipeline.Brief
					if err := briefCall(ctx, cmd, http.MethodPut, p, map[string]string{"body": string(raw)}, &b); err != nil {
						return err
					}
					fmt.Printf("saved brief %s: %d bytes, revision %s\n", b.Name, b.Bytes, b.Rev)
					return nil
				},
			},
			{
				Name:  "delete",
				Usage: "Remove a brief held here (its members then get the group's, or the repository's file)",
				Flags: flags(name),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := briefPath(kind, cmd)
					if err != nil {
						return err
					}
					if err := briefCall(ctx, cmd, http.MethodDelete, p, nil, nil); err != nil {
						return err
					}
					fmt.Printf("removed brief %s\n", strings.TrimSpace(cmd.GetString("name")))
					return nil
				},
			},
		},
	}
}

func briefPath(kind string, cmd *cli.Command) (string, error) {
	p, err := pipelinePath(kind, cmd)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(cmd.GetString("name"))
	if name == "" {
		return "", fmt.Errorf("--name is required")
	}
	return p + "/briefs/" + url.PathEscape(name), nil
}

func briefCall(ctx context.Context, cmd *cli.Command, method, path string, body, out any) error {
	return apiDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), audit.ViaCLI, "briefs", method, path, body, out)
}
