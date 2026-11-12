package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/fleetfile"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
	"github.com/paularlott/cli"
)

func init() {
	Register(serverConfigCmd())
	Register(runnerDefCmd())
	Register(wrapperDefCmd())
}

func configCall(ctx context.Context, cmd *cli.Command, via, method, path string, body, out any) error {
	return apiDo(ctx, strings.TrimRight(cmd.GetString("server-url"), "/"), cmd.GetString("api-key"), via, "config", method, path, body, out)
}

func configPath(kind, id string) string {
	if kind == pipeline.KindGroup {
		return "/api/groups/" + url.PathEscape(id) + "/config"
	}
	return "/api/workspaces/" + url.PathEscape(id) + "/config"
}

// configCmd is `skopos workspace config …` / `skopos group config …`: the
// part of a workspace's configuration every worker shares, held on the
// server (tmp/central-config — design.md, phase 2).
func configCmd(kind string) *cli.Command {
	target := &cli.StringFlag{Name: "workspace", Usage: "Workspace ID (default: this checkout's workspace)"}
	what := "a workspace's"
	if kind == pipeline.KindGroup {
		target = &cli.StringFlag{Name: "group", Usage: "Workspace group (name or id)"}
		what = "a group's"
	}
	flags := func(extra ...cli.Flag) []cli.Flag { return append(append(keyClientFlags(), target), extra...) }
	resolve := func(cmd *cli.Command) (string, error) {
		if kind == pipeline.KindGroup {
			if g := strings.TrimSpace(cmd.GetString("group")); g != "" {
				return g, nil
			}
			return "", fmt.Errorf("pass --group")
		}
		if ws := workspaceOrDefault(strings.TrimSpace(cmd.GetString("workspace"))); ws != "" {
			return ws, nil
		}
		return "", fmt.Errorf("pass --workspace (not inside a git checkout)")
	}
	commands := []*cli.Command{
		{
			Name:  "show",
			Usage: "Show " + what + " config on the server; for a workspace, everything that applies and where each field comes from",
			Flags: flags(&cli.BoolFlag{Name: "toml", Usage: "Print only the stored config as TOML (the form `set --file` takes)"}),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				id, err := resolve(cmd)
				if err != nil {
					return err
				}
				var v wsconfig.View
				if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, configPath(kind, id), nil, &v); err != nil {
					return err
				}
				if cmd.GetBool("toml") {
					if v.Config == nil {
						return nil
					}
					return toml.NewEncoder(os.Stdout).Encode(v.Config)
				}
				printConfigView(kind, id, &v)
				return nil
			},
		},
		{
			Name:  "set",
			Usage: "Save " + what + " config (admin permission). A workspace with a config is managed: every shared field comes from the server",
			Description: `Fields: default_branch, branch_prefix, branch_template, checkout, protected_branches,
setup, checks, check_fix_rounds, preflight, preflight_interval, setup_timeout,
check_timeout, quiet_timeout, plan_timeout, impl_timeout, push.kind, push.remote_url,
push.pull_request, push.steps, mcp_deny, mcp_servers, mcp, stages.<stage>.

--file replaces the whole config with a TOML file's. --set and --unset change
single fields of the stored one:
  --set default_branch=develop
  --set 'checks=["go vet ./...", "go test ./..."]'
  --set push.kind=github --set push.remote_url=https://github.com/you/repo.git
  --unset quiet_timeout

setup, checks and preflight run on every worker serving the workspace that has
worker.server_commands on. Their values are recorded in full: keep secrets out
of them (use ${VAR} from the worker's environment).

stages.plan, stages.implement, stages.review and stages.qa say what a stage's
agent is given — tools, env, settings — and which wrapper its runs start
behind, each as one block:
  --set 'stages.implement={"tools":["Bash(npm run:*)"],"env":{"NODE_ENV":"test"}}'
  --set 'stages.review={"wrap":[{"kind":"opencode","wrapper":"opencode-bwrap"}]}'
A workspace's block for a stage replaces its group's. Workers take the blocks
only with worker.server_commands on, and give a stage what its block says only
where something contains the run. ${NAME} in an env value is filled in from
the worker's [worker.vars].

mcp names the MCP servers the runs get (skopos mcp-server set), and
mcp_servers which of their tools a run may call:
  --set 'mcp=["router"]' --set 'mcp_servers=["mcp__router"]'
Once mcp is set — to [] too — the runs get exactly those servers and nothing
from a worker's own mcp_config.`,
			Flags: flags(
				&cli.StringFlag{Name: "file", Usage: "A TOML file with the config's fields (replaces the stored config)"},
				&cli.StringSliceFlag{Name: "set", Usage: "field=value (a JSON value, or plain text); repeat for each field"},
				&cli.StringSliceFlag{Name: "unset", Usage: "A field to stop setting at this level; repeat for each field"},
				&cli.StringFlag{Name: "note", Usage: "A note kept with the revision"},
			),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				id, err := resolve(cmd)
				if err != nil {
					return err
				}
				var next wsconfig.Config
				body := map[string]any{"note": cmd.GetString("note")}
				if file := cmd.GetString("file"); file != "" {
					md, err := toml.DecodeFile(file, &next)
					if err != nil {
						return fmt.Errorf("reading %s: %w", file, err)
					}
					// A misspelt field would be a field left out.
					if extra := md.Undecoded(); len(extra) > 0 {
						keys := make([]string, len(extra))
						for i, k := range extra {
							keys[i] = k.String()
						}
						return fmt.Errorf("%s: unknown fields: %s", file, strings.Join(keys, ", "))
					}
				} else {
					var cur wsconfig.View
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, configPath(kind, id), nil, &cur); err != nil {
						return err
					}
					if cur.Config != nil {
						next = *cur.Config
					}
					// Changing single fields of what was just read: refused
					// when someone else saved in between.
					body["if_rev"] = cur.Rev
				}
				if next, err = editConfig(next, cmd.GetStringSlice("set"), cmd.GetStringSlice("unset")); err != nil {
					return err
				}
				var v wsconfig.View
				body["config"] = next
				if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodPut, configPath(kind, id), body, &v); err != nil {
					return err
				}
				if len(v.Changed) == 0 {
					fmt.Println("nothing changed")
				} else {
					fmt.Printf("saved (revision %s): %s\n", v.Rev, strings.Join(v.Changed, ", "))
				}
				printConfigView(kind, id, &v)
				return nil
			},
		},
		{
			Name:  "clear",
			Usage: "Remove " + what + " config (admin permission). A workspace then goes back to each worker's own file",
			Flags: flags(),
			Run: func(ctx context.Context, cmd *cli.Command) error {
				id, err := resolve(cmd)
				if err != nil {
					return err
				}
				if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodDelete, configPath(kind, id), nil, nil); err != nil {
					return err
				}
				fmt.Println("cleared")
				return nil
			},
		},
	}
	if kind == pipeline.KindWorkspace {
		commands = append(commands, configImportCmd())
	}
	return &cli.Command{
		Name:     "config",
		Usage:    "The configuration every worker serving the workspace shares, held on the server",
		Commands: commands,
	}
}

// editConfig applies --set field=value and --unset field to a config.
func editConfig(c wsconfig.Config, set, unset []string) (wsconfig.Config, error) {
	if len(set) == 0 && len(unset) == 0 {
		return c, nil
	}
	fields := c.Fields()
	known := wsconfig.Defaults().Fields()
	// A stage's block has no default: stages.implement='{"tools":[…]}'.
	for _, f := range wsconfig.StageFields() {
		known[f] = json.RawMessage(`{}`)
	}
	// The MCP servers the runs get: not set by default (each worker's own
	// mcp_config then applies).
	known["mcp"] = json.RawMessage(`[]`)
	for _, kv := range set {
		name, value, ok := strings.Cut(kv, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return c, fmt.Errorf("--set %q: use field=value", kv)
		}
		raw := json.RawMessage(strings.TrimSpace(value))
		// Plain text, and a bare number or word for a field that is text (a
		// branch named 2024, a timeout of 0): the value as typed.
		if def, known := known[name]; !json.Valid(raw) || (known && len(def) > 0 && def[0] == '"' && raw[0] != '"' && string(raw) != "null") {
			raw, _ = json.Marshal(strings.TrimSpace(value))
		}
		// A timeout of "0" is no timeout.
		if string(raw) == `"0"` && strings.HasSuffix(name, "_timeout") {
			raw = json.RawMessage(`"0s"`)
		}
		fields[name] = raw
	}
	for _, name := range unset {
		delete(fields, strings.TrimSpace(name))
	}
	// Unknown names and wrong types are refused here, not dropped.
	for name, raw := range fields {
		// Inside a stage's block too: a misspelt key would otherwise save
		// an empty block, or a wrap rule that matches every runner.
		if strings.HasPrefix(name, "stages.") {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			var block wsconfig.StageCaps
			if err := dec.Decode(&block); err != nil {
				return c, fmt.Errorf("%s: %s (a block holds tools, env, settings and wrap; a wrap rule kind, provider, model, runner and wrapper)", name, strings.TrimPrefix(err.Error(), "json: "))
			}
		}
	}
	for name := range fields {
		if _, ok := known[name]; !ok {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			return c, fmt.Errorf("%q is not a config field (%s)", name, strings.Join(names, ", "))
		}
	}
	out, err := wsconfig.FromFields(fields)
	if err != nil {
		return c, fmt.Errorf("a value has the wrong type: %w", err)
	}
	return out, nil
}

func printConfigView(kind, id string, v *wsconfig.View) {
	label := "workspace " + id
	if kind == pipeline.KindGroup {
		label = "group " + id
	}
	fmt.Println(label + ":")
	if v.Config == nil {
		if kind == pipeline.KindGroup {
			fmt.Println("  no config at this level")
		} else {
			fmt.Println("  not managed: each worker uses its own file")
		}
		return
	}
	who := ""
	if v.UpdatedBy != "" {
		who = ", saved by " + v.UpdatedBy
	}
	fmt.Printf("  revision %s%s\n", v.Rev, who)
	if kind == pipeline.KindGroup {
		printFields("  ", v.Config.Fields(), nil)
		return
	}
	eff := v.Effective
	if eff == nil {
		printFields("  ", v.Config.Fields(), nil)
		return
	}
	fmt.Printf("  managed by the server; runs record config revision %s\n", eff.Rev)
	printFields("  ", eff.Config.Fields(), eff.Sources)
	for _, c := range eff.Conflicts {
		fmt.Printf("  conflict: %s\n", c)
	}
	fmt.Println("  every other field has its default")
}

func printFields(indent string, fields map[string]json.RawMessage, sources map[string]string) {
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		from := ""
		if s := sources[n]; s != "" && s != "workspace" {
			from = "   (" + s + ")"
		}
		fmt.Printf("%s%s = %s%s\n", indent, n, fields[n], from)
	}
}

// serverConfigCmd is `skopos config …`: everything the server holds about
// how runs are configured, as one file (phase 2c).
func serverConfigCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "The server's run configuration as one file: export it, review a change, apply it",
		Commands: []*cli.Command{
			{
				Name:  "export",
				Usage: "Write runner definitions, groups, policies, pipelines and workspace config as one TOML file (admin over every workspace)",
				Flags: append(keyClientFlags(), &cli.StringFlag{Name: "out", Usage: "File to write (default: standard output)"}),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					var doc fleetfile.Document
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, "/api/config/export", nil, &doc); err != nil {
						return err
					}
					var buf bytes.Buffer
					buf.WriteString("# skopos run configuration — `skopos config apply <this file>` makes the server match it.\n" +
						"# A workspace or group listed here gets exactly what this file gives it; one\n" +
						"# not listed is left as it is. Review a change with `skopos config apply --plan`.\n\n")
					if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
						return err
					}
					if out := cmd.GetString("out"); out != "" {
						return os.WriteFile(out, buf.Bytes(), 0o644)
					}
					_, err := os.Stdout.Write(buf.Bytes())
					return err
				},
			},
			{
				Name:  "apply",
				Usage: "Make the server match a configuration file (admin permission for each change); --plan only lists the changes",
				Flags: append(keyClientFlags(),
					&cli.BoolFlag{Name: "plan", Usage: "List what would change and change nothing"},
					&cli.BoolFlag{Name: "force", Usage: "Save policies even though they strand configured runners, and drop wrappers that stage blocks outside the file still name"},
					&cli.StringFlag{Name: "commit", Usage: "The git commit the file comes from (default: read from the file's checkout)"},
				),
				Arguments: []cli.Argument{&cli.StringArg{Name: "file", Usage: "The configuration file", Required: true}},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					path := cmd.GetStringArg("file")
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					var doc fleetfile.Document
					md, err := toml.Decode(string(raw), &doc)
					if err != nil {
						return fmt.Errorf("reading %s: %w", path, err)
					}
					// A key the file has and the server would not read is a
					// mistake to fix, not something to apply around.
					if extra := md.Undecoded(); len(extra) > 0 {
						keys := make([]string, len(extra))
						for i, k := range extra {
							keys[i] = k.String()
						}
						return fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
					}
					sum := sha256.Sum256(raw)
					commit := strings.TrimSpace(cmd.GetString("commit"))
					if commit == "" {
						commit = fileCommit(path)
					}
					in := fleetfile.Input{Document: doc, FileHash: hex.EncodeToString(sum[:]), Commit: commit, Force: cmd.GetBool("force"), Plan: cmd.GetBool("plan")}
					var res fleetfile.Result
					if err := configCall(ctx, cmd, audit.ViaApply, http.MethodPost, "/api/config/apply", in, &res); err != nil {
						return err
					}
					printApplyResult(&res)
					return nil
				},
			},
		},
	}
}

// fileCommit is the commit a file comes from: HEAD of its checkout, marked
// when the file differs from it. "" outside a git checkout.
func fileCommit(path string) string {
	dir, base := filepath.Dir(path), filepath.Base(path)
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	commit := strings.TrimSpace(string(out))
	if status, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--", base).Output(); err != nil || len(bytes.TrimSpace(status)) > 0 {
		commit += "+modified"
	}
	return commit
}

func printApplyResult(res *fleetfile.Result) {
	verb := "would"
	if res.Applied {
		verb = "did"
	}
	if len(res.Changes) == 0 {
		fmt.Println("no changes: the server matches the file")
	}
	for _, c := range res.Changes {
		line := fmt.Sprintf("%s %s %s %s", verb, c.Action, c.Kind, c.Owner)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		if c.Revision != "" {
			line += " (revision " + c.Revision + ")"
		}
		fmt.Println(line)
		for _, w := range c.Warnings {
			fmt.Println("  warning: " + w)
		}
	}
	if len(res.Unlisted) > 0 {
		fmt.Println("not in the file, left as they are: " + strings.Join(res.Unlisted, ", "))
	}
	if res.Applied {
		fmt.Printf("applied (file sha256 %s, commit %s)\n", res.FileHash, orDash(res.Commit))
	}
}

// runnerDefCmd is `skopos runner …`: runner definitions on the server.
func runnerDefCmd() *cli.Command {
	return &cli.Command{
		Name:  "runner",
		Usage: "Runner definitions on the server: one name, one CLI, model, effort and provider for every worker",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List the runner definitions",
				Flags: keyClientFlags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					var out struct {
						Runners []wsconfig.RunnerView `json:"runners"`
					}
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, "/api/fleet/runners", nil, &out); err != nil {
						return err
					}
					if len(out.Runners) == 0 {
						fmt.Println("no runner definitions: each worker's own file names its runners")
						return nil
					}
					for _, r := range out.Runners {
						fmt.Printf("%-24s kind=%s%s%s%s\n", r.Name, r.Kind, optField("model", r.Model), optField("effort", r.Effort), optField("provider", r.Provider))
					}
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Define or redefine a runner (admin over every workspace)",
				Description: `A worker offers a defined runner when one of its accounts is a login for the
runner's kind:

  [[worker.accounts]]
  name = "claude-1"
  kind = "claude"

The provider is what a runner policy matches. Declare it: a save that names a
defined runner whose provider the workspace's policy never allows is refused.`,
				Flags: append(keyClientFlags(),
					&cli.StringFlag{Name: "kind", Usage: "The CLI: claude, opencode, codex or antigravity"},
					&cli.StringFlag{Name: "model", Usage: "The model (opencode: <provider>/<model>)"},
					&cli.StringFlag{Name: "effort", Usage: "Reasoning effort, as the CLI takes it"},
					&cli.StringFlag{Name: "provider", Usage: "Who receives the code: anthropic, openai, zai, google, local, …"},
				),
				Arguments: []cli.Argument{&cli.StringArg{Name: "name", Usage: "Runner name", Required: true}},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.GetStringArg("name")
					def := wsconfig.RunnerDef{Kind: cmd.GetString("kind"), Model: cmd.GetString("model"), Effort: cmd.GetString("effort"), Provider: cmd.GetString("provider")}
					var v wsconfig.RunnerView
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodPut, "/api/fleet/runners/"+url.PathEscape(name), def, &v); err != nil {
						return err
					}
					fmt.Printf("saved %s (revision %s): kind=%s%s%s%s\n", v.Name, v.Rev, v.Kind, optField("model", v.Model), optField("effort", v.Effort), optField("provider", v.Provider))
					return nil
				},
			},
			{
				Name:      "delete",
				Usage:     "Remove a runner definition (admin over every workspace): workers then offer the name only from their own files",
				Flags:     keyClientFlags(),
				Arguments: []cli.Argument{&cli.StringArg{Name: "name", Usage: "Runner name", Required: true}},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.GetStringArg("name")
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodDelete, "/api/fleet/runners/"+url.PathEscape(name), nil, nil); err != nil {
						return err
					}
					fmt.Println("deleted " + name)
					return nil
				},
			},
		},
	}
}

func optField(name, value string) string {
	if value == "" {
		return ""
	}
	return "  " + name + "=" + value
}

// wrapperDefCmd is `skopos wrapper …`: wrapper definitions on the server
// (phase 2d).
func wrapperDefCmd() *cli.Command {
	return &cli.Command{
		Name:  "wrapper",
		Usage: "Wrapper definitions on the server: a command a stage's runs start behind, attached by a stage block's wrap rules",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List the wrapper definitions",
				Flags: keyClientFlags(),
				Run: func(ctx context.Context, cmd *cli.Command) error {
					var out struct {
						Wrappers []wsconfig.WrapperView `json:"wrappers"`
					}
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, "/api/fleet/wrappers", nil, &out); err != nil {
						return err
					}
					if len(out.Wrappers) == 0 {
						fmt.Println("no wrapper definitions")
						return nil
					}
					for _, w := range out.Wrappers {
						fmt.Printf("%s  (executor %s)\n", w.Name, w.Executor)
						fmt.Printf("  argv  %s\n", jsonLine(w.Argv))
						if len(w.Check) > 0 {
							fmt.Printf("  check %s\n", jsonLine(w.Check))
						}
						if len(w.UsedBy) > 0 {
							fmt.Printf("  named by %s\n", strings.Join(w.UsedBy, ", "))
						}
					}
					return nil
				},
			},
			{
				Name:  "set",
				Usage: "Define or redefine a wrapper (admin over every workspace)",
				Description: `A wrapper is the argv prefix a run starts behind: the agent, and the worker's
setup and check commands of the stage. A stage block attaches it by rule:

  skopos workspace config set --set 'stages.review={"wrap":[{"kind":"opencode","wrapper":"opencode-bwrap"}]}'

The worker fills in {dir} {gitdir} {login} {rundir} {cli} {run} {stage}
{workspace} for each run, and ${NAME} from its own [worker.vars]. Only a worker
with server_commands = true runs a wrapper from the server.

--check is a command that shows the wrapper can start on a host. A worker
where it fails takes no run the wrapper is for. It does not show that the
wrapper isolates anything.`,
				Flags: append(keyClientFlags(),
					&cli.StringFlag{Name: "argv", Usage: `The wrapper's argv as a JSON array: '["bwrap","--ro-bind","/","/","--bind","{dir}","{dir}"]'`},
					&cli.StringFlag{Name: "check", Usage: `A command that shows the wrapper starts, as a JSON array: '["bwrap","--ro-bind","/","/","true"]'`},
					&cli.StringFlag{Name: "file", Usage: "A TOML file with argv and check, instead of the flags"},
				),
				Arguments: []cli.Argument{&cli.StringArg{Name: "name", Usage: "Wrapper name", Required: true}},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.GetStringArg("name")
					var def wsconfig.WrapperDef
					if file := cmd.GetString("file"); file != "" {
						if cmd.GetString("argv") != "" || cmd.GetString("check") != "" {
							return fmt.Errorf("--file replaces --argv and --check: use one or the other")
						}
						md, err := toml.DecodeFile(file, &def)
						if err != nil {
							return fmt.Errorf("reading %s: %w", file, err)
						}
						if extra := md.Undecoded(); len(extra) > 0 {
							return fmt.Errorf("%s: %s is not a wrapper field (argv, check)", file, extra[0])
						}
					} else {
						for flag, dst := range map[string]*[]string{"argv": &def.Argv, "check": &def.Check} {
							if v := strings.TrimSpace(cmd.GetString(flag)); v != "" {
								if err := json.Unmarshal([]byte(v), dst); err != nil {
									return fmt.Errorf("--%s: a JSON array of strings (%v)", flag, err)
								}
							}
						}
					}
					var v wsconfig.WrapperView
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodPut, "/api/fleet/wrappers/"+url.PathEscape(name), def, &v); err != nil {
						return err
					}
					fmt.Printf("saved %s (revision %s): runs inside it record executor %s\n", v.Name, v.Rev, v.Executor)
					return nil
				},
			},
			{
				Name:  "delete",
				Usage: "Remove a wrapper definition (admin over every workspace)",
				Flags: append(keyClientFlags(),
					&cli.BoolFlag{Name: "force", Usage: "Delete it although stage blocks still name it: runs their rules match stop until the rule goes"}),
				Arguments: []cli.Argument{&cli.StringArg{Name: "name", Usage: "Wrapper name", Required: true}},
				Run: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.GetStringArg("name")
					path := "/api/fleet/wrappers/" + url.PathEscape(name)
					if cmd.GetBool("force") {
						path += "?force=1"
					}
					if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodDelete, path, nil, nil); err != nil {
						return err
					}
					fmt.Println("deleted " + name)
					return nil
				},
			},
		},
	}
}

func jsonLine(v any) string {
	buf, _ := json.Marshal(v)
	return string(buf)
}
