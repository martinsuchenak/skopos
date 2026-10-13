package worker

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

//go:embed prompts/*.md
var promptFS embed.FS

var prompts = template.Must(template.New("").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).ParseFS(promptFS, "prompts/*.md"))

const planSchema = `{"type":"object","properties":{"title":{"type":"string"},"summary":{"type":"string"},"size":{"type":"string","enum":["s","m","l"]},"needs_clarification":{"type":"boolean"},"steps":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}},"risks":{"type":"array","items":{"type":"string"}},"questions":{"type":"array","items":{"type":"string"}}},"required":["summary","size","needs_clarification","steps","risks","questions"]}`

const implSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["done","amendment","blocked"]},"steps_done":{"type":"array","items":{"type":"integer"}},"summary":{"type":"string"},"reason":{"type":"string"},"question":{"type":"string"},"amended_steps":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}}},"required":["status","steps_done","summary"]}`

// guardSettings: deny rules that apply to every agent run (git push/remote/
// config, forge CLIs, network fetch tools, destructive commands). Deny
// beats any allow.
var guardSettings = `{"permissions":{"deny":["Bash(git push:*)","Bash(git remote:*)","Bash(git config:*)","Bash(git branch:*)","Bash(git checkout:*)","Bash(git switch:*)","Bash(git reset --hard:*)","Bash(git rebase:*)","Bash(git worktree:*)","Bash(gh:*)","Bash(tea:*)","Bash(curl:*)","Bash(wget:*)","Bash(ssh:*)","Bash(scp:*)","Bash(rm -rf:*)","Bash(sudo:*)"]}}`

var planTools = []string{"Read", "Glob", "Grep"}

var implTools = []string{"Read", "Edit", "Write", "MultiEdit", "Glob", "Grep", "TodoWrite",
	"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)",
	"Bash(git show:*)", "Bash(git restore:*)", "Bash(ls:*)", "Bash(find:*)", "Bash(grep:*)", "Bash(rg:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(mkdir:*)"}

var reviewTools = []string{"Read", "Glob", "Grep", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)"}

// settingsJSON merges the hard guards with the deployment's MCP deny list.
func settingsJSON(mcpDeny []string) string {
	var g struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(guardSettings), &g)
	g.Permissions.Deny = append(g.Permissions.Deny, mcpDeny...)
	b, _ := json.Marshal(g)
	return string(b)
}

// Worker is the execution loop.
type Worker struct {
	cfg    *Config
	client *Client
	log    *slog.Logger
	// sessionState persists per-run agent session ids (stickiness: a
	// re-claimed run resumes its own session).
	sessions map[string]string
}

func New(cfg *Config, client *Client, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, client: client, log: log, sessions: map[string]string{}}
}

// Loop runs until the context is cancelled: claim, execute, report; pause on
// quota (affinity keeps the run); idle polls at cfg.PollEvery.
func (w *Worker) Loop(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		run, err := w.client.Claim(ctx, w.cfg.ID, w.cfg.Runners, w.cfg.Workspaces)
		if err != nil {
			w.log.Error("claim failed", "err", err)
			if !sleepCtx(ctx, w.cfg.PollEvery) {
				return ctx.Err()
			}
			continue
		}
		if run == nil {
			if !sleepCtx(ctx, w.cfg.PollEvery) {
				return ctx.Err()
			}
			continue
		}
		w.execute(ctx, run)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// execute runs one claimed stage to a terminal report or pause.
func (w *Worker) execute(ctx context.Context, run *Run) {
	log := w.log.With("run", run.ID, "item", run.ItemID, "stage", run.Stage)
	// Heartbeat while executing.
	hbCtx, hbStop := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := w.client.Heartbeat(hbCtx, run.ID, w.cfg.ID); err != nil {
					log.Warn("heartbeat failed", "err", err)
				}
			}
		}
	}()
	defer hbStop()

	var err error
	switch run.Stage {
	case "plan":
		err = w.stagePlan(ctx, run, log)
	case "implement":
		err = w.stageImplement(ctx, run, log)
	case "review", "qa":
		err = w.stageReview(ctx, run, log)
	default:
		err = fmt.Errorf("unknown stage %q", run.Stage)
	}
	if err == nil {
		return // the stage reported its own outcome
	}
	if q, ok := err.(*quotaError); ok {
		// Quota: pause (affinity keeps the run); the pool marks the
		// account exhausted until the reset.
		reason := "quota: " + q.detail
		_ = w.client.Pause(ctx, run.ID, w.cfg.ID, reason)
		log.Warn("quota pause", "until", q.resetAt, "detail", q.detail)
		return
	}
	_ = w.client.ReportResult(ctx, run.ID, w.cfg.ID, false, map[string]any{"reason": err.Error()})
	log.Error("stage failed", "err", err)
}

type quotaError struct {
	detail  string
	resetAt time.Time
	account *Account
}

func (e *quotaError) Error() string { return "quota: " + e.detail }

// pickAccount selects the credential for a NEW run on the stage's runner;
// resume runs reuse the account recorded with their session (stickiness).
func (w *Worker) pickAccount(run *Run, log *slog.Logger) *Account {
	runner := run.Runner
	if runner == "" {
		runner = "claude"
	}
	now := time.Now()
	a := w.cfg.AccountFor(runner, now)
	if a == nil {
		// Fall back to any account of any runner offered (single-runner
		// deployments have exactly one).
		for _, cand := range w.cfg.Accounts {
			if now.After(cand.exhaustedUntil) {
				a = cand
				break
			}
		}
	}
	return a
}

func (w *Worker) agentRun(bin, dir, prompt, schema string, tools []string, timeout time.Duration, account *Account, run *Run, phase string) AgentRun {
	sessionKey := run.ID + ":" + phase
	sess := w.sessions[sessionKey]
	return AgentRun{
		Bin: bin, Dir: dir, Prompt: prompt,
		SessionID: sess, Resume: sess != "",
		Schema: schema, AllowedTools: tools,
		Settings: settingsJSON(w.cfg.MCPDeny),
		Timeout:  timeout,
		LogFile:  filepath.Join(w.cfg.StateDir, "runs", run.ID, phase+".log"),
		Env:      account.Env,
	}
}

func (w *Worker) saveSession(run *Run, phase, sessionID string) {
	if sessionID == "" {
		return
	}
	w.sessions[run.ID+":"+phase] = sessionID
	// Persist lightly: sessions survive worker restarts for affinity runs.
	b, _ := json.Marshal(w.sessions)
	_ = os.MkdirAll(filepath.Dir(filepath.Join(w.cfg.StateDir, "sessions.json")), 0o700)
	_ = os.WriteFile(filepath.Join(w.cfg.StateDir, "sessions.json"), b, 0o600)
}

func (w *Worker) loadSessions() {
	b, err := os.ReadFile(filepath.Join(w.cfg.StateDir, "sessions.json"))
	if err == nil {
		_ = json.Unmarshal(b, &w.sessions)
	}
}

// classify converts an agent outcome into the worker's error protocol:
// nil = handled (reported), quotaError = pause, error = failed report.
func (w *Worker) handleAgentOutcome(ctx context.Context, run *Run, res AgentResult, account *Account, phase string) error {
	w.saveSession(run, phase, res.SessionID)
	switch res.Outcome {
	case OutcomeOK:
		return nil
	case OutcomeQuota:
		if account != nil {
			account.MarkExhausted(res.ResetAt, 30*time.Minute, time.Now())
		}
		return &quotaError{detail: res.Detail, resetAt: res.ResetAt, account: account}
	case OutcomeTransient:
		return fmt.Errorf("transient: %s", res.Detail)
	case OutcomeAuth:
		return fmt.Errorf("auth (fix the account login): %s", res.Detail)
	default:
		return fmt.Errorf("%s", res.Detail)
	}
}

func renderPrompt(name string, data any) (string, error) {
	var b strings.Builder
	if err := prompts.ExecuteTemplate(&b, "prompts/"+name, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Output string `json:"output,omitempty"`
}

// checksMarkdown renders checks for the report/PR body.
func checksMarkdown(checks []Check) string {
	if len(checks) == 0 {
		return "_No checks configured._"
	}
	var b strings.Builder
	for _, c := range checks {
		mark := "✅"
		if !c.Passed {
			mark = "❌"
		}
		fmt.Fprintf(&b, "- %s `%s`\n", mark, c.Name)
	}
	return b.String()
}
