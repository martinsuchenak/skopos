package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/paularlott/cli"
)

func init() {
	Register(policyCmd())
}

// policyCmd is `skopos policy …`: which runners may work on which
// workspaces (tmp/central-config — design.md, phase 1a).
func policyCmd() *cli.Command {
	owner := []cli.Flag{
		&cli.StringFlag{Name: "workspace", Usage: "A workspace's policy (default for show: this checkout's workspace)"},
		&cli.StringFlag{Name: "group", Usage: "A workspace group's policy (name or id)"},
		&cli.StringFlag{Name: "fleet", Usage: "A fleet policy: ceiling (always applied) or default (for workspaces with no other policy)"},
	}
	flags := func(extra ...cli.Flag) []cli.Flag { return append(append(keyClientFlags(), owner...), extra...) }
	return &cli.Command{
		Name:  "policy",
		Usage: "Which runners may work on which workspaces: by provider, model, runner kind, stage and containment",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "Show a policy; for a workspace, everything that applies to it and where each part comes from",
				Flags: flags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					kind, id, path, err := policyTarget(cmd, true)
					if err != nil {
						return err
					}
					if kind == pipeline.KindWorkspace {
						var eff pipeline.EffectivePolicyView
						if err := policyCall(ctx, cmd, http.MethodGet, path+"/effective", nil, &eff); err != nil {
							return err
						}
						printEffectivePolicy(&eff)
						return nil
					}
					var v pipeline.PolicyView
					if err := policyCall(ctx, cmd, http.MethodGet, path, nil, &v); err != nil {
						return err
					}
					printPolicy(policyLabel(kind, id), &v)
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Replace a policy with the given allow rules (admin permission at that level)",
				Description: `A rule is comma-separated key=value pairs; a runner is allowed when one rule matches:
  provider=anthropic                         who receives the code (anthropic, openai, zai, google, aws, local, …)
  provider=openai,stages=second_opinion      only for these stages (names joined with |)
  model=claude-opus-*                        a glob on the model id
  kind=opencode,provider=local               the CLI (claude, opencode, codex, antigravity, command)
  runner=opencode-*                          a glob on the runner name
  provider=zai,executor=isolated             only where the run is confined (isolated, contained, wrapper:bwrap:*)
  any                                        every runner`,
				Flags: flags(
					&cli.StringSliceFlag{Name: "allow", Usage: "An allow rule (repeat for each rule)"},
					&cli.BoolFlag{Name: "force", Usage: "Save even though it strands configured runners or drops a workspace's embedded index"},
				),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					kind, id, path, err := policyTarget(cmd, false)
					if err != nil {
						return err
					}
					raw := cmd.GetStringSlice("allow")
					if len(raw) == 0 {
						return fmt.Errorf("pass at least one --allow rule (to remove a policy: skopos policy clear)")
					}
					rules := make([]policy.Rule, 0, len(raw))
					for _, r := range raw {
						rule, err := policy.ParseRule(r)
						if err != nil {
							return err
						}
						rules = append(rules, rule)
					}
					var v pipeline.PolicyView
					body := map[string]any{"allow": rules, "force": cmd.GetBool("force"), "via": audit.ViaCLI}
					if err := policyCall(ctx, cmd, http.MethodPut, path, body, &v); err != nil {
						return err
					}
					fmt.Printf("saved %s\n", policyLabel(kind, id))
					printPolicy("", &v)
					return nil
				},
			},
			{
				Name:  "clear",
				Usage: "Remove a policy at one level (admin permission at that level)",
				Flags: flags(&cli.BoolFlag{Name: "force", Usage: "Clear even though what then applies strands configured runners"}),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					kind, id, path, err := policyTarget(cmd, false)
					if err != nil {
						return err
					}
					if cmd.GetBool("force") {
						path += "?force=true"
					}
					var v pipeline.PolicyView
					if err := policyCall(ctx, cmd, http.MethodDelete, path, nil, &v); err != nil {
						return err
					}
					fmt.Printf("cleared %s\n", policyLabel(kind, id))
					for _, w := range v.Warnings {
						fmt.Printf("  warning: %s\n", w)
					}
					return nil
				},
			},
		},
	}
}

// policyTarget resolves which policy the flags name: exactly one of
// --workspace, --group, --fleet (show defaults to this checkout's
// workspace).
func policyTarget(cmd *cli.Command, defaultWorkspace bool) (kind, id, path string, err error) {
	ws, group, fleet := strings.TrimSpace(cmd.GetString("workspace")), strings.TrimSpace(cmd.GetString("group")), strings.TrimSpace(cmd.GetString("fleet"))
	set := 0
	for _, v := range []string{ws, group, fleet} {
		if v != "" {
			set++
		}
	}
	if set > 1 {
		return "", "", "", fmt.Errorf("pass one of --workspace, --group or --fleet")
	}
	switch {
	case fleet != "":
		switch fleet {
		case "ceiling":
			return pipeline.KindFleetCeiling, "", "/api/policy/fleet/ceiling", nil
		case "default":
			return pipeline.KindFleetDefault, "", "/api/policy/fleet/default", nil
		}
		return "", "", "", fmt.Errorf("--fleet is ceiling or default")
	case group != "":
		return pipeline.KindGroup, group, "/api/groups/" + url.PathEscape(group) + "/policy", nil
	}
	if ws == "" && defaultWorkspace {
		ws = workspaceOrDefault("")
	}
	if ws == "" {
		return "", "", "", fmt.Errorf("pass --workspace, --group or --fleet")
	}
	return pipeline.KindWorkspace, ws, "/api/workspaces/" + url.PathEscape(ws) + "/policy", nil
}

func policyCall(ctx context.Context, cmd *cli.Command, method, path string, body, out any) error {
	return apiDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), audit.ViaCLI, "policy", method, path, body, out)
}

func policyLabel(kind, id string) string {
	switch kind {
	case pipeline.KindFleetCeiling:
		return "the fleet ceiling"
	case pipeline.KindFleetDefault:
		return "the fleet default"
	case pipeline.KindGroup:
		return "group " + id
	}
	return "workspace " + id
}

func printPolicy(label string, v *pipeline.PolicyView) {
	if label != "" {
		fmt.Println(label + ":")
	}
	if v.Policy == nil {
		fmt.Println("  no policy at this level")
	} else {
		for _, r := range v.Policy.Allow {
			fmt.Printf("  allow %s\n", r)
		}
		if v.Rev != "" {
			who := ""
			if v.UpdatedBy != "" {
				who = ", set by " + v.UpdatedBy
			}
			fmt.Printf("  revision %s%s\n", v.Rev, who)
		}
	}
	for _, w := range v.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func printEffectivePolicy(v *pipeline.EffectivePolicyView) {
	fmt.Printf("workspace %s:\n", v.WorkspaceID)
	if v.AllowAll {
		fmt.Println("  no policy applies: every runner is allowed")
		return
	}
	fmt.Println("  a runner must be allowed by each of:")
	for _, l := range v.Layers {
		fmt.Printf("  %s (revision %s)\n", l.Source, l.Rev)
		for _, r := range l.Policy.Allow {
			fmt.Printf("    allow %s\n", r)
		}
	}
}
