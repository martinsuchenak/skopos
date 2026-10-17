package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/runs"
)

// --run-stage: one runner is today's routing; a list on review/qa makes a
// multi-agent stage; focus names are validated; lists on other stages keep
// the first runner with a warning. --stage-policy parses policy, early-exit
// and timeout.
func TestRunWorkspacePrefs(t *testing.T) {
	prefs, warnings := runWorkspacePrefs([]string{
		"github.com/you/repo:plan=claude",
		"github.com/you/repo:review=security@claude,tests@opencode-glm",
		"github.com/you/repo:qa=e2e@claude",
		"github.com/you/repo:implement=claude,opencode-glm",
		"github.com/you/repo:review=Bad Focus@claude,conv@qwen,perf@qwen",
		"git.example.com:2222/team/app:review=claude",
		"no-equals-sign",
	}, []string{
		"github.com/you/repo:review=majority,early-exit,timeout=45m",
		"github.com/you/repo:qa=sometimes",
	})
	p := prefs["github.com/you/repo"]
	if p.StageRunner("plan") != "claude" || p.GroupMembers("plan") != nil {
		t.Fatalf("plan stays single: %+v", p)
	}
	// The later review spec wins; its malformed member is dropped.
	rev := p.GroupMembers("review")
	if len(rev) != 2 || rev[0] != (runs.Member{Runner: "qwen", Focus: "conv"}) || p.StageRunner("review") != "" {
		t.Fatalf("review group: %+v stage=%q", rev, p.StageRunner("review"))
	}
	if p.GroupMembers("qa") != nil || p.StageRunner("qa") != "claude" || p.SingleFocus("qa") != "e2e" {
		t.Fatalf("single focused qa: %+v", p)
	}
	if p.StageRunner("implement") != "claude" || p.GroupMembers("implement") != nil {
		t.Fatalf("implement never fans out: %+v", p)
	}
	if !p.Configured("review") || !p.Configured("qa") {
		t.Fatal("review and qa are configured")
	}
	if prefs["git.example.com:2222/team/app"].StageRunner("review") != "claude" {
		t.Fatalf("workspace with a port: %+v", prefs)
	}
	sp := p.PolicyFor("review")
	if sp.Policy != runs.PolicyMajority || !sp.EarlyExit || sp.Timeout != 45*time.Minute {
		t.Fatalf("review policy: %+v", sp)
	}
	if d := p.PolicyFor("qa"); d.Policy != runs.PolicyAnyBlocks || d.EarlyExit || d.Timeout != runs.DefaultGroupTimeout {
		t.Fatalf("bad policy must fall back to defaults: %+v", d)
	}
	all := strings.Join(warnings, "\n")
	for _, want := range []string{"only plan, review and qa run several agents", `ignoring member "Bad Focus@claude"`, `ignoring --run-stage "no-equals-sign"`, `ignoring --stage-policy "github.com/you/repo:qa=sometimes"`} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings missing %q:\n%s", want, all)
		}
	}
}
