package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Runners: the worker is agent-CLI agnostic. A Runner turns one AgentRun
// (prompt, allowed tools, schema, session, env, wrapper) into one agent
// invocation and its AgentResult. Built in: "claude" and "opencode", both
// with a lockdown skopos has verified live (instruction files not loaded,
// files outside the worktree unreadable, the tool allowlist enforced —
// TestLive*). "command" runs any CLI from an argv template; skopos cannot
// vouch for an arbitrary CLI, so it is UNVERIFIED and only ever runs on a
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

// RunnerSpec configures a runner name ([worker.runner_config.<name>]).
// kind defaults to the name; "claude" and "opencode" need no block.
type RunnerSpec struct {
	Kind string `toml:"kind"` // claude | opencode | command
	Bin  string `toml:"bin"`  // the CLI binary (claude/opencode)
	// Config (opencode): a base opencode.json for runs — providers and
	// models (secrets via {env:VAR}, injected through account env). The
	// worker user's ~/.config/opencode is never read.
	Config string `toml:"config"`
	// Command (command): the argv template. {prompt} {dir} {model}
	// expand; without {prompt} the prompt goes to stdin.
	Command []string `toml:"command"`
}

var runnerKinds = map[string]bool{"claude": true, "opencode": true, "command": true}

func (c *Config) runnerSpec(name string) (*RunnerSpec, string) {
	if name == "" {
		name = "claude"
	}
	spec := c.RunnerSpecs[name]
	if spec == nil {
		spec = &RunnerSpec{}
	}
	kind := spec.Kind
	if kind == "" {
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
	case "command":
		if len(spec.Command) == 0 {
			return nil, fmt.Errorf("runner %q: kind command needs command = [...]", name)
		}
		return commandRunner{argv: spec.Command}, nil
	}
	return nil, fmt.Errorf("unknown runner %q (built in: claude, opencode; or define [worker.runner_config.%s] with kind = \"command\")", name, name)
}

// RunnerBin is the binary a runner name executes (for checks and logs).
func (c *Config) RunnerBin(runner string) string {
	spec, kind := c.runnerSpec(runner)
	switch {
	case spec.Bin != "":
		return spec.Bin
	case kind == "claude":
		return c.Bin
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
		out = []string{"claude"}
	}
	sort.Strings(out)
	return out
}

// CheckRunners validates every used runner's CLI on pathEnv.
func (c *Config) CheckRunners(ctx context.Context, pathEnv string) []string {
	var problems []string
	for _, name := range c.UsedRunners() {
		r, err := c.RunnerFor(name)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if err := r.Check(ctx, pathEnv); err != nil {
			problems = append(problems, fmt.Sprintf("runner %s: %v", name, err))
		}
	}
	return problems
}

// runAgent executes one agent run on its runner — fail closed: a runner
// skopos has not verified, or a stage granted capabilities, only runs when
// something contains it (a wrapper, or settings the runner honors).
func (w *Worker) runAgent(ctx context.Context, ar AgentRun) AgentResult {
	r, err := w.cfg.RunnerFor(ar.Runner)
	if err != nil {
		return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
	}
	contained := len(ar.Wrapper) > 0 || (ar.Sandboxed && r.HonorsSettings())
	if !contained {
		if !r.Verified() {
			return AgentResult{Outcome: OutcomeError, Detail: fmt.Sprintf(
				"runner %q has no lockdown skopos has verified — it only runs on stages contained by a wrapper (worker.profiles.<name>.<stage>.wrapper)", ar.Runner)}
		}
		if ar.Granted {
			return AgentResult{Outcome: OutcomeError, Detail: fmt.Sprintf(
				"stage grants tools/env but is not contained for runner %q (sandbox settings only contain the claude runner — use a wrapper)", ar.Runner)}
		}
	}
	return r.Run(ctx, ar)
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
		args = append(args, strings.NewReplacer("{prompt}", prompt, "{dir}", r.Dir, "{model}", r.Model).Replace(a))
	}
	cctx, cancel := context.WithTimeout(ctx, orTimeout(r.Timeout))
	defer cancel()
	sub := r
	sub.Bin = args[0]
	name, argv := agentCommand(sub, args[1:])
	cmd := exec.CommandContext(cctx, name, argv...)
	cmd.Dir = r.Dir
	cmd.Env = withPWD(scrubEnv(r.Env, r.TokenEnvs, r.FileEnvKeys), r.Dir)
	if !usesPrompt {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
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
	_ = os.WriteFile(path, append(stdout, append([]byte("\n--- stderr\n"), stderr...)...), 0o600)
}
