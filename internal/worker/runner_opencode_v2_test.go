package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseOCVersion(t *testing.T) {
	for in, want := range map[string]string{"opencode v2.3.2\n": "2.3.2", "1.19.5": "1.19.5", "v2.10.0-beta": "2.10.0"} {
		if v := parseOCVersion(in); !v.ok || v.String() != want {
			t.Errorf("%q: %+v", in, v)
		}
	}
	if v := parseOCVersion(`{"type":"text"}`); v.ok || v.v2() {
		t.Errorf("no version: %+v", v)
	}
	if !(ocVersion{2, 2, 3, true}).atLeast(2, 2, 3) || (ocVersion{2, 1, 9, true}).atLeast(2, 2, 3) || !(ocVersion{2, 10, 0, true}).atLeast(2, 2, 3) {
		t.Error("atLeast")
	}
}

// The opencode 2 config: deny all first, the stage's allows, the hard denies
// last (the last match wins) and again as policies, which agent rules (build
// allows questions) cannot undo; MCP servers only the run's own.
func TestOpencodeV2Config(t *testing.T) {
	var cfg struct {
		Permissions []ocRule       `json:"permissions"`
		Policies    []ocRule       `json:"policies"`
		MCP         map[string]any `json:"mcp"`
		Update      string         `json:"update"`
	}
	raw := opencodeV2Config(append(baseImplTools, "Bash(go:*)", "mcp__skopos"),
		[]string{"Bash(git push:*)", "mcp__skopos__inbox_update"},
		map[string]any{"skopos": map[string]any{"type": "remote", "url": "https://s/mcp"}})
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Permissions[0] != (ocRule{"*", "*", "deny"}) || cfg.Update != "disable" {
		t.Fatalf("first rule and update: %+v %q", cfg.Permissions[0], cfg.Update)
	}
	idx := func(r ocRule) int { return slices.Index(cfg.Permissions, r) }
	for _, r := range []ocRule{{"shell", "go *", "allow"}, {"edit", "*", "allow"}, {"skopos*", "*", "allow"}} {
		if idx(r) < 1 {
			t.Errorf("missing allow %+v", r)
		}
	}
	last := len(cfg.Permissions) - 1
	for _, r := range []ocRule{{"shell", "git push *", "deny"}, {"edit", ".git/*", "deny"}, {"shell", "*$*", "deny"},
		{"skopos_inbox_update", "*", "deny"}, {"external_directory", "*", "deny"}, {"question", "*", "deny"}, {"subagent", "*", "deny"}} {
		if i := idx(r); i <= idx(ocRule{"shell", "go *", "allow"}) || i > last {
			t.Errorf("hard deny %+v at %d", r, i)
		}
	}
	for _, p := range []ocRule{{"tool.use", "question:*", "deny"}, {"tool.use", "shell:*$*", "deny"}, {"tool.use", "edit:*/.git/*", "deny"},
		{"tool.use", "skopos_inbox_update:*", "deny"}, {"skill.use", "*", "deny"}, {"plugin.use", "*", "deny"}} {
		if !slices.Contains(cfg.Policies, p) {
			t.Errorf("missing policy %+v", p)
		}
	}
	if i, j := slices.Index(cfg.Policies, ocRule{"mcp.use", "*", "deny"}), slices.Index(cfg.Policies, ocRule{"mcp.use", "skopos", "allow"}); i < 0 || j < i {
		t.Errorf("mcp.use: deny all, then the run's servers: %d %d", i, j)
	}
	servers, _ := cfg.MCP["servers"].(map[string]any)
	if servers["skopos"] == nil {
		t.Errorf("mcp.servers: %v", cfg.MCP)
	}

	// A plan stage: reads only.
	ro := opencodeV2Config(basePlanTools, nil, nil)
	if strings.Contains(ro, `"action":"edit","resource":"*","effect":"allow"`) || strings.Contains(ro, `"action":"shell","resource":"*","effect":"allow"`) || strings.Contains(ro, `"mcp"`) {
		t.Fatalf("plan stage: %s", ro)
	}
}

// An opencode 2 run: a private server, the effort as the model's variant,
// no --dir (the working directory and $PWD), and the rules and MCP servers
// in the run's config directory — never in OPENCODE_PERMISSION.
func TestOpencodeV2Run(t *testing.T) {
	logDir := t.TempDir()
	oc := fakeCLI(t, `if [ "$1" = "--version" ]; then echo "opencode v2.3.2"; exit 0; fi
printf '%s\n' "$@" > `+logDir+`/args
cp "$OPENCODE_CONFIG_DIR/opencode.json" `+logDir+`/cfg
echo "PWD=$PWD PERM=${OPENCODE_PERMISSION:-none} CONTENT=${OPENCODE_CONFIG_CONTENT:-none}" > `+logDir+`/env
echo '{"type":"text","sessionID":"ses_1","part":{"text":"ok"}}'`)
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"skopos":{"type":"http","url":"https://s/mcp","headers":{"Authorization":"Bearer x"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res := opencodeRunner{bin: oc}.Run(context.Background(), AgentRun{Dir: dir, Prompt: "p", Model: "p/m", Effort: "max",
		AllowedTools: basePlanTools, MCPConfig: mcp, Timeout: time.Minute, TempDir: t.TempDir()})
	if res.Outcome != OutcomeOK || res.Text != "ok" || res.SessionID != "ses_1" {
		t.Fatalf("result: %+v", res)
	}
	if a := readArgs(t, logDir); !strings.HasPrefix(a, "run\n--standalone\n--format\njson\n-m\np/m#max\np") || strings.Contains(a, "--dir") || strings.Contains(a, "--variant") {
		t.Errorf("args:\n%s", a)
	}
	cfg, err := os.ReadFile(filepath.Join(logDir, "cfg"))
	if err != nil || !strings.Contains(string(cfg), `"permissions"`) || !strings.Contains(string(cfg), `"Authorization":"Bearer x"`) {
		t.Errorf("config file: %s %v", cfg, err)
	}
	if env, _ := os.ReadFile(filepath.Join(logDir, "env")); !strings.Contains(string(env), "PWD="+dir+" ") || !strings.Contains(string(env), "PERM=none CONTENT=none") {
		t.Errorf("env: %s", env)
	}
}

// Check takes opencode 2 from 2.2.3 with the flags a run uses; an older 2.x
// (it ignores the skill, plugin and MCP policies) and an unknown major are
// refused. The effort needs no flag on 2.
func TestOpencodeV2Check(t *testing.T) {
	cli := func(version string) opencodeRunner {
		return opencodeRunner{bin: fakeCLI(t, `if [ "$1" = "--version" ]; then echo "opencode v`+version+`"; exit 0; fi
echo "--format --session --model --standalone"`)}
	}
	ctx := context.Background()
	if err := cli("2.3.2").Check(ctx, ""); err != nil {
		t.Fatalf("2.3.2: %v", err)
	}
	if err := cli("2.3.2").EffortSupported(ctx, ""); err != nil {
		t.Fatalf("effort on 2: %v", err)
	}
	if err := cli("2.1.0").Check(ctx, ""); err == nil || !strings.Contains(err.Error(), "2.2.3") {
		t.Fatalf("2.1.0: %v", err)
	}
	if err := cli("3.0.0").Check(ctx, ""); err == nil || !strings.Contains(err.Error(), "opencode 1 and 2") {
		t.Fatalf("3.0.0: %v", err)
	}
}
