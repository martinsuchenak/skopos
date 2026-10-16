package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// probeWord is what a probe asks the agent to reply with.
const probeWord = "SKOPOS-PROBE-OK"

// ProbeResult is one account's smoke run.
type ProbeResult struct {
	Account  string
	Runner   string
	Model    string
	Executor string
	Duration time.Duration
	Reply    string
	Err      error
}

// ProbeAccount runs one tiny, read-only prompt through the account's runner
// with the full lockdown — the same AgentRun shape a plan stage gets — in a
// throwaway git repo. It proves, in seconds, what otherwise fails at the
// first real item: the CLI starts with the lockdown, the auth works, the
// model id exists and the provider answers.
//
// Unverified (command) runners are probed directly, without the wrapper
// containment rule: the operator asked for it, and the prompt only asks for
// a reply.
func (c *Config) ProbeAccount(ctx context.Context, a *Account, timeout time.Duration) ProbeResult {
	runner := accountRunner(a)
	res := ProbeResult{Account: a.Name, Runner: runner, Model: a.Model}
	r, err := c.RunnerFor(runner)
	if err != nil {
		res.Err = err
		return res
	}
	dir, err := os.MkdirTemp("", "skopos-probe-")
	if err != nil {
		res.Err = err
		return res
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if out, err := exec.CommandContext(ctx, "git", "init", "-q", dir).CombinedOutput(); err != nil {
		res.Err = fmt.Errorf("git init: %v: %s", err, out)
		return res
	}
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("probe\n"), 0o644)

	ar := AgentRun{
		Bin: c.RunnerBin(runner), Dir: dir, Runner: runner, Model: a.Model,
		Prompt:       "This is a connectivity check. Do not use any tools. Reply with exactly: " + probeWord,
		AllowedTools: basePlanTools,
		Settings:     settingsJSON(c.MCPDeny, ""),
		Env:          a.Env,
		TokenEnvs:    c.TokenEnvs(),
		Timeout:      timeout,
	}
	res.Executor = "local"
	start := time.Now()
	out := r.Run(ctx, ar)
	res.Duration = time.Since(start)
	res.Reply = strings.TrimSpace(out.Text)
	switch {
	case out.Outcome != OutcomeOK:
		res.Err = fmt.Errorf("%s: %s", out.Outcome, firstLine(out.Detail))
	case !strings.Contains(out.Text, probeWord):
		res.Err = fmt.Errorf("the agent answered, but not with %s: %q", probeWord, firstLine(out.Text))
	}
	return res
}
