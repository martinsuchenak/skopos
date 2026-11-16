package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/martinsuchenak/skopos/internal/srvurl"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// The stage check: before the worker claims a workspace's work it tries
// what the workspace's runs will need on this host, so a problem shows on
// the Workers page with the reason instead of as a failed run. It checks
// that Claude Code's sandbox can start (bubblewrap, socat, /proc on Linux),
// that the commands a stage is given are on the PATH runs get, that the
// kits' cache folders exist (it creates them: the sandbox opens only a
// folder that exists), and that the remote takes the push token.
//
// A failure holds back only what it concerns: the stage it was found for
// (the sandbox, a command, a folder) or the implement stage (the push
// token). The worker reports that stage's executor as none:check, with the
// reason, and takes the workspace's other runs as before.

const (
	// stageCheckEvery: how often a passing check runs again.
	stageCheckEvery = 6 * time.Hour
	// stageRecheckFailed: how often a failing one does, so a fix shows soon.
	stageRecheckFailed = 2 * time.Minute
	stageCheckTimeout  = 30 * time.Second
	// maxCheckReason bounds a reason the worker reports.
	maxCheckReason = 400
)

// stageCheck is a workspace's last stage check.
type stageCheck struct {
	key string
	at  time.Time
	// stages: why each stage cannot run here; push: why the implement
	// stage's push would be refused ("" = fine).
	stages map[string]string
	push   string
}

func (r stageCheck) failing() bool { return len(r.stages) > 0 || r.push != "" }

// checkStages runs the workspace's stage check when it is due and records
// what it found for ExecutorFor. It reports whether that changed.
func (w *Worker) checkStages(ctx context.Context, ws string) (changed bool) {
	plan := w.stageCheckPlan(ws)
	if w.stageChecks == nil {
		w.stageChecks = map[string]stageCheck{}
	}
	last, seen := w.stageChecks[ws]
	if plan.empty() {
		delete(w.stageChecks, ws)
		return w.cfg.setCheckResult(ws, nil, "")
	}
	now := time.Now()
	due := !seen || last.key != plan.key ||
		(last.failing() && now.Sub(last.at) >= stageRecheckFailed) ||
		now.Sub(last.at) >= stageCheckEvery
	if !due {
		return false
	}
	res := w.runStageCheck(ctx, plan)
	if ctx.Err() != nil {
		return false // shutting down: not a verdict
	}
	res.key, res.at = plan.key, now
	for _, stage := range wsconfig.StageNames {
		was, why := last.stages[stage], res.stages[stage]
		switch {
		case why != "" && why != was:
			w.log.Warn("stage check failed: not taking this stage's runs of the workspace until it passes", "workspace", ws, "stage", stage, "reason", why)
		case why == "" && was != "":
			w.log.Info("stage check passes again", "workspace", ws, "stage", stage)
		}
	}
	switch {
	case res.push != "" && res.push != last.push:
		w.log.Warn("stage check failed: not taking this workspace's implement runs until it passes", "workspace", ws, "reason", res.push)
	case res.push == "" && last.push != "":
		w.log.Info("the push token check passes again", "workspace", ws)
	}
	w.stageChecks[ws] = res
	return w.cfg.setCheckResult(ws, res.stages, res.push)
}

// setCheckResult records a workspace's stage check for ExecutorFor and
// reports whether it changed.
func (c *Config) setCheckResult(ws string, stages map[string]string, push string) bool {
	before := c.checkBad[ws]
	var after map[string]string
	for stage, why := range stages {
		if why == "" {
			continue
		}
		if after == nil {
			after = map[string]string{}
		}
		after[stage] = why
	}
	if push != "" {
		if after == nil {
			after = map[string]string{}
		}
		after[checkPush] = push
	}
	if maps.Equal(before, after) {
		return false
	}
	if c.checkBad == nil {
		c.checkBad = map[string]map[string]string{}
	}
	if after == nil {
		delete(c.checkBad, ws)
	} else {
		c.checkBad[ws] = after
	}
	return true
}

// checkPush is the key of the push token's result in Config.checkBad.
const checkPush = "push"

// checkProblem says why a run of stage (a run stage) with the resolved
// profile may not start on this worker after the stage check ("" = it
// may): the stage's own result where the profile is sandboxed or given
// something, and the push token's for an implementation.
func (c *Config) checkProblem(ws, stage string, prof *StageProfile) string {
	bad := c.checkBad[ws]
	if len(bad) == 0 {
		return ""
	}
	if !prof.wraps() && (prof.sandboxed() || prof.grants()) {
		if why := bad[stagePhase(stage)]; why != "" {
			return why
		}
	}
	if stage == "implement" {
		return bad[checkPush]
	}
	return ""
}

// checkPlan is what the stage check of a workspace tries.
type checkPlan struct {
	key string
	// stages: per stage the check covers, what it needs.
	stages map[string]*stageNeeds
	// pushKind, pushURL, tokenEnv: the workspace's push ("" = none).
	pushKind, pushURL, tokenEnv string
	prefix                      string
	repoPath                    string
}

// stageNeeds is what one stage's runs need on this host.
type stageNeeds struct {
	Sandboxed bool     `json:"sandboxed"`
	Commands  []string `json:"commands"`
	Write     []string `json:"write"`
	// KitProblem: a kit's folder that could not be worked out.
	KitProblem string `json:"kit_problem"`
}

func (p checkPlan) empty() bool { return len(p.stages) == 0 && p.pushKind == "" }

// stageCheckPlan works out what to check for ws from what its runs would get.
func (w *Worker) stageCheckPlan(ws string) checkPlan {
	plan := checkPlan{stages: map[string]*stageNeeds{}}
	blocks := w.cfg.srvStages[ws]
	for _, stage := range wsconfig.StageNames {
		prof := w.cfg.StageProfile(ws, stage)
		// Inside a wrapper the commands are the wrapper's to provide.
		if prof == nil || prof.wraps() || (!prof.sandboxed() && !prof.grants()) {
			continue
		}
		need := &stageNeeds{Sandboxed: prof.sandboxed()}
		add := func(list *[]string, v string) {
			if v != "" && !slices.Contains(*list, v) {
				*list = append(*list, v)
			}
		}
		var kitTools []string
		if b := blocks[stage]; b != nil && len(b.Kits) > 0 {
			k := w.cfg.kitParts(b.Kits)
			kitTools = k.Tools
			for _, c := range k.Commands {
				add(&need.Commands, c)
			}
			if need.Sandboxed {
				for _, d := range k.Write {
					add(&need.Write, d)
				}
				need.KitProblem = k.problem
			}
		}
		// The block's own Bash grants: the program each one runs.
		for _, t := range prof.Tools {
			if !slices.Contains(kitTools, t) {
				add(&need.Commands, bashProgram(t))
			}
		}
		slices.Sort(need.Commands)
		plan.stages[stage] = need
	}
	if repo := w.repo(ws); repo != nil && repo.Push.Kind != "" && repo.Push.Kind != "none" {
		plan.pushKind, plan.pushURL, plan.tokenEnv = repo.Push.Kind, repo.Push.RemoteURL, repo.Push.TokenEnv
		plan.prefix, plan.repoPath = repo.BranchPrefix, repo.Path
	}
	// What the check depends on: when it changes the check runs again.
	keyParts := map[string]any{"stages": plan.stages, "nested": w.cfg.SandboxNested, "path": os.Getenv("PATH")}
	if plan.pushKind != "" {
		keyParts["push"] = []string{plan.pushKind, plan.pushURL, plan.tokenEnv, tokenFingerprint(os.Getenv(plan.tokenEnv))}
	}
	buf, _ := json.Marshal(keyParts)
	sum := sha256.Sum256(buf)
	plan.key = hex.EncodeToString(sum[:8])
	return plan
}

// tokenFingerprint changes when the token does, without holding it.
func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}

// bashProgram is the program a Bash(…) tool pattern runs: its first word,
// when that is a program name looked up on the PATH ("" otherwise: a
// path, a glob, or not a Bash pattern).
func bashProgram(tool string) string {
	inner, ok := strings.CutPrefix(tool, "Bash(")
	if !ok || !strings.HasSuffix(inner, ")") {
		return ""
	}
	inner = strings.TrimSuffix(inner, ")")
	inner, _, _ = strings.Cut(inner, ":")
	fields := strings.Fields(inner)
	if len(fields) == 0 {
		return ""
	}
	prog := fields[0]
	if strings.ContainsAny(prog, "/*?[=$") {
		return ""
	}
	return prog
}

// runStageCheck runs the checks of plan: what fails, per stage, and for the
// push.
func (w *Worker) runStageCheck(ctx context.Context, plan checkPlan) stageCheck {
	res := stageCheck{stages: map[string]string{}}
	runtimeWhy := ""
	for _, need := range plan.stages {
		if need.Sandboxed {
			runtimeWhy = sandboxRuntimeProblem(ctx, w.cfg.SandboxNested)
			break
		}
	}
	for stage, need := range plan.stages {
		var reasons []string
		if need.Sandboxed && runtimeWhy != "" {
			reasons = append(reasons, runtimeWhy)
		}
		if need.KitProblem != "" {
			reasons = append(reasons, need.KitProblem)
		}
		var missing []string
		for _, c := range need.Commands {
			if _, err := exec.LookPath(c); err != nil {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			reasons = append(reasons, fmt.Sprintf("not on the PATH runs get: %s — install them, or link them into a folder on the PATH of the worker's service (a grant matches the command's first word, so a program called by its full path is refused)", strings.Join(missing, ", ")))
		}
		for _, d := range need.Write {
			if err := os.MkdirAll(d, 0o755); err != nil {
				reasons = append(reasons, fmt.Sprintf("the cache folder %s cannot be created: %v", d, err))
			}
		}
		if len(reasons) > 0 {
			res.stages[stage] = clip(w.redacted(strings.Join(reasons, "; ")), maxCheckReason)
		}
	}
	if plan.pushKind != "" {
		res.push = clip(w.redacted(pushTokenProblem(ctx, plan)), maxCheckReason)
	}
	return res
}

// redacted is s with every secret the worker knows taken out.
func (w *Worker) redacted(s string) string {
	if r := w.redactor(); r != nil {
		return r.Replace(s)
	}
	return s
}

// clip bounds s to n bytes on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// sandboxRuntime caches what the sandbox's runtime check found on this
// host, per mode.
var sandboxRuntime struct {
	mu   sync.Mutex
	seen map[bool]runtimeCheck
}

type runtimeCheck struct {
	at     time.Time
	reason string
}

// sandboxRuntimeProblem says why Claude Code's sandbox cannot start on this
// host ("" = it can, as far as can be told without starting the agent). On
// macOS the sandbox is the system's own; on Linux it needs bubblewrap and
// socat, and bubblewrap must be able to mount /proc unless nested.
func sandboxRuntimeProblem(ctx context.Context, nested bool) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	sandboxRuntime.mu.Lock()
	defer sandboxRuntime.mu.Unlock()
	if last, ok := sandboxRuntime.seen[nested]; ok {
		limit := stageCheckEvery
		if last.reason != "" {
			limit = stageRecheckFailed
		}
		if time.Since(last.at) < limit {
			return last.reason
		}
	}
	why := linuxSandboxProblem(ctx, nested)
	if ctx.Err() != nil {
		return why
	}
	if sandboxRuntime.seen == nil {
		sandboxRuntime.seen = map[bool]runtimeCheck{}
	}
	sandboxRuntime.seen[nested] = runtimeCheck{at: time.Now(), reason: why}
	return why
}

func linuxSandboxProblem(ctx context.Context, nested bool) string {
	var missing []string
	for _, p := range []string{"bwrap", "socat"} {
		if _, err := exec.LookPath(p); err != nil {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("Claude Code's sandbox needs %s, which this host does not have (on Debian and Ubuntu: apt-get install bubblewrap socat)", strings.Join(missing, " and "))
	}
	args := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--unshare-pid"}
	if nested {
		args = append(args, "--bind", "/proc", "/proc")
	} else {
		args = append(args, "--proc", "/proc")
	}
	args = append(args, "true")
	cctx, cancel := context.WithTimeout(ctx, stageCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bwrap", args...)
	containProcess(cmd)
	out, err := cmd.CombinedOutput()
	if err = settled(cmd, err); err == nil {
		return ""
	}
	text := strings.TrimSpace(string(out))
	// bubblewrap's own words for the refused mount: "Can't mount proc on
	// /newroot/proc: Operation not permitted".
	if !nested && strings.Contains(text, "mount proc") {
		return "Claude Code's sandbox cannot start here: bubblewrap cannot mount /proc (" + tail(text, 200) + "), which an unprivileged container refuses. Set sandbox_nested = true under [worker] in this worker's file to use the sandbox's nested mode (weaker: commands in the sandbox see the host's processes), then restart the worker"
	}
	return fmt.Sprintf("Claude Code's sandbox cannot start here: bubblewrap failed: %v: %s", err, tail(text, 300))
}

// pushTokenProblem says why the remote would refuse the run's push ("" =
// it takes the token, or the check could not tell). It asks the remote to
// receive a push without sending one (git push --dry-run): a token that may
// not write is refused there.
func pushTokenProblem(ctx context.Context, plan checkPlan) string {
	token := os.Getenv(plan.tokenEnv)
	if plan.tokenEnv == "" || token == "" {
		return fmt.Sprintf("the push token's variable %s is empty on this worker: an implementation could not be pushed", plan.tokenEnv)
	}
	if fi, err := os.Stat(plan.repoPath); plan.repoPath == "" || err != nil || !fi.IsDir() {
		return "" // no clone yet: the worker's own checks say so
	}
	cctx, cancel := context.WithTimeout(ctx, stageCheckTimeout)
	defer cancel()
	remote, err := srvurl.Resolve(cctx, plan.pushURL)
	if err != nil {
		return "" // unreachable now: not the token's fault
	}
	_, err = gitEnv(cctx, plan.repoPath, []string{
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + pushAuthHeader(plan.pushKind, token),
		"GIT_CONFIG_KEY_1=http.sslVerify", "GIT_CONFIG_VALUE_1=true",
		"GIT_CONFIG_KEY_2=core.hooksPath", "GIT_CONFIG_VALUE_2=/dev/null",
	}, "push", "--dry-run", "--porcelain", remote, "HEAD:refs/heads/"+plan.prefix+"skopos-push-check")
	if pushAuthRefused(err) {
		return fmt.Sprintf("the remote refuses the push token (%s) for %s: it is wrong, expired, or may not write to the repository (%s)", plan.tokenEnv, plan.pushURL, tail(err.Error(), 200))
	}
	return ""
}
