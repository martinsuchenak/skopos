package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookTestHome builds a fake HOME with a stub skopos + git on PATH so the
// hook scripts run their real logic against controlled outputs.
type hookTestEnv struct {
	home    string
	binDir  string
	project string
}

func newHookEnv(t *testing.T) *hookTestEnv {
	t.Helper()
	env := &hookTestEnv{home: t.TempDir(), binDir: t.TempDir(), project: t.TempDir()}
	os.MkdirAll(filepath.Join(env.home, ".claude", "hooks"), 0o755)
	os.MkdirAll(env.binDir, 0o755)
	// Stub skopos: index status succeeds (in-project); search returns a hit.
	os.WriteFile(filepath.Join(env.binDir, "skopos"), []byte(`#!/bin/sh
case "$1" in
  index) echo "main  10 files  20 symbols"; exit 0;;
  search) echo '{"hits":[{"name":"LoadConfig","qualified":"Auth::LoadConfig","kind":"func","path":"auth.go","line":42}]}'; exit 0;;
  *) exit 0;;
esac
`), 0o755)
	os.WriteFile(filepath.Join(env.project, ".git", "HEAD"), []byte("ref: refs/heads/feat/x"), 0o644)
	// Prepend stubs to PATH via env when invoking scripts.
	return env
}

func (e *hookTestEnv) runHook(t *testing.T, script, stdin string) string {
	t.Helper()
	// Install the scripts into the fake hooks dir first.
	for name, src := range hookScripts {
		os.WriteFile(filepath.Join(e.home, ".claude", "hooks", name), []byte(src), 0o755)
	}
	out, err := runScript(t, e, script, stdin)
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return out
}

func runScript(t *testing.T, e *hookTestEnv, script, stdin string) (string, error) {
	t.Helper()
	return runWithEnv(t,
		filepath.Join(e.home, ".claude", "hooks", script),
		stdin,
		[]string{"PATH=" + e.binDir + ":" + os.Getenv("PATH"), "HOME=" + e.home, "TMPDIR=" + e.home},
		e.project,
	)
}

func TestHooksSyntaxValid(t *testing.T) {
	for name := range hookScripts {
		if name == "skopos-common.sh" {
			continue
		}
		out, err := runWithEnv(t, "bash -n "+filepath.Join("assets", "hooks", name), "", nil, ".")
		if err != nil {
			t.Errorf("%s: syntax error: %v\n%s", name, err, out)
		}
	}
}

func TestHookSessionEmitsBriefing(t *testing.T) {
	env := newHookEnv(t)
	out := env.runHook(t, "skopos-session.sh", `{}`)
	if !strings.Contains(out, "skopos_context") || !strings.Contains(out, "main  10 files") {
		t.Fatalf("session briefing missing content:\n%s", out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
}

func TestHookPromptPrefetchesSymbols(t *testing.T) {
	env := newHookEnv(t)
	out := env.runHook(t, "skopos-prompt.sh", `{"user_prompt":"how does LoadConfig work in the auth flow","num_turns":3}`)
	if !strings.Contains(out, "Relevant symbols") {
		t.Fatalf("expected code pre-fetch:\n%s", out)
	}
}

func TestHookPromptCheckpointEvery10Turns(t *testing.T) {
	env := newHookEnv(t)
	out := env.runHook(t, "skopos-prompt.sh", `{"user_prompt":"please continue with the implementation","num_turns":10}`)
	if !strings.Contains(out, "checkpoint") {
		t.Fatalf("expected checkpoint reminder:\n%s", out)
	}
}

func TestHookPreToolNudgesSymbolGrep(t *testing.T) {
	env := newHookEnv(t)
	out := env.runHook(t, "skopos-pre-tool.sh", `{"tool_name":"Grep","tool_input":{"pattern":"LoadConfig"}}`)
	if !strings.Contains(out, "who-calls") {
		t.Fatalf("expected grep nudge:\n%s", out)
	}
	// Literal-string greps are legitimately grep's job — no nudge.
	out = env.runHook(t, "skopos-pre-tool.sh", `{"tool_name":"Grep","tool_input":{"pattern":"TODO: fix"}}`)
	if out != "" {
		t.Fatalf("literal grep must not be nudged:\n%s", out)
	}
}

func TestHookPostToolRemindsToRecord(t *testing.T) {
	env := newHookEnv(t)
	out := env.runHook(t, "skopos-post-tool.sh", `{"tool_name":"Edit","tool_input":{"file_path":"/x/svc.go"}}`)
	if !strings.Contains(out, "blackboard_write") {
		t.Fatalf("expected memory reminder:\n%s", out)
	}
}

func TestHookStopExtractsKnowledge(t *testing.T) {
	env := newHookEnv(t)
	out := env.runHook(t, "skopos-stop.sh", `{"num_turns":8}`)
	if !strings.Contains(out, "ARCHITECTURAL DECISIONS") {
		t.Fatalf("expected extraction categories:\n%s", out)
	}
}

func TestHookSilentWhenSkoposUnavailable(t *testing.T) {
	env := newHookEnv(t)
	for name, src := range hookScripts {
		os.WriteFile(filepath.Join(env.home, ".claude", "hooks", name), []byte(src), 0o755)
	}
	// Replace the stub with one that fails (not in a skopos project).
	os.WriteFile(filepath.Join(env.binDir, "skopos"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	out, err := runWithEnv(t, filepath.Join(env.home, ".claude", "hooks", "skopos-session.sh"), `{}`,
		[]string{"PATH=" + env.binDir + ":" + os.Getenv("PATH"), "HOME=" + env.home, "TMPDIR=" + env.home}, env.project)
	if err != nil {
		t.Fatalf("hook must not fail the agent: %v\n%s", err, out)
	}
	if out != "" {
		t.Fatalf("hook must be silent when unavailable, got:\n%s", out)
	}
}
