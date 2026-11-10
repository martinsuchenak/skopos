package cmd

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/integrations"
	"github.com/paularlott/cli"
)

// skopos webhook …: subscriptions that tell another system what happens to
// items (tmp/integrations — design.md §3). Root manages any; an approver
// key manages those for its own workspaces.

func init() {
	Register(webhookCmd())
}

func webhookCmd() *cli.Command {
	return &cli.Command{
		Name:    "webhook",
		MaxArgs: 1,
		Usage:   "Manage webhook subscriptions (signed event deliveries to another system)",
		Commands: []*cli.Command{
			webhookCreateCmd(), webhookListCmd(), webhookShowCmd(), webhookUpdateCmd(),
			webhookToggleCmd("enable", true), webhookToggleCmd("disable", false),
			webhookRotateCmd(), webhookTestCmd(), webhookDeliveriesCmd(), webhookRedeliverCmd(), webhookDeleteCmd(),
		},
	}
}

func webhookFlags(extra ...cli.Flag) []cli.Flag {
	return append(keyClientFlags(), extra...)
}

func webhookScopeFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "name", Usage: "Name shown in listings"},
		&cli.StringFlag{Name: "url", Usage: "Where to POST events (https)"},
		&cli.StringSliceFlag{Name: "workspace", Usage: "Workspace whose events it gets (repeat or comma-separate)"},
		&cli.StringSliceFlag{Name: "group", Usage: "Workspace group whose events it gets (root key only)"},
		&cli.StringSliceFlag{Name: "type", Usage: "Event types (default: all but run.started and run.finished): " + strings.Join(integrations.Types, ", ")},
	}
}

func webhookCall(ctx context.Context, cmd *cli.Command, method, path string, body, out any) error {
	return apiDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), "cli", "webhooks", method, path, body, out)
}

func oneArg(cmd *cli.Command, usage string) (string, error) {
	args := cmd.GetArgs()
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("usage: %s", usage)
	}
	return strings.TrimSpace(args[0]), nil
}

func printWebhook(w integrations.Webhook) {
	state := "enabled"
	if !w.Enabled {
		state = "disabled"
		if w.DisabledReason != "" {
			state += ": " + w.DisabledReason
		}
	}
	types := "all but run.*"
	if len(w.Types) > 0 {
		types = strings.Join(w.Types, ", ")
	}
	scope := strings.Join(w.Workspaces, ", ")
	if len(w.Groups) > 0 {
		scope = strings.TrimPrefix(scope+", groups "+strings.Join(w.Groups, ", "), ", ")
	}
	fmt.Printf("%s  %s  %s\n  url: %s\n  scope: %s\n  types: %s\n", w.ID, w.Name, state, w.URL, scope, types)
	if w.FailingSince != nil && w.Enabled {
		fmt.Printf("  failing since %s\n", w.FailingSince.Local().Format("2006-01-02 15:04"))
	}
}

func printSecret(s integrations.WithSecret, what string) {
	printWebhook(s.Webhook)
	fmt.Printf("\n%s — store it now, it is shown only once:\n  %s\n", what, s.Secret)
}

func webhookCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a subscription; the signing secret is printed once",
		Flags: webhookFlags(webhookScopeFlags()...),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			body := map[string]any{"name": cmd.GetString("name"), "url": cmd.GetString("url"),
				"workspaces": cmd.GetStringSlice("workspace"), "groups": cmd.GetStringSlice("group"), "types": cmd.GetStringSlice("type")}
			var out integrations.WithSecret
			if err := webhookCall(ctx, cmd, http.MethodPost, "/api/webhooks", body, &out); err != nil {
				return err
			}
			printSecret(out, "Signing secret (Standard Webhooks)")
			return nil
		},
	}
}

func webhookListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List the subscriptions you may manage",
		Flags: webhookFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			var out []integrations.Webhook
			if err := webhookCall(ctx, cmd, http.MethodGet, "/api/webhooks", nil, &out); err != nil {
				return err
			}
			if len(out) == 0 {
				fmt.Println("no webhooks")
			}
			for _, w := range out {
				printWebhook(w)
			}
			return nil
		},
	}
}

func webhookShowCmd() *cli.Command {
	return &cli.Command{
		Name:    "show",
		MaxArgs: 1,
		Usage:   "Show one subscription: skopos webhook show <id>",
		Flags:   webhookFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook show <id>")
			if err != nil {
				return err
			}
			var out integrations.Webhook
			if err := webhookCall(ctx, cmd, http.MethodGet, "/api/webhooks/"+id, nil, &out); err != nil {
				return err
			}
			printWebhook(out)
			return nil
		},
	}
}

func webhookUpdateCmd() *cli.Command {
	return &cli.Command{
		Name:    "update",
		MaxArgs: 1,
		Usage:   "Change a subscription: skopos webhook update <id> [--name] [--url] [--workspace …] [--group …] [--type …] [--all-types]",
		Flags:   webhookFlags(append(webhookScopeFlags(), &cli.BoolFlag{Name: "all-types", Usage: "Back to every type but run.*"})...),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook update <id> [flags]")
			if err != nil {
				return err
			}
			body := map[string]any{}
			if v := cmd.GetString("name"); v != "" {
				body["name"] = v
			}
			if v := cmd.GetString("url"); v != "" {
				body["url"] = v
			}
			if v := cmd.GetStringSlice("workspace"); len(v) > 0 {
				body["workspaces"] = v
			}
			if v := cmd.GetStringSlice("group"); len(v) > 0 {
				body["groups"] = v
			}
			if v := cmd.GetStringSlice("type"); len(v) > 0 {
				body["types"] = v
			} else if cmd.GetBool("all-types") {
				body["types"] = []string{}
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: pass --name, --url, --workspace, --group, --type or --all-types")
			}
			var out integrations.Webhook
			if err := webhookCall(ctx, cmd, http.MethodPatch, "/api/webhooks/"+id, body, &out); err != nil {
				return err
			}
			printWebhook(out)
			return nil
		},
	}
}

func webhookToggleCmd(name string, enabled bool) *cli.Command {
	usage := "Disable a subscription: deliveries stop, events wait"
	if enabled {
		usage = "Enable a subscription: delivers what waited since it was disabled"
	}
	return &cli.Command{
		Name:    name,
		MaxArgs: 1,
		Usage:   usage,
		Flags:   webhookFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook "+name+" <id>")
			if err != nil {
				return err
			}
			var out integrations.Webhook
			if err := webhookCall(ctx, cmd, http.MethodPatch, "/api/webhooks/"+id, map[string]any{"enabled": enabled}, &out); err != nil {
				return err
			}
			printWebhook(out)
			return nil
		},
	}
}

func webhookRotateCmd() *cli.Command {
	return &cli.Command{
		Name:    "rotate",
		MaxArgs: 1,
		Usage:   "Replace the signing secret (the old one also signs for 24 hours)",
		Flags:   webhookFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook rotate <id>")
			if err != nil {
				return err
			}
			var out integrations.WithSecret
			if err := webhookCall(ctx, cmd, http.MethodPost, "/api/webhooks/"+id+"/rotate", nil, &out); err != nil {
				return err
			}
			printSecret(out, "New signing secret (the old one also signs for 24 hours)")
			return nil
		},
	}
}

func webhookTestCmd() *cli.Command {
	return &cli.Command{
		Name:    "test",
		MaxArgs: 1,
		Usage:   "Send a webhook.test event to the subscription",
		Flags:   webhookFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook test <id>")
			if err != nil {
				return err
			}
			var out integrations.Delivery
			if err := webhookCall(ctx, cmd, http.MethodPost, "/api/webhooks/"+id+"/test", nil, &out); err != nil {
				return err
			}
			fmt.Printf("queued delivery %s (event %s); see: skopos webhook deliveries %s\n", out.ID, out.EventID, id)
			return nil
		},
	}
}

func webhookDeliveriesCmd() *cli.Command {
	return &cli.Command{
		Name:    "deliveries",
		MaxArgs: 1,
		Usage:   "Show a subscription's deliveries, newest first",
		Flags: webhookFlags(
			&cli.StringFlag{Name: "state", Usage: "pending, delivered or failed"},
			&cli.IntFlag{Name: "limit", DefaultValue: 20, Usage: "How many (at most 200)"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook deliveries <id>")
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/webhooks/%s/deliveries?limit=%d", id, cmd.GetInt("limit"))
			if st := cmd.GetString("state"); st != "" {
				path += "&state=" + st
			}
			var out []integrations.Delivery
			if err := webhookCall(ctx, cmd, http.MethodGet, path, nil, &out); err != nil {
				return err
			}
			if len(out) == 0 {
				fmt.Println("no deliveries")
			}
			for _, d := range out {
				line := fmt.Sprintf("%s  %-20s %-9s attempts %d", d.ID, d.EventType, d.State, d.Attempts)
				if d.LastCode != 0 || d.LastError != "" {
					line += fmt.Sprintf("  last: %d %s", d.LastCode, d.LastError)
				}
				if d.NextAt != nil {
					line += "  next " + d.NextAt.Local().Format("15:04:05")
				}
				fmt.Println(line)
			}
			return nil
		},
	}
}

func webhookRedeliverCmd() *cli.Command {
	return &cli.Command{
		Name:    "redeliver",
		MaxArgs: 1,
		Usage:   "Deliver again: --delivery <id>, or --since <time|duration> for every failed one since",
		Flags: webhookFlags(
			&cli.StringFlag{Name: "delivery", Usage: "Delivery id"},
			&cli.StringFlag{Name: "since", Usage: "RFC 3339 time, or a duration back from now (e.g. 24h)"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook redeliver <id> --delivery <id> | --since <when>")
			if err != nil {
				return err
			}
			body := map[string]any{}
			if d := cmd.GetString("delivery"); d != "" {
				body["delivery_id"] = d
			}
			if v := strings.TrimSpace(cmd.GetString("since")); v != "" {
				t, err := time.Parse(time.RFC3339, v)
				if err != nil {
					dur, derr := time.ParseDuration(v)
					if derr != nil {
						return fmt.Errorf("--since: an RFC 3339 time or a duration such as 24h")
					}
					t = time.Now().Add(-dur)
				}
				body["since"] = t.UTC().Format(time.RFC3339)
			}
			var out struct {
				Redelivered int64 `json:"redelivered"`
			}
			if err := webhookCall(ctx, cmd, http.MethodPost, "/api/webhooks/"+id+"/redeliver", body, &out); err != nil {
				return err
			}
			fmt.Printf("%d deliveries queued again\n", out.Redelivered)
			return nil
		},
	}
}

func webhookDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:    "delete",
		MaxArgs: 1,
		Usage:   "Delete a subscription and its deliveries",
		Flags:   webhookFlags(),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id, err := oneArg(cmd, "skopos webhook delete <id>")
			if err != nil {
				return err
			}
			if err := webhookCall(ctx, cmd, http.MethodDelete, "/api/webhooks/"+id, nil, nil); err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", id)
			return nil
		},
	}
}
