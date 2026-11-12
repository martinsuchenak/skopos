package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/worker"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
	"github.com/paularlott/cli"
)

// configImportCmd is `skopos workspace config import`: propose server
// config from what a worker's file says, show the difference, and apply it
// on --yes (tmp/central-config — design.md, phase 2, migration).
func configImportCmd() *cli.Command {
	return &cli.Command{
		Name:  "import",
		Usage: "Take workspace config and runner definitions from workers' files: shows the changes, applies them with --yes",
		Description: `Reads the [worker] section of each file and proposes, for every workspace it
has a repo for, the config the server would hold — the fields the file sets —
and a runner definition for each runner its accounts name. Two files that
disagree about a workspace or a runner are a conflict: it is shown and not
applied, never merged.

A runner definition sets that runner's model and effort on every worker that
offers the name, not only on the one the file comes from.

Nothing changes without --yes. Each change needs the admin permission and is
recorded as an import. After importing a workspace, set
worker.server_commands = true on its workers so they take its setup and checks
from the server; the fields the server now holds can then go from their files.`,
		Flags: append(keyClientFlags(),
			&cli.StringSliceFlag{Name: "from", Usage: "A worker's config file (repeat for several workers)"},
			&cli.StringSliceFlag{Name: "workspace", Usage: "Import only this workspace (repeat for several; default: every workspace in the files)"},
			&cli.BoolFlag{Name: "runners", Usage: "Import runner definitions too (admin over every workspace)"},
			&cli.BoolFlag{Name: "yes", Usage: "Apply the changes"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			files := cmd.GetStringSlice("from")
			if len(files) == 0 {
				return fmt.Errorf("pass --from <worker config file>")
			}
			prop, err := proposeImport(files)
			if err != nil {
				return err
			}
			only := map[string]bool{}
			for _, ws := range cmd.GetStringSlice("workspace") {
				only[strings.TrimSpace(ws)] = true
			}
			apply := cmd.GetBool("yes")
			changes := 0
			for _, c := range prop.conflicts {
				fmt.Println("conflict: " + c)
			}
			for _, ws := range sortedNames(prop.workspaces) {
				if len(only) > 0 && !only[ws] {
					continue
				}
				next := prop.workspaces[ws]
				var cur wsconfig.View
				if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, configPath(pipeline.KindWorkspace, ws), nil, &cur); err != nil {
					return fmt.Errorf("workspace %s: %w", ws, err)
				}
				was := wsconfig.Config{}
				state := "not managed yet"
				if cur.Config != nil {
					was, state = *cur.Config, "revision "+cur.Rev
				}
				fields := wsconfig.Changed(was, next)
				if len(fields) == 0 && cur.Config != nil {
					fmt.Printf("workspace %s: no change (%s)\n", ws, state)
					continue
				}
				changes++
				fmt.Printf("workspace %s (%s):\n", ws, state)
				a, b := was.Fields(), next.Fields()
				for _, f := range fields {
					fmt.Printf("  %s: %s -> %s\n", f, wsconfig.Describe(a[f]), wsconfig.Describe(b[f]))
				}
				if len(fields) == 0 {
					fmt.Println("  becomes managed with every field at its default")
				}
				if !apply {
					continue
				}
				var v wsconfig.View
				if err := configCall(ctx, cmd, audit.ViaImport, http.MethodPut, configPath(pipeline.KindWorkspace, ws), map[string]any{"config": next, "note": "imported from " + strings.Join(prop.sources[ws], ", ")}, &v); err != nil {
					return fmt.Errorf("workspace %s: %w", ws, err)
				}
				fmt.Printf("  imported (revision %s)\n", v.Rev)
			}
			if cmd.GetBool("runners") {
				var have struct {
					Runners []wsconfig.RunnerView `json:"runners"`
				}
				if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, "/api/fleet/runners", nil, &have); err != nil {
					return err
				}
				current := map[string]wsconfig.RunnerDef{}
				for _, r := range have.Runners {
					current[r.Name] = r.RunnerDef
				}
				for _, name := range sortedNames(prop.runners) {
					def := prop.runners[name]
					if cur, ok := current[name]; ok && cur == def {
						fmt.Printf("runner %s: no change\n", name)
						continue
					}
					changes++
					fmt.Printf("runner %s: kind=%s%s%s%s\n", name, def.Kind, optField("model", def.Model), optField("effort", def.Effort), optField("provider", def.Provider))
					if !apply {
						continue
					}
					if err := configCall(ctx, cmd, audit.ViaImport, http.MethodPut, "/api/fleet/runners/"+url.PathEscape(name), def, nil); err != nil {
						return fmt.Errorf("runner %s: %w", name, err)
					}
					fmt.Println("  imported")
				}
			} else if len(prop.runners) > 0 {
				fmt.Printf("%d runner definitions could be imported too (--runners): %s\n", len(prop.runners), strings.Join(sortedNames(prop.runners), ", "))
			}
			switch {
			case changes == 0 && len(prop.conflicts) == 0:
				fmt.Println("nothing to import")
			case !apply && changes > 0:
				fmt.Println("nothing was changed: pass --yes to apply")
			}
			if len(prop.conflicts) > 0 {
				return fmt.Errorf("%d conflict(s) between the files were not imported: make the files agree, or import one file", len(prop.conflicts))
			}
			return nil
		},
	}
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// importProposal is what a set of worker files says the server should hold.
type importProposal struct {
	workspaces map[string]wsconfig.Config
	sources    map[string][]string
	runners    map[string]wsconfig.RunnerDef
	conflicts  []string
}

// proposeImport reads worker files and builds the proposal. A workspace or
// runner two files describe differently is a conflict and is left out.
func proposeImport(files []string) (*importProposal, error) {
	p := &importProposal{workspaces: map[string]wsconfig.Config{}, sources: map[string][]string{}, runners: map[string]wsconfig.RunnerDef{}}
	badWS, badRunner := map[string]bool{}, map[string]bool{}
	for _, path := range files {
		var file struct {
			Worker *worker.Config `toml:"worker"`
		}
		if _, err := toml.DecodeFile(path, &file); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		if file.Worker == nil {
			return nil, fmt.Errorf("%s has no [worker] section", path)
		}
		w := file.Worker
		for ws, repo := range w.Repos {
			if repo == nil {
				continue
			}
			c, err := wsconfig.Normalize(configFromWorker(w, repo))
			if err != nil {
				return nil, fmt.Errorf("%s, workspace %s: %w", path, ws, err)
			}
			if prev, ok := p.workspaces[ws]; ok {
				if diff := wsconfig.Changed(prev, c); len(diff) > 0 {
					badWS[ws] = true
					p.conflicts = append(p.conflicts, fmt.Sprintf("workspace %s: %s and %s differ in %s", ws, strings.Join(p.sources[ws], ", "), path, strings.Join(diff, ", ")))
				}
			} else {
				p.workspaces[ws] = c
			}
			p.sources[ws] = append(p.sources[ws], path)
		}
		// Runner definitions: what the file's accounts name. The provider is
		// the one the worker would report — derived with the file loaded the
		// way the worker loads it.
		facts := map[string]string{}
		loaded := &worker.Config{}
		if err := loadWorkerConfigFile(loaded, path); err == nil {
			if home, herr := os.UserHomeDir(); herr == nil && loaded.Default(home) == nil {
				for _, f := range loaded.RunnerFacts() {
					if !strings.HasPrefix(f.Provider, "unknown:") {
						facts[f.Name] = f.Provider
					}
				}
			}
		}
		for _, a := range w.Accounts {
			if a == nil || a.Kind != "" {
				continue
			}
			name := a.Runner
			if name == "" {
				name = "claude"
			}
			kind := name
			if spec := w.RunnerSpecs[name]; spec != nil && spec.Kind != "" {
				kind = spec.Kind
			}
			if kind == "command" {
				continue // a command runner's argv stays in the worker's file
			}
			def, err := wsconfig.NormalizeRunner(wsconfig.RunnerDef{Name: name, Kind: kind, Model: a.Model, Effort: a.Effort, Provider: facts[name]})
			if err != nil {
				return nil, fmt.Errorf("%s, account %s: %w", path, a.Name, err)
			}
			if prev, ok := p.runners[name]; ok && prev != def {
				badRunner[name] = true
				p.conflicts = append(p.conflicts, fmt.Sprintf("runner %s: defined differently (%s; and %s in %s)", name, importRunnerLine(prev), importRunnerLine(def), path))
				continue
			}
			p.runners[name] = def
		}
	}
	for ws := range badWS {
		delete(p.workspaces, ws)
	}
	for name := range badRunner {
		delete(p.runners, name)
	}
	sort.Strings(p.conflicts)
	return p, nil
}

func importRunnerLine(d wsconfig.RunnerDef) string {
	return strings.TrimSpace("kind=" + d.Kind + optField("model", d.Model) + optField("effort", d.Effort) + optField("provider", d.Provider))
}

// configFromWorker is the server config a worker file's repo describes:
// only the fields the file sets. The worker-wide timeouts and MCP lists
// apply to each of its workspaces.
func configFromWorker(w *worker.Config, r *worker.Repo) wsconfig.Config {
	var c wsconfig.Config
	str := func(dst **string, v string) {
		if v != "" {
			*dst = &v
		}
	}
	list := func(dst **[]string, v []string) {
		if len(v) > 0 {
			*dst = &v
		}
	}
	dur := func(dst **wsconfig.Duration, v time.Duration) {
		if v != 0 {
			d := wsconfig.Duration(v)
			*dst = &d
		}
	}
	str(&c.DefaultBranch, r.DefaultBranch)
	str(&c.BranchPrefix, r.BranchPrefix)
	str(&c.BranchTemplate, r.BranchTemplate)
	str(&c.Checkout, r.Checkout)
	list(&c.ProtectedBranches, r.Protected)
	list(&c.Setup, r.Setup)
	list(&c.Checks, r.Checks)
	c.CheckFixRounds = r.CheckFixRounds
	list(&c.Preflight, r.Preflight)
	dur(&c.PreflightInterval, r.PreflightInterval)
	dur(&c.SetupTimeout, r.SetupTimeout)
	dur(&c.CheckTimeout, r.CheckTimeout)
	dur(&c.QuietTimeout, r.QuietTimeout)
	dur(&c.PlanTimeout, w.PlanTimeout)
	dur(&c.ImplTimeout, w.ImplTimeout)
	if r.Push.Kind != "" || r.Push.RemoteURL != "" || r.Push.PullRequest != "" || r.Push.Steps != nil {
		c.Push = &wsconfig.Push{Steps: r.Push.Steps}
		str(&c.Push.Kind, r.Push.Kind)
		str(&c.Push.RemoteURL, r.Push.RemoteURL)
		str(&c.Push.PullRequest, r.Push.PullRequest)
	}
	list(&c.MCPDeny, w.MCPDeny)
	list(&c.MCPServers, w.MCPServers)
	// What the file's profile gives each stage. What contains a stage (a
	// wrapper, the sandbox block) is the host's and stays in the file.
	for _, stage := range wsconfig.StageNames {
		p := r.Stages[stage]
		if p == nil && r.Profile != "" {
			p = w.Profiles[r.Profile][stage]
		}
		if p == nil {
			continue
		}
		block := &wsconfig.StageCaps{Tools: p.Tools, Env: p.Env, Settings: wsconfig.Settings(settingsWithoutSandbox(p.Settings))}
		if len(block.Tools) == 0 && len(block.Env) == 0 && block.Settings == "" {
			continue
		}
		if c.Stages == nil {
			c.Stages = map[string]*wsconfig.StageCaps{}
		}
		c.Stages[stage] = block
	}
	return c
}

// settingsWithoutSandbox is a profile's settings without the sandbox block
// ("" when nothing else is in them, or they are not JSON — the server then
// says what is wrong with them).
func settingsWithoutSandbox(settings string) string {
	if strings.TrimSpace(settings) == "" {
		return ""
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settings), &top); err != nil {
		return settings
	}
	delete(top, "sandbox")
	if len(top) == 0 {
		return ""
	}
	buf, _ := json.Marshal(top)
	return string(buf)
}
