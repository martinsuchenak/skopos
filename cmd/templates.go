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
	"github.com/martinsuchenak/skopos/internal/labels"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/paularlott/cli"
)

// `skopos workspace|group template …`: item templates held on the server — a
// request that repeats, from which an item is started (skopos inbox add
// --template <name>).

func templateCmd(kind string) *cli.Command {
	ownerFlag := &cli.StringFlag{Name: "workspace", Usage: "Workspace ID (default: resolved from this checkout's git remote)"}
	if kind == pipeline.KindGroup {
		ownerFlag = &cli.StringFlag{Name: "group", Usage: "Group name or id"}
	}
	flags := func(extra ...cli.Flag) []cli.Flag { return append(append(keyClientFlags(), ownerFlag), extra...) }
	name := &cli.StringFlag{Name: "name", Usage: "The template's name (lowercase letters, digits, - and _)"}
	return &cli.Command{
		Name:  "template",
		Usage: "Item templates: a request that repeats, from which an item is started (skopos inbox add --template <name>)",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List the templates (for a workspace: its own and the ones it gets from a group)",
				Flags: flags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := templateOwnerPath(kind, cmd)
					if err != nil {
						return err
					}
					var out struct {
						Templates []pipeline.Template `json:"templates"`
					}
					if err := templateCall(ctx, cmd, http.MethodGet, p, nil, &out); err != nil {
						return err
					}
					if len(out.Templates) == 0 {
						fmt.Println("no templates are held here")
						return nil
					}
					for _, t := range out.Templates {
						extra := ""
						if t.AutoApprovePlan {
							extra += "  plan approved automatically"
						}
						if t.Source != "" {
							extra += "  " + t.Source
						}
						fmt.Printf("%-20s %q%s\n", t.Name, t.Title, extra)
					}
					return nil
				},
			},
			{
				Name:  "show",
				Usage: "Print a template",
				Flags: flags(name),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := templatePath(kind, cmd)
					if err != nil {
						return err
					}
					var t pipeline.Template
					if err := templateCall(ctx, cmd, http.MethodGet, p, nil, &t); err != nil {
						return err
					}
					fmt.Printf("name:   %s\ntitle:  %s\n", t.Name, t.Title)
					if len(t.Tags) > 0 {
						fmt.Printf("tags:   %s\n", strings.Join(t.Tags, ", "))
					}
					if len(t.Needs) > 0 {
						fmt.Printf("needs:  %s\n", strings.Join(t.Needs, ", "))
					}
					if t.Agents != "" {
						fmt.Printf("agents: %s\n", t.Agents)
					}
					fmt.Printf("plan approved automatically: %v\n", t.AutoApprovePlan)
					if t.Source != "" {
						fmt.Printf("from:   %s\n", t.Source)
					}
					fmt.Printf("revision %s by %s, %s\n\n%s\n", t.Rev, t.CreatedBy, t.CreatedAt.Local().Format("2006-01-02 15:04"), t.Brief)
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Save a template: its title, and its brief from a file (or standard input with --file -). {date}, {month}, {year} and {workspace} are filled in when an item is started",
				Flags: flags(name,
					&cli.StringFlag{Name: "title", Usage: "The item's title (e.g. \"Update the dependencies {month}\")"},
					&cli.StringFlag{Name: "file", Usage: "The brief: a file, or - for standard input"},
					&cli.StringSliceFlag{Name: "tag", Usage: "Tag for the items (repeatable)"},
					&cli.StringFlag{Name: "needs", Usage: "Worker labels the items need, comma-separated"},
					&cli.StringFlag{Name: "agents", Usage: "panel or solo (omit for the repository default)"},
					&cli.BoolFlag{Name: "auto-approve", Usage: "Approve the items' plans automatically when the planner asks no question"},
				),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := templatePath(kind, cmd)
					if err != nil {
						return err
					}
					brief := ""
					if file := strings.TrimSpace(cmd.GetString("file")); file != "" {
						var raw []byte
						if file == "-" {
							raw, err = io.ReadAll(io.LimitReader(os.Stdin, pipeline.MaxTemplateBrief+1))
						} else {
							raw, err = os.ReadFile(file)
						}
						if err != nil {
							return fmt.Errorf("reading the brief: %w", err)
						}
						brief = string(raw)
					}
					needs, _ := labels.Arg(cmd.GetString("needs"))
					body := pipeline.TemplateFields{
						Title: cmd.GetString("title"), Brief: brief, Tags: cmd.GetStringSlice("tag"), Needs: needs,
						Agents: cmd.GetString("agents"), AutoApprovePlan: cmd.GetBool("auto-approve"),
					}
					var t pipeline.Template
					if err := templateCall(ctx, cmd, http.MethodPut, p, body, &t); err != nil {
						return err
					}
					fmt.Printf("saved template %s, revision %s\n", t.Name, t.Rev)
					return nil
				},
			},
			{
				Name:  "delete",
				Usage: "Remove a template held here",
				Flags: flags(name),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					p, err := templatePath(kind, cmd)
					if err != nil {
						return err
					}
					if err := templateCall(ctx, cmd, http.MethodDelete, p, nil, nil); err != nil {
						return err
					}
					fmt.Printf("removed template %s\n", strings.TrimSpace(cmd.GetString("name")))
					return nil
				},
			},
		},
	}
}

func templateOwnerPath(kind string, cmd *cli.Command) (string, error) {
	p, err := pipelinePath(kind, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, "/pipeline") + "/templates", nil
}

func templatePath(kind string, cmd *cli.Command) (string, error) {
	p, err := templateOwnerPath(kind, cmd)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(cmd.GetString("name"))
	if name == "" {
		return "", fmt.Errorf("--name is required")
	}
	return p + "/" + url.PathEscape(name), nil
}

func templateCall(ctx context.Context, cmd *cli.Command, method, path string, body, out any) error {
	return apiDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), audit.ViaCLI, "templates", method, path, body, out)
}
