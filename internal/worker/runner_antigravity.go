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
	"strings"
	"time"
)

// antigravityRunner is the Antigravity CLI (`agy --print`, Gemini's route
// now that the Gemini CLI is decommissioned). It is UNVERIFIED, so it runs
// only on wrapped stages — the spike against agy 1.1.25 (tmp/second-opinion-
// review — design.md) found:
//   - instruction files: agy always loads the working directory's AGENTS.md
//     and GEMINI.md and has no switch to skip them (review worktrees reset
//     them to the base version, as for every runner);
//   - headless mode auto-denies every tool that would need a permission
//     prompt — reads outside the workspace, writes, any shell command
//     without an allow rule — but the first denial ENDS the run with no
//     output, and the allow rules live only in
//     $HOME/.gemini/antigravity-cli/settings.json;
//   - so the stage allowlist has to be written into the account's login
//     HOME, and that the --sandbox confines allowed commands is unverified.
//
// What the adapter does: HOME = the account's own login directory (agy keeps
// its sign-in, settings and conversations under ~/.gemini; there is no
// variable to move them), the stage allowlist as permissions.allow rules in
// that directory's settings.json, --sandbox, --disable-slash-commands (no
// skill expansion), --json-schema (native structured output), --conversation
// to resume, and --print-timeout = the stage timeout (agy's default is 5m).
// Never --dangerously-skip-permissions.
type antigravityRunner struct{ bin string }

func (antigravityRunner) Verified() bool       { return false }
func (antigravityRunner) HonorsSettings() bool { return false }

func (a antigravityRunner) Check(ctx context.Context, pathEnv string) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, a.bin, "--help")
	if pathEnv != "" {
		cmd.Env = append(os.Environ(), "PATH="+pathEnv)
		if p, err := lookPathIn(a.bin, pathEnv); err == nil {
			cmd.Path = p
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return fmt.Errorf("running %s --help: %v", a.bin, err)
	}
	var missing []string
	for _, f := range []string{"--print", "--output-format", "--json-schema", "--conversation", "--print-timeout", "--sandbox", "--disable-slash-commands"} {
		if !strings.Contains(string(out), f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s does not support %s — upgrade the Antigravity CLI", a.bin, strings.Join(missing, ", "))
	}
	return nil
}

// antigravityHomeProblem: an antigravity account needs a login directory of
// its own as HOME (skopos writes the run's allow rules into it, and never
// into the worker user's ~/.gemini), without user-level instructions.
func antigravityHomeProblem(env map[string]string) string {
	home := env["HOME"]
	if home == "" {
		return "an antigravity account needs env HOME = a login directory of its own (HOME=<dir> agy, then sign in) — skopos writes each run's allow rules into it"
	}
	home = expandHome(home, userHome())
	for _, f := range []string{"GEMINI.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(home, ".gemini", f)); err == nil {
			return fmt.Sprintf("%s has .gemini/%s, which agy loads into every run — remove it from the login directory", home, f)
		}
	}
	return ""
}

// antigravityPermissions maps the stage allowlist (Claude-style names) onto
// agy's permission rules. Reads inside the workspace need no rule; edits
// become write_file(<worktree>); Bash patterns become command(<prefix>);
// the hard denies stay denied.
func antigravityPermissions(dir string, allowed, denied []string) (allow, deny []string) {
	seen := map[string]bool{}
	add := func(list *[]string, rule string) {
		if !seen[rule] {
			seen[rule] = true
			*list = append(*list, rule)
		}
	}
	for _, a := range allowed {
		name, pattern, hasPattern := strings.Cut(strings.TrimSpace(a), "(")
		pattern = strings.TrimSuffix(pattern, ")")
		switch {
		case name == "Edit" || name == "Write" || name == "MultiEdit":
			add(&allow, "write_file("+dir+")")
		case name == "Bash" && !hasPattern:
			add(&allow, "command(*)")
		case name == "Bash":
			add(&allow, "command("+strings.TrimSuffix(strings.TrimSuffix(pattern, ":*"), "*")+")")
		}
	}
	for _, d := range denied {
		if n, p, ok := strings.Cut(d, "("); ok && n == "Bash" {
			add(&deny, "command("+strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(p, ")"), ":*"), "*")+")")
		}
	}
	return allow, deny
}

// writeAntigravitySettings sets permissions in the login directory's
// settings.json, keeping what agy itself stored there (model, trust).
func writeAntigravitySettings(home string, allow, deny []string) error {
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
	}
	if allow == nil {
		allow = []string{}
	}
	if deny == nil {
		deny = []string{}
	}
	doc["permissions"] = map[string]any{"allow": allow, "deny": deny}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

type antigravityJSON struct {
	ConversationID   string          `json:"conversation_id"`
	Status           string          `json:"status"`
	Response         string          `json:"response"`
	Error            string          `json:"error"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	DeniedActions    []struct {
		Action      string `json:"action"`
		DisplayName string `json:"display_name"`
	} `json:"denied_actions"`
}

func (a antigravityRunner) Run(ctx context.Context, r AgentRun) AgentResult {
	if p := antigravityHomeProblem(r.Env); p != "" {
		return AgentResult{Outcome: OutcomeError, Detail: p}
	}
	home := expandHome(r.Env["HOME"], userHome())
	allow, deny := antigravityPermissions(r.Dir, r.AllowedTools, settingsDeny(r.Settings))
	if err := writeAntigravitySettings(home, allow, deny); err != nil {
		return AgentResult{Outcome: OutcomeError, Detail: "agy settings: " + err.Error()}
	}
	timeout := orTimeout(r.Timeout)
	args := []string{"--output-format", "json", "--sandbox", "--disable-slash-commands",
		"--print-timeout", timeout.String()}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Schema != "" {
		tmp, err := os.MkdirTemp("", "skopos-agy-")
		if err != nil {
			return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		path := filepath.Join(tmp, "schema.json")
		if err := os.WriteFile(path, []byte(r.Schema), 0o600); err != nil {
			return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
		}
		args = append(args, "--json-schema", path)
	}
	if r.Resume && r.SessionID != "" {
		args = append(args, "--conversation", r.SessionID)
	}
	// agy reads the prompt only from --print (stdin "-" is taken literally);
	// attached with = so a prompt starting with "-" can't be read as a flag.
	args = append(args, "--print="+r.Prompt)

	// The context outlives --print-timeout a little, so agy reports its own
	// timeout rather than being killed mid-write.
	cctx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
	defer cancel()
	sub := r
	sub.Bin = a.bin
	name, argv := agentCommand(sub, args)
	cmd := exec.CommandContext(cctx, name, argv...)
	cmd.Dir = r.Dir
	cmd.Env = withPWD(scrubEnv(r.Env, r.TokenEnvs, r.FileEnvKeys), r.Dir) // HOME = the login directory
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	writeRunLog(r.LogFile, stdout.Bytes(), stderr.Bytes())
	return parseAntigravity(stdout.Bytes(), stderr.String(), runErr, cctx.Err(), r, time.Now())
}

func parseAntigravity(stdout []byte, stderr string, runErr, ctxErr error, r AgentRun, now time.Time) AgentResult {
	res := AgentResult{SessionID: r.SessionID}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		res.Outcome, res.Detail = OutcomeTimeout, fmt.Sprintf("no result after %s", r.Timeout)
		return res
	}
	var out antigravityJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &out); err != nil {
		text := string(stdout) + "\n" + stderr
		res.Outcome, res.Detail = classifyText(text, nil, runErr)
		res.ResetAt = parseReset(text, now)
		return res
	}
	if out.ConversationID != "" {
		res.SessionID = out.ConversationID
	}
	res.Text = out.Response
	if out.Status != "SUCCESS" {
		text := strings.TrimSpace(out.Error + "\n" + out.Response + "\n" + stderr)
		res.Outcome, res.Detail = classifyText(text, nil, runErr)
		if res.Outcome == OutcomeError && strings.Contains(strings.ToLower(text), "timed out") {
			res.Outcome = OutcomeTimeout
		}
		res.ResetAt = parseReset(text, now)
		return res
	}
	structured := len(out.StructuredOutput) > 0 && string(out.StructuredOutput) != "null"
	if len(out.DeniedActions) > 0 && !structured && strings.TrimSpace(out.Response) == "" {
		var names []string
		for _, d := range out.DeniedActions {
			names = append(names, d.DisplayName+" ("+d.Action+")")
		}
		res.Outcome, res.Detail = OutcomeError, "agy stopped at a tool this stage doesn't allow: "+strings.Join(names, ", ")+
			" — headless agy ends the run at the first denied tool"
		return res
	}
	if r.Schema == "" {
		res.Outcome = OutcomeOK
		return res
	}
	if structured {
		var v any
		if err := json.Unmarshal(out.StructuredOutput, &v); err == nil {
			if err := validateSchema(v, r.Schema); err == nil {
				res.Outcome, res.Structured = OutcomeOK, out.StructuredOutput
				return res
			}
		}
	}
	obj, err := extractStructured(out.Response, r.Schema)
	if err != nil {
		res.Outcome, res.Detail = OutcomeError, "no valid structured result: "+err.Error()
		return res
	}
	res.Outcome, res.Structured = OutcomeOK, obj
	return res
}
