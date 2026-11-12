package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Runners: the worker is agent-CLI agnostic. A Runner turns one AgentRun
// (prompt, allowed tools, schema, session, env, wrapper) into one agent
// invocation and its AgentResult. Built in: "claude", "opencode" and
// "codex" (read-only stages), each with a lockdown skopos has verified live
// (instruction files not loaded, files outside the worktree unreadable, the
// tool allowlist enforced — TestLive*). "antigravity" (agy) and "command"
// (any CLI from an argv template) are UNVERIFIED: they only ever run on a
// stage the operator contains with a wrapper (fail closed, runAgent).
type Runner interface {
	// Run executes the agent once (wrapper applied, env scrubbed).
	Run(ctx context.Context, r AgentRun) AgentResult
	// Verified: skopos has verified this runner's lockdown live. An
	// unverified runner only runs inside a wrapper.
	Verified() bool
	// HonorsSettings: the stage profile's `settings` JSON (e.g. Claude
	// Code's sandbox) is applied by this runner — so sandbox.enabled counts
	// as containment only here.
	HonorsSettings() bool
	// Check validates the CLI is usable on pathEnv ("" = this process's).
	Check(ctx context.Context, pathEnv string) error
}

// canRunStage: runner can run stage — false only for a read-only runner
// (codex) on a stage that edits. A runner that does not resolve counts as
// capable: the run then fails with that error, as before.
func (c *Config) canRunStage(runner, stage string) bool {
	if stage != "implement" {
		return true
	}
	r, err := c.RunnerFor(runner)
	if err != nil {
		return true
	}
	ro, ok := r.(interface{ ReadOnly() bool })
	return !ok || !ro.ReadOnly()
}

// RunnerSpec configures a runner name ([worker.runner_config.<name>]).
// kind defaults to the name; "claude" and "opencode" need no block.
type RunnerSpec struct {
	Kind string `toml:"kind"` // claude | opencode | codex | antigravity | command
	Bin  string `toml:"bin"`  // the CLI binary (claude/opencode/codex/agy)
	// Config (opencode): a base opencode.json for runs — providers and
	// models (secrets via {env:VAR}, injected through account env). The
	// worker user's ~/.config/opencode is never read.
	Config string `toml:"config"`
	// Command (command): the argv template. {prompt} {dir} {model}
	// {effort} expand; without {prompt} the prompt goes to stdin.
	Command []string `toml:"command"`
	// Provider and Endpoint declare where the runner's requests go when the
	// worker cannot tell (facts.go): a model on another machine on the
	// local network (provider = "local"), a router (provider = "router"),
	// any `command` runner. A workspace's policy matches on the provider.
	Provider string `toml:"provider"`
	Endpoint string `toml:"endpoint"`
}

var runnerKinds = map[string]bool{"claude": true, "opencode": true, "codex": true, "antigravity": true, "command": true}

// EffortLevels lists the effort values a runner kind takes; nil = any plain
// word (opencode variants, command templates).
func EffortLevels(kind string) []string { return wsconfig.EffortLevels(kind) }

// ValidateEffort refuses an effort a runner kind can't take ("" is always
// fine), so a setting is never silently dropped. command = the template of
// a command runner. The rule is the server's too (a runner definition).
func ValidateEffort(kind string, command []string, effort string) error {
	return wsconfig.ValidateEffort(kind, command, effort)
}

// TemplateHasEffort: a command template uses {effort}.
func TemplateHasEffort(command []string) bool { return wsconfig.TemplateHasEffort(command) }

// checkEffort validates an account's effort for its runner. A command
// template with {effort} needs it on every account: it would expand empty.
func (c *Config) checkEffort(a *Account) error {
	spec, kind := c.runnerSpec(accountRunner(a))
	if kind == "command" && a.Effort == "" && TemplateHasEffort(spec.Command) {
		return fmt.Errorf("runner %s's command has {effort}: set effort on the account", accountRunner(a))
	}
	return ValidateEffort(kind, spec.Command, a.Effort)
}

func (c *Config) runnerSpec(name string) (*RunnerSpec, string) {
	if name == "" {
		name = "claude"
	}
	spec := c.RunnerSpecs[name]
	// A runner the server defines is of the definition's kind, and without
	// a block of its own takes its kind's (runner_config.opencode.config
	// serves every defined opencode runner).
	d := c.def(name)
	if spec == nil && d != nil {
		spec = c.RunnerSpecs[d.Kind]
	}
	if spec == nil {
		spec = &RunnerSpec{}
	}
	kind := spec.Kind
	switch {
	case d != nil:
		kind = d.Kind
	case kind == "":
		kind = name
	}
	return spec, kind
}

// RunnerFor resolves a runner name to its implementation.
func (c *Config) RunnerFor(name string) (Runner, error) {
	spec, kind := c.runnerSpec(name)
	switch kind {
	case "claude":
		bin := spec.Bin
		if bin == "" {
			bin = c.Bin
		}
		return claudeRunner{bin: bin}, nil
	case "opencode":
		bin := spec.Bin
		if bin == "" {
			bin = "opencode"
		}
		return opencodeRunner{bin: bin, config: spec.Config}, nil
	case "codex":
		bin := spec.Bin
		if bin == "" {
			bin = "codex"
		}
		return codexRunner{bin: bin}, nil
	case "antigravity":
		bin := spec.Bin
		if bin == "" {
			bin = "agy"
		}
		return antigravityRunner{bin: bin}, nil
	case "command":
		if len(spec.Command) == 0 {
			return nil, fmt.Errorf("runner %q: kind command needs command = [...]", name)
		}
		return commandRunner{argv: spec.Command}, nil
	}
	return nil, fmt.Errorf("unknown runner %q (built in: claude, opencode, codex, antigravity; or define [worker.runner_config.%s] with kind = \"command\")", name, name)
}

// RunnerBin is the binary a runner name executes (for checks and logs).
func (c *Config) RunnerBin(runner string) string {
	spec, kind := c.runnerSpec(runner)
	switch {
	case spec.Bin != "":
		return spec.Bin
	case kind == "claude":
		return c.Bin
	case kind == "antigravity":
		return "agy"
	case kind == "command" && len(spec.Command) > 0:
		return spec.Command[0]
	}
	return kind
}

// UsedRunners are the runner names the accounts feed (what this worker can
// actually execute), sorted.
func (c *Config) UsedRunners() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range c.Accounts {
		r := a.Runner
		if r == "" {
			r = "claude"
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		// A worker whose accounts only name a kind feeds no runner until
		// the server's definitions arrive: it has no claude to check.
		for _, a := range c.fileAccounts {
			if a.poolAccount() {
				return nil
			}
		}
		out = []string{"claude"}
	}
	sort.Strings(out)
	return out
}

// AccountProblems reports accounts their runner can't use as configured: a
// codex login directory holding an AGENTS.md (codex loads it into every
// run), an antigravity account without a login directory of its own.
func (c *Config) AccountProblems() []string {
	var out []string
	for _, a := range c.Accounts {
		p := ""
		switch _, kind := c.runnerSpec(accountRunner(a)); kind {
		case "codex":
			p = codexHomeProblem(a.Env)
		case "antigravity":
			p = antigravityHomeProblem(a.Env)
		}
		if p != "" {
			out = append(out, "account "+a.Name+": "+p)
		}
	}
	return out
}

// CheckRunners validates every used runner's CLI on pathEnv, and the
// accounts that feed them.
func (c *Config) CheckRunners(ctx context.Context, pathEnv string) []string {
	problems := c.AccountProblems()
	for _, name := range c.UsedRunners() {
		r, err := c.RunnerFor(name)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if err := c.CheckRunner(ctx, name, r, pathEnv); err != nil {
			problems = append(problems, fmt.Sprintf("runner %s: %v", name, err))
		}
	}
	return problems
}

// CheckRunner checks one runner's CLI on pathEnv: its lockdown flags, and
// its effort flag when an account on it sets effort.
func (c *Config) CheckRunner(ctx context.Context, name string, r Runner, pathEnv string) error {
	if err := r.Check(ctx, pathEnv); err != nil {
		return err
	}
	es, ok := r.(interface {
		EffortSupported(ctx context.Context, pathEnv string) error
	})
	if !ok {
		return nil // codex: -c takes any config key; command: the template
	}
	for _, a := range c.Accounts {
		if accountRunner(a) == name && a.Effort != "" {
			return es.EffortSupported(ctx, pathEnv)
		}
	}
	return nil
}

// AccountWarnings: accounts of one runner that differ in model or effort.
// Stages pin runners and the pool rotates accounts within one, so a quota
// switch would quietly change how the stage runs. Not refused — a cheaper
// fallback can be deliberate.
func (c *Config) AccountWarnings() []string {
	type setting struct{ account, model, effort string }
	first := map[string]setting{}
	var out []string
	for _, name := range c.UsedRunners() {
		for _, a := range c.Accounts {
			if accountRunner(a) != name {
				continue
			}
			f, ok := first[name]
			if !ok {
				first[name] = setting{a.Name, a.Model, a.Effort}
				continue
			}
			if f.model != a.Model || f.effort != a.Effort {
				out = append(out, fmt.Sprintf("runner %s: accounts %s (%s) and %s (%s) differ — a quota switch changes how its stages run; give each its own runner name",
					name, f.account, orDefault(runDetail(f.model, f.effort)), a.Name, orDefault(runDetail(a.Model, a.Effort))))
			}
		}
	}
	return out
}

func orDefault(s string) string {
	if s == "" {
		return "CLI defaults"
	}
	return s
}

// runAgent executes one agent run on its runner — fail closed: a runner
// skopos has not verified, or a stage granted capabilities, only runs when
// something contains it (a wrapper, or settings the runner honors).
func (w *Worker) runAgent(ctx context.Context, ar AgentRun) AgentResult {
	r, err := w.cfg.RunnerFor(ar.Runner)
	if err != nil {
		return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
	}
	if ar.StartErr != "" {
		return AgentResult{Outcome: OutcomeError, Detail: ar.StartErr}
	}
	contained := len(ar.Wrapper) > 0 || ar.HostContained || (ar.Sandboxed && r.HonorsSettings())
	if !contained {
		if !r.Verified() {
			return AgentResult{Outcome: OutcomeError, Detail: fmt.Sprintf(
				"runner %q has no lockdown skopos has verified — it only runs on stages contained by a wrapper (worker.profiles.<name>.<stage>.wrapper) or on a worker whose host is a container (worker.contained_by)", ar.Runner)}
		}
		if ar.Granted {
			return AgentResult{Outcome: OutcomeError, Detail: fmt.Sprintf(
				"stage grants tools/env but is not contained for runner %q (sandbox settings only contain the claude runner — use a wrapper)", ar.Runner)}
		}
	}
	rl := runLogFrom(ctx)
	if rl != nil {
		what := ar.Runner
		if d := runDetail(ar.Model, ar.Effort); d != "" {
			what += " (" + d + ")"
		}
		if ar.Resume {
			what += ", resuming its session"
		}
		rl.notef("agent %s started: %s", what, strings.TrimSuffix(filepath.Base(ar.LogFile), ".log"))
	}
	res := r.Run(ctx, ar)
	if rl != nil {
		msg := "agent finished: " + string(res.Outcome)
		if res.CostUSD > 0 {
			msg += fmt.Sprintf(", $%.2f", res.CostUSD)
		}
		if res.Detail != "" {
			msg += " — " + res.Detail
		}
		rl.line("worker", msg, res.Outcome != OutcomeOK)
	}
	return res
}

// --- structured output for runners without a native schema option ---

// schemaInstruction asks for the result as one fenced JSON object; the
// worker extracts and validates it (extractStructured).
func schemaInstruction(schema string) string {
	return "\n\n---\nWhen you are done, end your reply with exactly ONE JSON object that matches this JSON Schema, " +
		"in a ```json fenced block, with nothing after the block:\n\n" + schema + "\n"
}

// extractStructured finds the result object in agent text — the last
// ```json fenced block, else the last balanced top-level {...} — and
// validates it against the schema.
func extractStructured(text, schema string) (json.RawMessage, error) {
	var cands []string
	if i := strings.LastIndex(text, "```json"); i >= 0 {
		rest := text[i+len("```json"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			cands = append(cands, strings.TrimSpace(rest[:j]))
		}
	}
	if obj := lastJSONObject(text); obj != "" {
		cands = append(cands, obj)
	}
	if len(cands) == 0 {
		return nil, errors.New("no JSON object in the reply")
	}
	var lastErr error
	for _, c := range cands {
		var v any
		if err := json.Unmarshal([]byte(c), &v); err != nil {
			lastErr = fmt.Errorf("invalid JSON: %v", err)
			continue
		}
		if err := validateSchema(v, schema); err != nil {
			lastErr = err
			continue
		}
		return json.RawMessage(c), nil
	}
	return nil, lastErr
}

// lastJSONObject returns the last top-level JSON object in s: a forward
// scan that decodes at each '{' and, on success, skips past the object (so
// nested objects never count as candidates).
func lastJSONObject(s string) string {
	last := ""
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			continue
		}
		last = string(raw)
		i += int(dec.InputOffset()) - 1
	}
	return last
}

// validateSchema checks v against the subset of JSON Schema the stage
// schemas use: type, properties, required, enum, items.
func validateSchema(v any, schema string) error {
	var s map[string]any
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		return fmt.Errorf("schema: %v", err)
	}
	return validateNode(v, s, "$")
}

func validateNode(v any, s map[string]any, path string) error {
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of %v", path, v, enum)
		}
	}
	switch s["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object", path)
		}
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, has := obj[fmt.Sprint(r)]; !has {
					return fmt.Errorf("%s: missing required %q", path, r)
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, pv := range obj {
			if ps, ok := props[k].(map[string]any); ok {
				if err := validateNode(pv, ps, path+"."+k); err != nil {
					return err
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array", path)
		}
		if is, ok := s["items"].(map[string]any); ok {
			for i, it := range arr {
				if err := validateNode(it, is, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: want a string", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: want a boolean", path)
		}
	case "integer", "number":
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("%s: want a number", path)
		}
		if s["type"] == "integer" && f != float64(int64(f)) {
			return fmt.Errorf("%s: want an integer", path)
		}
	}
	return nil
}

// --- the command runner (any CLI, unverified, wrapper-only) ---

type commandRunner struct{ argv []string }

func (commandRunner) Verified() bool       { return false }
func (commandRunner) HonorsSettings() bool { return false }

func (c commandRunner) Check(ctx context.Context, pathEnv string) error {
	if pathEnv == "" {
		pathEnv = os.Getenv("PATH")
	}
	if _, err := lookPathIn(c.argv[0], pathEnv); err != nil {
		return err
	}
	return nil
}

func (c commandRunner) Run(ctx context.Context, r AgentRun) AgentResult {
	prompt := r.Prompt
	if r.Schema != "" {
		prompt += schemaInstruction(r.Schema)
	}
	usesPrompt := false
	args := make([]string, 0, len(c.argv))
	for _, a := range c.argv {
		if strings.Contains(a, "{prompt}") {
			usesPrompt = true
		}
		args = append(args, strings.NewReplacer("{prompt}", prompt, "{dir}", r.Dir, "{model}", r.Model, "{effort}", r.Effort).Replace(a))
	}
	cctx, cancel := context.WithTimeout(ctx, orTimeout(r.Timeout))
	defer cancel()
	sub := r
	sub.Bin = args[0]
	name, argv := agentCommand(sub, args[1:])
	cmd := exec.CommandContext(cctx, name, argv...)
	containProcess(cmd) // a timeout stops the agent and every tool process it started
	cmd.Dir = r.Dir
	cmd.Env = withPWD(scrubEnv(r.Env, r.TokenEnvs, r.FileEnvKeys), r.Dir)
	if !usesPrompt {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stdout, stderr bytes.Buffer
	var flush func()
	cmd.Stdout, cmd.Stderr, flush = teeOutput(ctx, &stdout, &stderr, nil)
	err := settled(cmd, cmd.Run())
	flush()
	writeRunLog(r.LogFile, stdout.Bytes(), stderr.Bytes())
	res := AgentResult{Text: stdout.String()}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.Outcome, res.Detail = OutcomeTimeout, fmt.Sprintf("no result after %s", r.Timeout)
		return res
	}
	if err != nil {
		res.Outcome, res.Detail = classifyText(stdout.String()+"\n"+stderr.String(), nil, err)
		return res
	}
	if r.Schema != "" {
		obj, perr := extractStructured(res.Text, r.Schema)
		if perr != nil {
			res.Outcome, res.Detail = OutcomeError, "no valid structured result: "+perr.Error()
			return res
		}
		res.Structured = obj
	}
	res.Outcome = OutcomeOK
	return res
}

func orTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 90 * time.Minute
	}
	return d
}

// writeRunLog keeps the raw agent output per run (state dir, 0600).
func writeRunLog(path string, stdout, stderr []byte) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(logsDirOf(path), 0o700)
	_ = writeNoFollow(path, append(stdout, append([]byte("\n--- stderr\n"), stderr...)...), 0o600)
}

// runDetail names the model and effort for notes ("opus, effort high").
func runDetail(model, effort string) string {
	switch {
	case effort == "":
		return model
	case model == "":
		return "effort " + effort
	}
	return model + ", effort " + effort
}
