package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/schedules"
	"github.com/paularlott/cli"
)

// `skopos schedule …`: an item template started on a rhythm, for a
// workspace or each workspace of a group.

func init() {
	Register(scheduleCmd())
}

func scheduleCmd() *cli.Command {
	owner := []cli.Flag{
		&cli.StringFlag{Name: "workspace", Usage: "The workspace the schedule is for (default: resolved from this checkout's git remote)"},
		&cli.StringFlag{Name: "group", Usage: "A group: one item per member workspace (root only)"},
	}
	name := &cli.StringFlag{Name: "name", Usage: "The schedule's name (lowercase letters, digits, - and _)"}
	flags := func(extra ...cli.Flag) []cli.Flag { return append(keyClientFlags(), extra...) }
	withOwner := func(extra ...cli.Flag) []cli.Flag { return append(append(keyClientFlags(), owner...), extra...) }
	return &cli.Command{
		Name:  "schedule",
		Usage: "Schedules: an item template started on a rhythm (skopos workspace template)",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List the schedules this key may see",
				Flags: flags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					all, err := scheduleList(ctx, cmd)
					if err != nil {
						return err
					}
					if len(all) == 0 {
						fmt.Println("no schedules")
						return nil
					}
					for _, sc := range all {
						state := "next " + sc.Next.Local().Format("2006-01-02 15:04")
						if !sc.Enabled {
							state = "paused"
						}
						fmt.Printf("%-16s %-9s %-36s template %-12s %-16q %s  (%s)\n", sc.Name, sc.OwnerKind, sc.OwnerID, sc.Template, sc.Rhythm, state, sc.ID)
					}
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Hold a schedule, or change the one of this name: --template, --rhythm (five-field cron, or hourly, daily, weekly, monthly) and --timezone",
				Flags: withOwner(name,
					&cli.StringFlag{Name: "template", Usage: "The template the items are started from"},
					&cli.StringFlag{Name: "rhythm", Usage: "When: \"0 6 1 * *\" (minute hour day month weekday), or hourly, daily (06:00), weekly (Monday 06:00), monthly (the 1st, 06:00)"},
					&cli.StringFlag{Name: "timezone", Usage: "The rhythm's timezone, e.g. Europe/London (default UTC)"},
				),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					kind, ownerID, err := scheduleOwner(cmd)
					if err != nil {
						return err
					}
					n := strings.TrimSpace(cmd.GetString("name"))
					if n == "" {
						return fmt.Errorf("--name is required")
					}
					in := schedules.Input{Name: n, Template: cmd.GetString("template"), Rhythm: cmd.GetString("rhythm"), Timezone: cmd.GetString("timezone")}
					existing, err := scheduleFind(ctx, cmd, kind, ownerID, n)
					if err != nil {
						return err
					}
					var sc schedules.Schedule
					if existing == nil {
						in.OwnerKind, in.OwnerID = kind, ownerID
						err = scheduleCall(ctx, cmd, http.MethodPost, "/api/schedules", in, &sc)
					} else {
						err = scheduleCall(ctx, cmd, http.MethodPut, "/api/schedules/"+url.PathEscape(existing.ID), in, &sc)
					}
					if err != nil {
						return err
					}
					fmt.Printf("schedule %s: template %s, %q %s; next %s\n", sc.Name, sc.Template, sc.Rhythm, sc.Timezone, sc.Next.Local().Format(time.RFC1123))
					return nil
				},
			},
			scheduleAction("pause", "Pause a schedule (it keeps its settings)", withOwner(name), func(ctx context.Context, cmd *cli.Command, id string) error {
				off := false
				return scheduleCall(ctx, cmd, http.MethodPut, "/api/schedules/"+url.PathEscape(id), schedules.Input{Enabled: &off}, nil)
			}),
			scheduleAction("resume", "Resume a paused schedule", withOwner(name), func(ctx context.Context, cmd *cli.Command, id string) error {
				on := true
				return scheduleCall(ctx, cmd, http.MethodPut, "/api/schedules/"+url.PathEscape(id), schedules.Input{Enabled: &on}, nil)
			}),
			scheduleAction("delete", "Remove a schedule", withOwner(name), func(ctx context.Context, cmd *cli.Command, id string) error {
				return scheduleCall(ctx, cmd, http.MethodDelete, "/api/schedules/"+url.PathEscape(id), nil, nil)
			}),
			scheduleAction("run", "Fire a schedule now, to try its template out (its rhythm is unchanged)", withOwner(name), func(ctx context.Context, cmd *cli.Command, id string) error {
				var out struct {
					Firings []schedules.Firing `json:"firings"`
				}
				if err := scheduleCall(ctx, cmd, http.MethodPost, "/api/schedules/"+url.PathEscape(id)+"/run", nil, &out); err != nil {
					return err
				}
				for _, f := range out.Firings {
					line := f.WorkspaceID + ": " + f.Outcome
					if f.ItemID != "" {
						line += " item " + f.ItemID
					}
					if f.Reason != "" {
						line += " — " + f.Reason
					}
					fmt.Println(line)
				}
				return nil
			}),
		},
	}
}

// scheduleAction is a command on one schedule, named by owner and name.
func scheduleAction(name, usage string, flags []cli.Flag, do func(ctx context.Context, cmd *cli.Command, id string) error) *cli.Command {
	return &cli.Command{
		Name:  name,
		Usage: usage,
		Flags: flags,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			kind, ownerID, err := scheduleOwner(cmd)
			if err != nil {
				return err
			}
			n := strings.TrimSpace(cmd.GetString("name"))
			if n == "" {
				return fmt.Errorf("--name is required")
			}
			sc, err := scheduleFind(ctx, cmd, kind, ownerID, n)
			if err != nil {
				return err
			}
			if sc == nil {
				return fmt.Errorf("no schedule %s is held for %s %s", n, kind, ownerID)
			}
			if err := do(ctx, cmd, sc.ID); err != nil {
				return err
			}
			if name != "run" {
				fmt.Printf("%s: %s\n", name, n)
			}
			return nil
		},
	}
}

func scheduleOwner(cmd *cli.Command) (kind, id string, err error) {
	if g := strings.TrimSpace(cmd.GetString("group")); g != "" {
		return schedules.KindGroup, g, nil
	}
	ws := workspaceOrDefault(cmd.GetString("workspace"))
	if ws == "" {
		return "", "", fmt.Errorf("--workspace or --group is required (no git remote to resolve a workspace from)")
	}
	return schedules.KindWorkspace, ws, nil
}

func scheduleList(ctx context.Context, cmd *cli.Command) ([]schedules.Schedule, error) {
	var out struct {
		Schedules []schedules.Schedule `json:"schedules"`
	}
	err := scheduleCall(ctx, cmd, http.MethodGet, "/api/schedules", nil, &out)
	return out.Schedules, err
}

// scheduleFind is the owner's schedule of a name (a group named by its name
// or id), nil when there is none.
func scheduleFind(ctx context.Context, cmd *cli.Command, kind, owner, name string) (*schedules.Schedule, error) {
	all, err := scheduleList(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var groupID string
	if kind == schedules.KindGroup {
		var g struct {
			ID string `json:"id"`
		}
		if err := scheduleCall(ctx, cmd, http.MethodGet, "/api/groups/"+url.PathEscape(owner), nil, &g); err == nil {
			groupID = g.ID
		}
	}
	for i, sc := range all {
		if sc.OwnerKind == kind && sc.Name == strings.ToLower(name) && (sc.OwnerID == owner || (groupID != "" && sc.OwnerID == groupID)) {
			return &all[i], nil
		}
	}
	return nil, nil
}

func scheduleCall(ctx context.Context, cmd *cli.Command, method, path string, body, out any) error {
	return apiDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), audit.ViaCLI, "schedules", method, path, body, out)
}
