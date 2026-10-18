package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/paularlott/cli"
)

// pipelineCmd is `skopos workspace pipeline …` / `skopos group pipeline …`:
// which agents run each stage (tmp/workspace-pipeline-settings — design.md).
func pipelineCmd(kind string) *cli.Command {
	ownerFlag := &cli.StringFlag{Name: "workspace", Usage: "Workspace ID (default: resolved from this checkout's git remote)"}
	if kind == pipeline.KindGroup {
		ownerFlag = &cli.StringFlag{Name: "group", Usage: "Group name or id"}
	}
	flags := func(extra ...cli.Flag) []cli.Flag { return append(append(keyClientFlags(), ownerFlag), extra...) }
	return &cli.Command{
		Name:  "pipeline",
		Usage: "Which agents run each stage (plan, compare, synthesize, implement, review, qa)",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "Show the settings (for a workspace: the effective ones, with where each comes from)",
				Flags: flags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					v, err := pipelineGet(ctx, cmd, kind)
					if err != nil {
						return err
					}
					printPipeline(v)
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Set one stage's agents (and decision rule), or the agents default",
				Flags: flags(
					&cli.StringFlag{Name: "stage", Usage: "plan, compare, synthesize, implement, review or qa"},
					&cli.StringFlag{Name: "runners", Usage: "[focus@]runner list, e.g. security@claude,tests@opencode-glm (several only on plan, review, qa)"},
					&cli.StringFlag{Name: "policy", Usage: "review/qa with several agents: any-blocks (default), majority or all-pass"},
					&cli.BoolFlag{Name: "early-exit", Usage: "review/qa: decide as soon as the outcome is certain"},
					&cli.StringFlag{Name: "timeout", Usage: "plan/review/qa with several agents: decide at this deadline (e.g. 45m)"},
					&cli.StringFlag{Name: "agents-default", Usage: "What items without their own switch get: panel, solo or default"},
				),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					v, err := pipelineGet(ctx, cmd, kind)
					if err != nil {
						return err
					}
					own := v.Own
					if own.Stages == nil {
						own.Stages = map[string]pipeline.StageSetting{}
					}
					stage := strings.ToLower(strings.TrimSpace(cmd.GetString("stage")))
					changed := false
					if stage != "" {
						ms, err := pipeline.ParseMembers(cmd.GetString("runners"))
						if err != nil {
							return fmt.Errorf("--runners: %w", err)
						}
						st := pipeline.StageSetting{Members: ms, Policy: cmd.GetString("policy"), EarlyExit: cmd.GetBool("early-exit")}
						if t := strings.TrimSpace(cmd.GetString("timeout")); t != "" {
							d, err := time.ParseDuration(t)
							if err != nil {
								return fmt.Errorf("--timeout: %w", err)
							}
							st.TimeoutMS = d.Milliseconds()
						}
						own.Stages[stage] = st
						changed = true
					}
					if a := strings.TrimSpace(cmd.GetString("agents-default")); a != "" {
						own.AgentsDefault = a
						changed = true
					}
					if !changed {
						return fmt.Errorf("nothing to set: pass --stage with --runners, and/or --agents-default")
					}
					return pipelinePut(ctx, cmd, kind, v.OwnerID, own)
				},
			},
			{
				Name:  "clear",
				Usage: "Clear one stage at this level (a workspace then inherits its group's), or everything",
				Flags: flags(
					&cli.StringFlag{Name: "stage", Usage: "The stage to clear"},
					&cli.BoolFlag{Name: "all", Usage: "Clear every stage and the agents default"},
				),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					v, err := pipelineGet(ctx, cmd, kind)
					if err != nil {
						return err
					}
					own := v.Own
					switch stage := strings.ToLower(strings.TrimSpace(cmd.GetString("stage"))); {
					case cmd.GetBool("all"):
						own = pipeline.Settings{Stages: map[string]pipeline.StageSetting{}}
					case stage != "":
						if _, ok := own.Stages[stage]; !ok {
							return fmt.Errorf("%s is not set at this level", stage)
						}
						delete(own.Stages, stage)
					default:
						return fmt.Errorf("pass --stage <stage> or --all")
					}
					return pipelinePut(ctx, cmd, kind, v.OwnerID, own)
				},
			},
		},
	}
}

func pipelinePath(kind string, cmd *cli.Command) (string, error) {
	if kind == pipeline.KindGroup {
		g := strings.TrimSpace(cmd.GetString("group"))
		if g == "" {
			return "", fmt.Errorf("--group is required")
		}
		return "/api/groups/" + url.PathEscape(g) + "/pipeline", nil
	}
	ws := workspaceOrDefault(cmd.GetString("workspace"))
	if ws == "" {
		return "", fmt.Errorf("--workspace is required (no git remote to resolve it from)")
	}
	return "/api/workspaces/" + url.PathEscape(ws) + "/pipeline", nil
}

func pipelineGet(ctx context.Context, cmd *cli.Command, kind string) (*pipeline.View, error) {
	p, err := pipelinePath(kind, cmd)
	if err != nil {
		return nil, err
	}
	var v pipeline.View
	if err := keysCall(ctx, cmd, http.MethodGet, p, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func pipelinePut(ctx context.Context, cmd *cli.Command, kind, ownerID string, own pipeline.Settings) error {
	p, err := pipelinePath(kind, cmd)
	if err != nil {
		return err
	}
	body := map[string]any{"stages": own.Stages, "agents_default": own.AgentsDefault, "via": audit.ViaCLI}
	var v pipeline.View
	if err := keysCall(ctx, cmd, http.MethodPut, p, body, &v); err != nil {
		return err
	}
	fmt.Printf("saved %s %s\n", kind, ownerID)
	printPipeline(&v)
	return nil
}

// printPipeline: one line per stage. A workspace shows its effective
// routing with the source; a group shows its own settings.
func printPipeline(v *pipeline.View) {
	for _, stage := range pipeline.Stages {
		var st pipeline.StageSetting
		source := ""
		if v.OwnerKind == pipeline.KindWorkspace {
			e, ok := v.Effective[stage]
			if !ok {
				fmt.Printf("%-11s —\n", stage)
				continue
			}
			st, source = e.StageSetting, e.Source
		} else {
			own, ok := v.Own.Stages[stage]
			if !ok {
				fmt.Printf("%-11s —\n", stage)
				continue
			}
			st = own
		}
		line := fmt.Sprintf("%-11s %s", stage, pipeline.FormatMembers(st.Members))
		var extra []string
		if st.Policy != "" {
			extra = append(extra, st.Policy)
		}
		if st.EarlyExit {
			extra = append(extra, "early exit")
		}
		if st.TimeoutMS > 0 {
			extra = append(extra, "timeout "+(time.Duration(st.TimeoutMS)*time.Millisecond).String())
		}
		if len(extra) > 0 {
			line += "  (" + strings.Join(extra, ", ") + ")"
		}
		if source != "" && source != pipeline.KindWorkspace {
			line += "  ← " + strings.Replace(source, "group:", "group ", 1)
		}
		fmt.Println(line)
	}
	agents, source := v.Own.AgentsDefault, ""
	if v.OwnerKind == pipeline.KindWorkspace {
		agents, source = v.EffectiveAgentsDefault, v.AgentsDefaultSource
	}
	if agents == "" {
		agents = "panel (default)"
	}
	line := "agents default: " + agents
	if source != "" && source != pipeline.KindWorkspace {
		line += "  ← " + strings.Replace(source, "group:", "group ", 1)
	}
	fmt.Println(line)
	for _, c := range v.Conflicts {
		fmt.Println("conflict: " + c)
	}
	if len(v.UnknownRunners) > 0 {
		fmt.Printf("warning: no worker serving this workspace offers %s yet; those runs will wait\n", strings.Join(v.UnknownRunners, ", "))
	}
}
