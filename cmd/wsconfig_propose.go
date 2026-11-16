package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/repoprobe"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
	"github.com/paularlott/cli"
)

// configProposeCmd is `skopos workspace config propose`: the workspace's
// config read from its repository's files, saved only with --save and then
// only where nothing sets the field yet.
func configProposeCmd() *cli.Command {
	return &cli.Command{
		Name:  "propose",
		Usage: "Propose a workspace's config from its repository's files (kits, setup, checks, push); --save adds the fields nothing sets yet",
		Description: `Reads a checkout of the repository (--dir, default: this checkout) — the
files git tracks, as a worker's clone holds them — and proposes the config its workers need: the kits for its toolchains, setup that
installs its dependencies, the checks its task runner or toolchain defines,
and for a workspace on github.com the push block. Each field comes with the
file it was read from.

Nothing is saved without --save, and --save adds only the fields neither the
workspace nor its groups set: it never replaces a field someone chose. Read
the proposal first: a kit gives the stages' agents its tools and hosts.

A workspace with no config on the server takes its fields from its workers'
files. --save refuses it: a config of its own would make it managed, and the
files' fields would no longer apply. Import them first (skopos workspace
config import), or pass --manage to start the config from the proposal alone.`,
		Flags: append(keyClientFlags(),
			&cli.StringFlag{Name: "workspace", Usage: "Workspace ID (default: this checkout's workspace)"},
			&cli.StringFlag{Name: "dir", Usage: "The repository's checkout to read (default: this checkout's top folder)"},
			&cli.BoolFlag{Name: "save", Usage: "Save the proposed fields nothing sets yet (admin permission)"},
			&cli.BoolFlag{Name: "manage", Usage: "With --save on a workspace that has no config on the server: start one from the proposal alone (its workers' files are then not read for it)"},
		),
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ws := workspaceOrDefault(strings.TrimSpace(cmd.GetString("workspace")))
			if ws == "" {
				return fmt.Errorf("pass --workspace (not inside a git checkout)")
			}
			dir := strings.TrimSpace(cmd.GetString("dir"))
			if dir == "" {
				out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
				if err != nil {
					return fmt.Errorf("pass --dir (not inside a git checkout)")
				}
				dir = strings.TrimSpace(string(out))
			}
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a folder", dir)
			}
			// What a worker's clone holds: the files git tracks. A checkout
			// also has downloaded copies, generated folders and data.
			var p *repoprobe.Proposal
			var err error
			if out, lerr := exec.Command("git", "-C", dir, "ls-files", "-z").Output(); lerr == nil {
				var tracked []string
				for _, f := range strings.Split(string(out), "\x00") {
					if f != "" {
						tracked = append(tracked, f)
					}
				}
				p, err = repoprobe.ProposeTracked(os.DirFS(dir), tracked, ws)
			} else {
				fmt.Printf("note: %s is not a git checkout: every file in it is read\n", dir)
				p, err = repoprobe.Propose(os.DirFS(dir), ws)
			}
			if err != nil {
				return err
			}
			printProposal(ws, dir, p)
			if !cmd.GetBool("save") {
				if len(p.Config.Fields()) > 0 {
					fmt.Println("\nNothing saved. --save adds the fields neither the workspace nor its groups set.")
				}
				return nil
			}
			var cur wsconfig.View
			if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodGet, configPath(pipeline.KindWorkspace, ws), nil, &cur); err != nil {
				return err
			}
			// A workspace without a config of its own takes every shared field
			// from its workers' files. Saving one makes it managed: each field
			// the server does not set then has its default, not the file's
			// value. So the proposal is saved only over a config that exists,
			// or when the caller says so.
			if cur.Config == nil && !cmd.GetBool("manage") {
				return fmt.Errorf("the workspace has no config on the server: saving one makes it managed, and every field its workers' files set (default_branch, checkout, timeouts, …) would then have its default instead. Take the files' fields first (skopos workspace config import --from <worker config> --workspace %s --yes), or pass --manage to save the proposal alone", ws)
			}
			own := wsconfig.Config{}
			if cur.Config != nil {
				own = *cur.Config
			}
			var sources map[string]string
			if cur.Effective != nil {
				sources = map[string]string{}
				for f, src := range cur.Effective.Sources {
					if strings.HasPrefix(src, "group:") {
						sources[f] = src
					}
				}
			}
			next, added, err := p.Fill(own, sources)
			if err != nil {
				return err
			}
			if len(added) == 0 {
				fmt.Println("\nnothing to add: every proposed field is set already")
				return nil
			}
			body := map[string]any{"config": next, "if_rev": cur.Rev, "note": "proposed from the repository"}
			var v wsconfig.View
			if err := configCall(ctx, cmd, audit.ViaCLI, http.MethodPut, configPath(pipeline.KindWorkspace, ws), body, &v); err != nil {
				return err
			}
			fmt.Printf("\nsaved (revision %s): %s\n", v.Rev, strings.Join(added, ", "))
			return nil
		},
	}
}

// plainJSON is a JSON value as a person reads it: & < > not escaped.
func plainJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return string(raw)
	}
	return strings.TrimSpace(b.String())
}

func printProposal(ws, dir string, p *repoprobe.Proposal) {
	fmt.Printf("workspace %s, read from %s\n", ws, dir)
	fields := p.Config.Fields()
	if len(fields) == 0 {
		fmt.Println("\nnothing to propose")
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("\n%s = %s\n", n, plainJSON(fields[n]))
		for _, why := range p.Why[n] {
			fmt.Println("    " + why)
		}
	}
	if len(p.Notes) > 0 {
		fmt.Println("\nnotes:")
		for _, n := range p.Notes {
			fmt.Println("  - " + n)
		}
	}
}
