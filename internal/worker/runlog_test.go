package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// runLogServer collects what workers post to /api/runs/{id}/log.
type runLogServer struct {
	mu      sync.Mutex
	records []RunLogRecord
	headers http.Header
}

func (s *runLogServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/r1/log" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.headers = r.Header.Clone()
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var rec RunLogRecord
			if json.Unmarshal(sc.Bytes(), &rec) == nil {
				s.records = append(s.records, rec)
			}
		}
		_, _ = w.Write([]byte(`{"stored":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *runLogServer) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, r := range s.records {
		b.WriteString(r.Src + "|" + r.Msg + "\n")
	}
	return b.String()
}

// fakeClaude writes a claude stand-in that prints a stream-json session
// (a tool call, its result, text, the result event, then one more event —
// the result is not always last).
func fakeClaude(t *testing.T, secret string) string {
	t.Helper()
	dir := t.TempDir()
	events := []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls","description":"List"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"a.txt\nb.txt\ntoken ` + secret + `","is_error":false}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Two files."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Two files.","session_id":"s1","total_cost_usd":0.25,"structured_output":{"answer":"ok"}}`,
		`{"type":"system","subtype":"task_summary"}`,
	}
	script := "#!/bin/sh\ncat <<'EOF'\n" + strings.Join(events, "\n") + "\nEOF\necho 'warming up' >&2\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A claude run streams readable lines (tool call, result head, text) to
// skopos while the result event still decides the outcome; secrets are
// redacted, the raw stream lands in the log file, and the worker notes the
// agent's start and end.
func TestClaudeRunStreamsToRunLog(t *testing.T) {
	const secret = "ghp_abcdefghijklmnop"
	t.Setenv("SKOPOS_TEST_PUSH_TOKEN", secret)
	var srv runLogServer
	client := NewClient(srv.start(t), "sk_worker_key")
	client.leases["r1"] = "lease-1"
	repo := &Repo{}
	repo.Push.TokenEnv = "SKOPOS_TEST_PUSH_TOKEN"
	cfg := &Config{ID: "w1", StateDir: t.TempDir(), Bin: fakeClaude(t, secret), Repos: map[string]*Repo{"ws": repo}}
	w := New(cfg, client, slog.New(slog.DiscardHandler))
	run := &Run{ID: "r1", ItemID: "i1", WorkspaceID: "ws", Stage: "plan"}
	rl := w.openRunLog(run)
	if rl == nil {
		t.Fatal("no run log")
	}
	ctx := withRunLog(context.Background(), rl)
	logFile := filepath.Join(cfg.StateDir, "runs", "r1", "plan.log")
	res := w.runAgent(ctx, AgentRun{Bin: cfg.Bin, Dir: t.TempDir(), Prompt: "p", Timeout: time.Minute, LogFile: logFile, Runner: "claude"})
	rl.close()
	if res.Outcome != OutcomeOK || res.SessionID != "s1" || res.CostUSD != 0.25 || string(res.Structured) != `{"answer":"ok"}` {
		t.Fatalf("result: %+v", res)
	}
	got := srv.text()
	for _, want := range []string{
		"worker|agent claude started: plan",
		"agent|▸ Bash ls",
		"agent|  ⎿ a.txt",
		"agent|    token [redacted]",
		"agent|Two files.",
		"agent|warming up",
		"worker|agent finished: ok, $0.25",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, secret) || strings.Contains(got, `"type"`) {
		t.Fatalf("raw events or the secret leaked:\n%s", got)
	}
	if srv.headers.Get("X-Skopos-Lease") != "lease-1" || srv.headers.Get("X-Skopos-Worker") != "w1" || srv.headers.Get("Authorization") != "Bearer sk_worker_key" {
		t.Fatalf("headers: %v", srv.headers)
	}
	raw, _ := os.ReadFile(logFile)
	if !strings.Contains(string(raw), `"type":"result"`) || !strings.Contains(string(raw), "--- stderr\nwarming up") {
		t.Fatalf("raw log: %s", raw)
	}
}

// Setup and check commands stream their output under their own source,
// with the command and its result.
func TestShellOutputStreams(t *testing.T) {
	var srv runLogServer
	client := NewClient(srv.start(t), "k")
	client.leases["r1"] = "lease-1"
	w := New(&Config{ID: "w1", StateDir: t.TempDir()}, client, slog.New(slog.DiscardHandler))
	rl := w.openRunLog(&Run{ID: "r1"})
	ctx := withRunLog(context.Background(), rl)
	dir := t.TempDir()
	if _, err := runSetup(ctx, dir, []string{"echo installing; echo done >&2"}, dir, nil, nil, nil, runLimits{total: time.Minute}); err != nil {
		t.Fatal(err)
	}
	checks := runChecks(ctx, dir, []string{"echo testing; exit 3"}, nil, nil, runLimits{total: time.Minute})
	rl.close()
	if len(checks) != 1 || checks[0].Passed {
		t.Fatalf("checks: %+v", checks)
	}
	got := srv.text()
	for _, want := range []string{
		"setup|$ echo installing; echo done >&2", "setup|installing", "setup|done", "setup|✓ passed",
		"check|$ echo testing; exit 3", "check|testing", "check|✗ exit status 3",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestEventFormatters(t *testing.T) {
	long := strings.Repeat("line\n", 10)
	cases := map[string]struct {
		got  []string
		want []string
	}{
		"claude error result": {
			claudeEventLines([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"denied"}],"is_error":true}]}}`)),
			[]string{"  ⎿ error: denied"},
		},
		"claude long result": {
			claudeEventLines([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":` + jsonString(long) + `}]}}`)),
			[]string{"  ⎿ line", "    line", "    line", "    line", "    line", "    line", "    … 4 more lines"},
		},
		"claude system": {claudeEventLines([]byte(`{"type":"system","subtype":"init"}`)), nil},
		"claude denied": {
			claudeEventLines([]byte(`{"type":"system","subtype":"permission_denied","tool_name":"Bash","message":"Permission to use Bash has been denied"}`)),
			[]string{"▸ Bash denied (not in this stage's tools)"},
		},
		"opencode tool": {
			opencodeEventLines([]byte(`{"type":"tool_use","part":{"tool":"read","state":{"status":"completed","input":{"filePath":"/wt/main.go"},"output":"package main"}}}`)),
			[]string{"▸ read /wt/main.go", "  ⎿ package main"},
		},
		"opencode text": {opencodeEventLines([]byte(`{"type":"text","part":{"text":"Done."}}`)), []string{"Done."}},
		"codex command": {
			codexEventLines([]byte(`{"type":"item.completed","item":{"type":"command_execution","command":"cat x","aggregated_output":"boom\n","exit_code":1}}`)),
			[]string{"  ⎿ error: boom"},
		},
		"codex started": {codexEventLines([]byte(`{"type":"item.started","item":{"type":"command_execution","command":"cat x"}}`)), []string{"▸ shell cat x"}},
		"not json":      {codexEventLines([]byte(`plain text`)), []string{"plain text"}},
	}
	for name, c := range cases {
		if strings.Join(c.got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", name, c.got, c.want)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Long lines are cut on rune boundaries; the redactor takes the worker's
// keys, the env-file values and account credentials — not short values or
// account paths.
func TestRunLogSplitAndRedact(t *testing.T) {
	parts := splitBytes(strings.Repeat("é", 3000), maxRunLogLine)
	if len(parts) != 2 || len(parts[0]) > maxRunLogLine || !strings.HasPrefix(parts[1], "é") {
		t.Fatalf("split: %d parts, first %d bytes", len(parts), len(parts[0]))
	}
	t.Setenv("SKOPOS_TEST_FILE_SECRET", "file-secret-value")
	w := New(&Config{StateDir: t.TempDir(), Accounts: []*Account{{Name: "a", Env: map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-123456789", "CLAUDE_CONFIG_DIR": "/home/w/.claude-a",
	}}}}, nil, slog.New(slog.DiscardHandler))
	w.SetEnvFileKeys([]string{"SKOPOS_TEST_FILE_SECRET"})
	w.AddSecrets("sk_worker_0123456789", "short")
	got := w.redactor().Replace("sk_worker_0123456789 sk-ant-123456789 file-secret-value short /home/w/.claude-a")
	if got != "[redacted] [redacted] [redacted] short /home/w/.claude-a" {
		t.Fatalf("redacted: %q", got)
	}
}

// A claude run that ends without a result event is classified from what
// stdout printed outside the events and stderr — never from tool output in
// the events (a file saying "rate limit" is not a quota).
func TestClaudeFailureIgnoresToolOutput(t *testing.T) {
	dir := t.TempDir()
	events := `{"type":"user","message":{"content":[{"type":"tool_result","content":"429 rate limit exceeded; authentication failed; connection refused"}]}}`
	script := "#!/bin/sh\ncat <<'EOF'\n" + events + "\nEOF\necho 'segfault in the CLI' >&2\nexit 1\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	res := runClaude(context.Background(), AgentRun{Bin: p, Dir: dir, Prompt: "p", Timeout: time.Minute})
	if res.Outcome != OutcomeError || !strings.Contains(res.Detail, "segfault") {
		t.Fatalf("outcome %s detail %q, want error from stderr", res.Outcome, res.Detail)
	}
	// Plain stdout outside the events still counts (an old CLI's message).
	c := &claudeResultCatcher{}
	_, _ = c.Write([]byte(events + "\nYou've hit your usage limit\n"))
	if got := c.plainText(); got != "You've hit your usage limit\n" {
		t.Fatalf("plain: %q", got)
	}
}

// An agent event too large to format is left out with one note — never
// sent as raw JSON — and the stream carries on after it.
func TestOversizedEventLeftOut(t *testing.T) {
	var srv runLogServer
	client := NewClient(srv.start(t), "k")
	client.leases["r1"] = "lease-1"
	w := New(&Config{ID: "w1", StateDir: t.TempDir()}, client, slog.New(slog.DiscardHandler))
	rl := w.openRunLog(&Run{ID: "r1"})
	lw := rl.writer("agent", false, claudeEventLines)
	_, _ = lw.Write([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":"`))
	chunk := []byte(strings.Repeat("A", 1<<20))
	for i := 0; i < 33; i++ {
		_, _ = lw.Write(chunk)
	}
	_, _ = lw.Write([]byte(`"}]}}` + "\n" + `{"type":"assistant","message":{"content":[{"type":"text","text":"after"}]}}` + "\n"))
	lw.Flush()
	rl.close()
	got := srv.text()
	if !strings.Contains(got, "agent|(an agent event over 32 MB was left out of the log)\n") || !strings.HasSuffix(got, "agent|after\n") || strings.Contains(got, "AAAA") {
		t.Fatalf("log (%d bytes): %.300s", len(got), got)
	}
	// A cut-off event line is dropped; plain text is kept.
	if unparsed([]byte(`{"type":"assi`)) != nil || unparsed([]byte("plain"))[0] != "plain" {
		t.Fatal("unparsed")
	}
}
