package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Stage capabilities and wrappers from the server (tmp/central-config —
// design.md, phase 2d).
//
// A stage profile does two jobs: it says what the stage's agent is given
// (tools, env, settings) and what contains the run (a wrapper, a sandbox).
// For a workspace the server manages — on a worker with
// worker.server_commands — the first comes from the workspace's config, per
// stage, and the worker's file no longer says it. The second stays this
// host's: worker.contained_by, and a wrapper or sandbox settings the file
// sets for the stage, are kept. The server adds a wrapper only where the
// file sets none, through a rule in the stage's block that names a wrapper
// the server defines; the server never takes one away.
//
// Without worker.server_commands the file's profiles apply as they always
// did: tools, env and a wrapper are commands on this host.

// wrapperCheckTimeout bounds a wrapper's check command.
const wrapperCheckTimeout = 30 * time.Second

// wrapperCheckEvery is how often a wrapper's check runs again.
const wrapperCheckEvery = 5 * time.Minute

// stagesFromServer reports whether the workspace's stage blocks come from
// the server on this worker.
func (c *Config) stagesFromServer(ws string) bool {
	_, ok := c.srvStages[ws]
	return ok
}

// setStages makes blocks the workspace's stage blocks (nil = the worker's
// file applies again).
func (c *Config) setStages(ws string, blocks map[string]*wsconfig.StageCaps) {
	delete(c.stagesBroken, ws)
	if blocks == nil {
		delete(c.srvStages, ws)
		return
	}
	if c.srvStages == nil {
		c.srvStages = map[string]map[string]*wsconfig.StageCaps{}
	}
	c.srvStages[ws] = blocks
}

// breakStages records that the server holds the workspace's stage blocks
// and this worker cannot use them (why): nothing of the workspace runs here
// until that changes — the file's profiles do not step in.
func (c *Config) breakStages(ws, why string) {
	delete(c.srvStages, ws)
	if c.stagesBroken == nil {
		c.stagesBroken = map[string]string{}
	}
	c.stagesBroken[ws] = why
}

// prepareStages checks the stage blocks the server sends for use on this
// host and fills in the host's variables. An error says why they cannot be
// used here.
func (c *Config) prepareStages(in map[string]*wsconfig.StageCaps) (map[string]*wsconfig.StageCaps, error) {
	out := map[string]*wsconfig.StageCaps{}
	stages := make([]string, 0, len(in))
	for stage := range in {
		stages = append(stages, stage)
	}
	sort.Strings(stages)
	for _, stage := range stages {
		// The server checked the block when it was saved; the worker does
		// not take its word for what runs here.
		block, err := wsconfig.NormalizeStage(stage, in[stage])
		if err != nil {
			return nil, fmt.Errorf("the server's config has a stage block this worker does not accept: %v", err)
		}
		names := make([]string, 0, len(block.Env))
		for k := range block.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			v, err := wsconfig.Expand(block.Env[k], c.Vars)
			if err != nil {
				return nil, fmt.Errorf("stages.%s: env %s: %v", stage, k, err)
			}
			block.Env[k] = v
		}
		out[stage] = block
	}
	return out, nil
}

// setWrappers takes the server's wrapper definitions and sorts them into
// the ones this worker can use and the ones it refuses (wrapperInvalid): a
// definition the worker's own check of it rejects, or one that needs a
// variable this host does not declare. A refused definition is not kept —
// nothing can make it usable but a new definition — while the result of a
// wrapper's check command (wrapperBad) comes and goes with the check.
func (c *Config) setWrappers(defs []wsconfig.WrapperDef) {
	prev, prevBad := c.wrappers, c.wrapperBad
	c.wrappers = map[string]wsconfig.WrapperDef{}
	c.wrapperInvalid = map[string]string{}
	c.wrapperBad = map[string]string{}
	for _, d := range defs {
		// As with stage blocks: the server checked it, and the worker does
		// not take its word for what runs here.
		norm, err := wsconfig.NormalizeWrapper(d)
		if err != nil {
			c.wrapperInvalid[d.Name] = err.Error()
			continue
		}
		if _, err := c.wrapperArgv(norm.Argv); err != nil {
			c.wrapperInvalid[norm.Name] = err.Error()
			continue
		}
		if _, err := c.wrapperArgv(norm.Check); err != nil {
			c.wrapperInvalid[norm.Name] = "its check: " + err.Error()
			continue
		}
		c.wrappers[norm.Name] = norm
		// A wrapper whose check failed stays unusable until a check
		// passes: an unchanged definition keeps its result, and a changed
		// one with a check is checked before it is offered again.
		if old, ok := prev[norm.Name]; ok && (len(norm.Check) > 0 || (slices.Equal(old.Argv, norm.Argv) && slices.Equal(old.Check, norm.Check))) {
			if why := prevBad[norm.Name]; why != "" {
				c.wrapperBad[norm.Name] = why
			}
		}
	}
}

// wrapperArgv fills this host's variables into a wrapper's argv.
func (c *Config) wrapperArgv(argv []string) ([]string, error) {
	out := make([]string, len(argv))
	for i, a := range argv {
		v, err := wsconfig.Expand(a, c.Vars)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// runnerFact is what the worker reports about one runner (zero when it
// does not have it).
func (c *Config) runnerFact(name string) policy.Facts {
	for _, f := range c.RunnerFacts() {
		if f.Name == name {
			return f
		}
	}
	// No account feeds it right now: its kind is still known.
	_, kind := c.runnerSpec(name)
	return policy.Facts{Name: name, Kind: kind}
}

// StageProfile is what a stage of the workspace's runs gets, whatever the
// runner: the tools, env and settings, and the containment this worker's
// file sets. A wrapper the server attaches by runner is stageProfileFor's.
// Nil = a plain run.
func (c *Config) StageProfile(workspace, stage string) *StageProfile {
	p, _ := c.stageProfileFor(workspace, stage, "")
	return p
}

// stageProfileFor resolves what a run of a stage on a runner gets in a
// workspace. problem is set when the workspace's config wraps that runner's
// runs in a wrapper this worker cannot use: the run must then not start.
func (c *Config) stageProfileFor(workspace, stage, runner string) (prof *StageProfile, problem string) {
	file := c.fileProfile(workspace, stage)
	if why := c.stagesBroken[workspace]; why != "" {
		// The server holds the workspace's stages and this worker cannot
		// use them: not the file's profiles instead.
		return nil, why
	}
	blocks, fromServer := c.srvStages[workspace]
	if !fromServer {
		return file, ""
	}
	p := &StageProfile{server: true}
	// What contains the run on this host stays the file's: its wrapper,
	// its sandbox block, and what its settings deny.
	var sandbox string
	var deny []string
	if file != nil {
		p.Wrapper, p.name = file.Wrapper, file.name
		if file.sandboxed() {
			sandbox = file.sandboxSettings()
		}
		deny = file.denyRules()
	}
	if block := blocks[stage]; block != nil {
		p.Tools, p.Env, p.Settings = block.Tools, block.Env, string(block.Settings)
		// A wrapper from the server only where the file sets no real one:
		// what the operator put around the stage is never replaced.
		if !file.wraps() && runner != "" {
			if name := block.WrapperFor(c.runnerFact(runner)); name != "" {
				def, ok := c.wrappers[name]
				switch {
				case c.wrapperInvalid[name] != "":
					problem = fmt.Sprintf("the workspace's config wraps %s runs of %s in wrapper %s, which this worker does not accept: %s", runner, stage, name, c.wrapperInvalid[name])
				case !ok:
					problem = fmt.Sprintf("the workspace's config wraps %s runs of %s in wrapper %s, which the server does not define", runner, stage, name)
				case c.wrapperBad[name] != "":
					problem = fmt.Sprintf("the workspace's config wraps %s runs of %s in wrapper %s, which cannot be used on this worker: %s", runner, stage, name, c.wrapperBad[name])
				default:
					argv, err := c.wrapperArgv(def.Argv)
					if err != nil {
						problem = fmt.Sprintf("wrapper %s: %v", name, err)
						break
					}
					p.Wrapper, p.name, p.hash = argv, name, wsconfig.ArgvHash(def.Argv)
				}
			}
		}
	}
	p.Settings = withContainment(p.Settings, sandbox, deny)
	if len(p.Wrapper) == 0 && len(p.Tools) == 0 && len(p.Env) == 0 && p.Settings == "" {
		return nil, problem
	}
	return p, problem
}

// withContainment lays what this host's file says about containment over
// a stage's settings from the server: its sandbox block replaces one the
// server's block carries, and what it denies is added to what the server
// denies. Both only ever narrow what a run may do.
func withContainment(settings, sandbox string, deny []string) string {
	if sandbox == "" && len(deny) == 0 {
		return settings
	}
	top := map[string]json.RawMessage{}
	if strings.TrimSpace(settings) != "" {
		_ = json.Unmarshal([]byte(settings), &top)
	}
	if sandbox != "" {
		top["sandbox"] = json.RawMessage(sandbox)
	}
	if len(deny) > 0 {
		perms := map[string]json.RawMessage{}
		_ = json.Unmarshal(top["permissions"], &perms)
		var have []string
		_ = json.Unmarshal(perms["deny"], &have)
		for _, d := range deny {
			if !slices.Contains(have, d) {
				have = append(have, d)
			}
		}
		perms["deny"], _ = json.Marshal(have)
		top["permissions"], _ = json.Marshal(perms)
	}
	buf, _ := json.Marshal(top)
	return string(buf)
}

// denyRules are the permissions.deny entries of the profile's settings.
func (p *StageProfile) denyRules() []string {
	if p == nil || strings.TrimSpace(p.Settings) == "" {
		return nil
	}
	var s struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(p.Settings), &s)
	return s.Permissions.Deny
}

// fileGrants lists the stages for which the worker's file gives a
// workspace's runs something (tools, env, settings beyond the sandbox) —
// what a workspace whose stages the server holds no longer takes from it.
func (c *Config) fileGrants(ws string) []string {
	var out []string
	for _, stage := range wsconfig.StageNames {
		p := c.fileProfile(ws, stage)
		if p == nil {
			continue
		}
		// The sandbox block and the deny rules are kept; anything else in
		// the settings is no longer used.
		var settings map[string]json.RawMessage
		_ = json.Unmarshal([]byte(p.Settings), &settings)
		delete(settings, "sandbox")
		if perms := map[string]json.RawMessage{}; json.Unmarshal(settings["permissions"], &perms) == nil {
			delete(perms, "deny")
			if len(perms) == 0 {
				delete(settings, "permissions")
			}
		}
		if len(p.Tools) > 0 || len(p.Env) > 0 || len(settings) > 0 {
			out = append(out, "stages."+stage)
		}
	}
	return out
}

// fileStage renders what the worker's file gives a stage, for the change
// log ("" = nothing).
func (c *Config) fileStage(ws, stage string) string {
	p := c.fileProfile(ws, stage)
	if p == nil || (len(p.Tools) == 0 && len(p.Env) == 0 && strings.TrimSpace(p.Settings) == "") {
		return ""
	}
	names := make([]string, 0, len(p.Env))
	for k := range p.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	buf, _ := json.Marshal(map[string]any{"tools": p.Tools, "env_names": names, "settings": p.Settings})
	return string(buf)
}

// usedWrappers lists the server-defined wrappers the held stage blocks
// name, sorted.
func (c *Config) usedWrappers() []string {
	var out []string
	for _, blocks := range c.srvStages {
		for _, b := range blocks {
			for _, name := range b.Wrappers() {
				if !slices.Contains(out, name) {
					out = append(out, name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ExecutorProblems says, for each workspace, stage and runner the worker
// cannot run under the server's config, why not — the cases it reports as
// executor none:….
func (c *Config) ExecutorProblems() []string {
	var runners []string
	for _, f := range c.RunnerFacts() {
		runners = append(runners, f.Name)
	}
	var out []string
	for _, ws := range c.Workspaces {
		if why := c.stagesBroken[ws]; why != "" {
			out = append(out, fmt.Sprintf("%s: %s — this worker takes none of its runs", ws, why))
			continue
		}
		whose := "this worker's file"
		if c.stagesFromServer(ws) {
			whose = "the server's config"
		}
		for _, stage := range wsconfig.StageNames {
			for _, r := range runners {
				prof, problem := c.stageProfileFor(ws, stage, r)
				switch {
				case problem != "":
					out = append(out, fmt.Sprintf("%s: %s — this worker takes no %s run of it for %s", ws, problem, stage, r))
				case prof.grants() && !c.contained(prof, r):
					out = append(out, fmt.Sprintf("%s: %s gives the %s stage tools, env or settings, and nothing contains a %s run of it on this worker — it takes none (set worker.contained_by when the whole host is a container, or a wrapper for the stage in this worker's file; sandbox settings contain only a runner that honours them)", ws, whose, stage, r))
				}
			}
		}
	}
	return out
}

// CheckWrappers runs the check command of every wrapper the held stage
// blocks name (again when one is older than wrapperCheckEvery, or all of
// them with force) and records which cannot start here. It reports whether
// what the worker can run changed.
func (c *Config) CheckWrappers(ctx context.Context, checked map[string]time.Time, force bool, env []string) (changed bool, notes []string) {
	now := time.Now()
	for _, name := range c.usedWrappers() {
		def, ok := c.wrappers[name]
		if !ok || len(def.Check) == 0 {
			continue
		}
		key := name + ":" + wsconfig.ArgvHash(append(slices.Clone(def.Argv), def.Check...))
		if at, seen := checked[key]; seen && !force && now.Sub(at) < wrapperCheckEvery {
			continue
		}
		checked[key] = now
		was := c.wrapperBad[name]
		why := c.runWrapperCheck(ctx, def, env)
		if ctx.Err() != nil {
			return changed, notes // shutting down: not a verdict
		}
		switch {
		case why == "" && was == "":
		case why == "":
			delete(c.wrapperBad, name)
			changed = true
			notes = append(notes, fmt.Sprintf("wrapper %s starts on this worker again", name))
		case was == "":
			c.wrapperBad[name] = why
			changed = true
			notes = append(notes, fmt.Sprintf("wrapper %s cannot be used on this worker: %s", name, why))
		default:
			// Still failing: the newest reason is kept, and nothing the
			// worker reports changes (a check's output may differ each
			// time it runs).
			c.wrapperBad[name] = why
		}
	}
	return changed, notes
}

// runWrapperCheck runs one wrapper's check command and says why it failed
// ("" = it passed). The check shows the wrapper can start on this host —
// not that it isolates anything.
func (c *Config) runWrapperCheck(ctx context.Context, def wsconfig.WrapperDef, env []string) string {
	argv, err := c.wrapperArgv(def.Check)
	if err != nil {
		return "its check: " + err.Error()
	}
	cctx, cancel := context.WithTimeout(ctx, wrapperCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Env = env
	containProcess(cmd)
	out, err := cmd.CombinedOutput()
	if err = settled(cmd, err); err == nil {
		return ""
	}
	if cctx.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("timed out after %s", wrapperCheckTimeout)
	}
	return fmt.Sprintf("its check (%s) failed: %v: %s", strings.Join(def.Check, " "), err, tail(strings.TrimSpace(string(out)), 300))
}

// checkWrappers is CheckWrappers for the running worker: results are
// logged, and what the worker reports about its executors is worked out
// again when one changed.
func (w *Worker) checkWrappers(ctx context.Context, force bool) {
	if len(w.cfg.usedWrappers()) == 0 {
		return
	}
	if w.wrapperChecked == nil {
		w.wrapperChecked = map[string]time.Time{}
	}
	changed, notes := w.cfg.CheckWrappers(ctx, w.wrapperChecked, force, scrubEnv(nil, w.cfg.TokenEnvs(), w.envFileKeys))
	for _, n := range notes {
		w.log.Warn("wrappers: " + n)
	}
	if changed {
		w.resetFacts()
	}
}

// resetFacts makes the next claim work out what the worker reports about
// its runners and executors again.
func (w *Worker) resetFacts() {
	w.factsMu.Lock()
	w.facts, w.factsSet = nil, false
	w.factsMu.Unlock()
}
