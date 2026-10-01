package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/paularlott/cli"
)

func init() {
	Register(groupCmd())
}

func groupCmd() *cli.Command {
	return &cli.Command{
		Name:  "group",
		Usage: "Manage workspace groups for API keys (requires the root key)",
		Commands: []*cli.Command{
			groupCreateCmd(),
			groupListCmd(),
			groupAddCmd(),
		},
	}
}

func groupCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a workspace group; keys can hold it instead of listing workspaces",
		Flags: append(keyClientFlags(),
			&cli.StringFlag{Name: "name", Usage: "Group name (e.g. work, personal)"},
			&cli.StringFlag{Name: "description", Usage: "Optional description"},
			&cli.StringSliceFlag{Name: "workspace", Usage: "Explicit member workspace ID (repeat or comma-separate; must be registered)"},
			&cli.StringSliceFlag{Name: "pattern", Usage: "Auto-membership pattern (path.Match: * matches one segment, never crosses /)"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			name := strings.TrimSpace(cmd.GetString("name"))
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			members := cmd.GetStringSlice("workspace")
			patterns := cmd.GetStringSlice("pattern")
			if len(members) == 0 && len(patterns) == 0 {
				return fmt.Errorf("a group needs at least one --workspace or --pattern")
			}
			var g apikeys.Group
			body := map[string]any{"name": name}
			if d := strings.TrimSpace(cmd.GetString("description")); d != "" {
				body["description"] = d
			}
			if len(members) > 0 {
				body["members"] = members
			}
			if len(patterns) > 0 {
				body["patterns"] = patterns
			}
			if err := keysCall(ctx, cmd, http.MethodPost, "/api/groups", body, &g); err != nil {
				return err
			}
			fmt.Printf("group created: %s (%s)\n", g.Name, g.ID)
			printGroupDetail(g)
			return nil
		},
	}
}

func groupListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List workspace groups with their members and patterns",
		Flags: keyClientFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			var groups []apikeys.Group
			if err := keysCall(ctx, cmd, http.MethodGet, "/api/groups", nil, &groups); err != nil {
				return err
			}
			if len(groups) == 0 {
				fmt.Println("no workspace groups")
				return nil
			}
			for _, g := range groups {
				fmt.Printf("%s  %-20s %s\n", g.ID, g.Name, groupSummary(g))
			}
			return nil
		},
	}
}

func groupAddCmd() *cli.Command {
	return &cli.Command{
		Name:    "add",
		Usage:   "Add workspace IDs to a group (by group name or id)",
		MaxArgs: 2,
		Flags: append(keyClientFlags(),
			&cli.StringSliceFlag{Name: "pattern", Usage: "Also add patterns (path.Match)"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.GetArgs()
			if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("usage: skopos group add <group> [workspace...]")
			}
			ref := strings.TrimSpace(args[0])
			var workspaces []string
			if len(args) == 2 {
				workspaces = splitAndTrim(args[1])
			}
			patterns := cmd.GetStringSlice("pattern")
			if len(workspaces) == 0 && len(patterns) == 0 {
				return fmt.Errorf("nothing to add: pass workspace ids or --pattern")
			}

			var groups []apikeys.Group
			if err := keysCall(ctx, cmd, http.MethodGet, "/api/groups", nil, &groups); err != nil {
				return err
			}
			var found *apikeys.Group
			for i := range groups {
				if groups[i].ID == ref || groups[i].Name == ref {
					found = &groups[i]
					break
				}
			}
			if found == nil {
				return fmt.Errorf("no group named or identified by %q", ref)
			}
			body := map[string]any{
				"members":  mergeUnique(found.Members, workspaces),
				"patterns": mergeUnique(found.Patterns, patterns),
			}
			var g apikeys.Group
			if err := keysCall(ctx, cmd, http.MethodPatch, "/api/groups/"+found.ID, body, &g); err != nil {
				return err
			}
			fmt.Printf("updated group %s (%s)\n", g.Name, g.ID)
			printGroupDetail(g)
			return nil
		},
	}
}

func keyWhoCanCmd() *cli.Command {
	return &cli.Command{
		Name:    "who-can",
		Usage:   "Which API keys can access a workspace, and how (explicit, group, or pattern)",
		MaxArgs: 1,
		Flags:   keyClientFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.GetArgs()
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("usage: skopos key who-can <workspace-id>")
			}
			return runKeyWhoCan(ctx, cmd, strings.TrimSpace(args[0]))
		},
	}
}

func runKeyWhoCan(ctx context.Context, cmd *cli.Command, ws string) error {
	var reach []apikeys.KeyReach
	if err := keysCall(ctx, cmd, http.MethodGet, "/api/keys/who-can?workspace="+url.QueryEscape(ws), nil, &reach); err != nil {
		return err
	}
	if len(reach) == 0 {
		fmt.Printf("no active key can access %s\n", ws)
		return nil
	}
	for _, r := range reach {
		fmt.Printf("%s  %-20s %-17s via %s\n", r.Key.ID, r.Key.Name, r.Key.Prefix, r.Via)
	}
	return nil
}

func printGroupDetail(g apikeys.Group) {
	for _, m := range g.Members {
		fmt.Printf("  member:  %s\n", m)
	}
	for _, p := range g.Patterns {
		fmt.Printf("  pattern: %s\n", p)
	}
}

func groupSummary(g apikeys.Group) string {
	parts := make([]string, 0, 2)
	if len(g.Members) > 0 {
		parts = append(parts, fmt.Sprintf("members: %s", strings.Join(g.Members, ", ")))
	}
	if len(g.Patterns) > 0 {
		parts = append(parts, fmt.Sprintf("patterns: %s", strings.Join(g.Patterns, ", ")))
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, "  ")
}

func splitAndTrim(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, v := range append(append([]string{}, a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
