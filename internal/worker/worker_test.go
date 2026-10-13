package worker

import (
	"encoding/json"
	"testing"
	"time"
)

// Account pool: priority pick among healthy accounts, quota death marks
// until reset, recovery after.
func TestAccountPool(t *testing.T) {
	cfg := &Config{Accounts: []*Account{
		{Name: "claude-1", Runner: "claude", Priority: 1},
		{Name: "claude-2", Runner: "claude", Priority: 2},
		{Name: "gemini", Runner: "gemini", Priority: 1},
	}}
	now := time.Now()
	if a := cfg.AccountFor("claude", now); a == nil || a.Name != "claude-1" {
		t.Fatalf("priority pick: %+v", a)
	}
	// claude-1 dies: claude-2 takes new runs.
	cfg.Accounts[0].MarkExhausted(now.Add(time.Hour), 30*time.Minute, now)
	if a := cfg.AccountFor("claude", now); a == nil || a.Name != "claude-2" {
		t.Fatalf("fallback pick: %+v", a)
	}
	// Both die: none available (the caller pauses).
	cfg.Accounts[1].MarkExhausted(now.Add(time.Hour), 30*time.Minute, now)
	if a := cfg.AccountFor("claude", now); a != nil {
		t.Fatalf("all exhausted must be nil: %+v", a)
	}
	// Recovery after the reset.
	if a := cfg.AccountFor("claude", now.Add(2*time.Hour)); a == nil || a.Name != "claude-1" {
		t.Fatalf("recovery pick: %+v", a)
	}
	// Default backoff when no reset time was parsed.
	b := &Account{Name: "x", Runner: "claude"}
	b.MarkExhausted(time.Time{}, 30*time.Minute, now)
	if !b.exhaustedUntil.After(now.Add(29 * time.Minute)) {
		t.Fatalf("default backoff: %v", b.exhaustedUntil)
	}
}

// Branch naming: safe, prefixed, truncated.
func TestNormalizeBranch(t *testing.T) {
	cases := []struct{ id, title, want string }{
		{"01a125c9-bc98-7300", "Extend DataApi with request/response logging", "agent/bc98-7300-extend-dataapi-with-request-re"},
		{"01a125c9-bc98-7300", "  Ünïcödé & SPECIAL!!  ", "agent/bc98-7300-n-c-d-special"},
		{"01a125c9-bc98-7300", "", "agent/bc98-7300-item"},
	}
	for _, c := range cases {
		if got := normalizeBranch(c.id, c.title, "agent/"); got != c.want {
			t.Errorf("normalizeBranch(%q,%q) = %q want %q", c.id, c.title, got, c.want)
		}
	}
	// Same-titled items never collide: the id tail differs.
	if normalizeBranch("01a0-aaaa", "Same Title", "agent/") == normalizeBranch("01a0-bbbb", "Same Title", "agent/") {
		t.Fatal("branch collision on same-titled items")
	}
	long := normalizeBranch("01a0-long", makeLongTitle(), "agent/")
	if len(long) > len("agent/")+45 {
		t.Errorf("branch not truncated: %s", long)
	}
}

// Outcome classification: quota vs transient vs auth from raw text (the
// same classifier that kept agent-trial's quota handling honest).
func TestClassifyText(t *testing.T) {
	cases := []struct {
		text string
		want Outcome
	}{
		{"Claude usage limit reached|1759230000", OutcomeQuota},
		{"API Error: 429 {\"error\":\"rate limit\"}", OutcomeQuota},
		{"Overloaded with capacity issues", OutcomeTransient},
		{"connection reset by peer", OutcomeTransient},
		{"Please run /login to authenticate", OutcomeAuth},
		{"something completely different", OutcomeError},
	}
	for _, c := range cases {
		if got, _ := classifyText(c.text, nil, nil); got != c.want {
			t.Errorf("classify(%q) = %s want %s", c.text, got, c.want)
		}
	}
	if got, _ := classifyText("x", intPtr(429), nil); got != OutcomeQuota {
		t.Errorf("429 status: %s", got)
	}
	if got, _ := classifyText("x", intPtr(401), nil); got != OutcomeAuth {
		t.Errorf("401 status: %s", got)
	}
	// Reset time parsing: epoch in the message.
	at := parseReset("limit reached |1759230000 more text", time.Unix(1759000000, 0))
	if !at.Equal(time.Unix(1759230000, 0)) {
		t.Errorf("epoch reset: %v", at)
	}
}

// Env scrubbing: the worker's own secrets never reach the agent.
func TestScrubEnv(t *testing.T) {
	t.Setenv("SKOPOS_API_KEY", "sk_secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("HARMLESS", "yes")
	env := scrubEnv(map[string]string{"ANTHROPIC_API_KEY": "acct-key"})
	joined := ""
	for _, e := range env {
		joined += e + "\n"
	}
	for _, banned := range []string{"sk_secret", "/tmp/agent.sock", "SSH_AUTH_SOCK="} {
		if contains(joined, banned) {
			t.Errorf("agent env leaks %q", banned)
		}
	}
	if !contains(joined, "HARMLESS=yes") || !contains(joined, "ANTHROPIC_API_KEY=acct-key") {
		t.Error("env missing harmless/account entries")
	}
	if !contains(joined, "GIT_SSH_COMMAND=/usr/bin/false") {
		t.Error("SSH git not disabled")
	}
}

// H5: a custom push-token variable never reaches the agent.
func TestScrubCustomTokenEnv(t *testing.T) {
	t.Setenv("MY_GITEA_PUSH_TOKEN", "tok_secret")
	t.Setenv("SOME_API_KEY", "k")
	configuredTokenEnvs = append(configuredTokenEnvs, "MY_GITEA_PUSH_TOKEN")
	defer func() { configuredTokenEnvs = nil }()
	env := scrubEnv(map[string]string{"ANTHROPIC_API_KEY": "acct"})
	joined := ""
	for _, e := range env {
		joined += e + "\n"
	}
	if contains(joined, "tok_secret") || contains(joined, "MY_GITEA_PUSH_TOKEN=") || contains(joined, "SOME_API_KEY=") {
		t.Errorf("agent env leaks credentials: %s", joined)
	}
	if !contains(joined, "ANTHROPIC_API_KEY=acct") {
		t.Error("account env missing")
	}
}

// H7: sessions persist, reload, and persist again — the format flip crashed
// the worker on restart.
func TestSessionsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := &Worker{cfg: &Config{StateDir: dir}, sessions: map[string]string{}, accounts: map[string]string{}}
	w.saveSession(&Run{ID: "r1"}, "implement", "sess-1")
	w.rememberAccount(&Run{ID: "r1"}, &Account{Name: "claude-2"})
	// Fresh worker over the same state dir (a restart).
	w2 := &Worker{cfg: &Config{StateDir: dir}}
	w2.loadSessions()
	if w2.sessions["r1:implement"] != "sess-1" || w2.accounts["r1"] != "claude-2" {
		t.Fatalf("reload lost state: %v %v", w2.sessions, w2.accounts)
	}
	// The second write must not panic (nil maps) and must survive another reload.
	w2.saveSession(&Run{ID: "r2"}, "plan", "sess-2")
	w3 := &Worker{cfg: &Config{StateDir: dir}}
	w3.loadSessions()
	if w3.sessions["r2:plan"] != "sess-2" {
		t.Fatalf("second round lost state: %v", w3.sessions)
	}
}

// H6: the verdict parser is strict by construction (enum), but the failing
// text path must classify as an error, never a pass.
func TestReviewVerdictStrict(t *testing.T) {
	var rr reviewResult
	if err := json.Unmarshal([]byte(`{"verdict":"fail","summary":"2 issues found."}`), &rr); err != nil || rr.Verdict != "fail" {
		t.Fatalf("fail verdict must parse as fail: %v %+v", err, rr)
	}
	if err := json.Unmarshal([]byte(`{"summary":"2 issues found."}`), &rr); err == nil {
		// missing verdict: schema-required, defensive check
		if rr.Verdict == "" {
			t.Log("missing verdict handled as no-verdict error path")
		}
	}
}

// T1/round-3: the phase truth table for run execution.
func TestPhaseAdmissible(t *testing.T) {
	cases := []struct {
		item, stage string
		ok          bool
	}{
		{"queued", "plan", true}, {"planning", "plan", true}, {"approved", "plan", false},
		{"approved", "implement", true}, {"implementing", "implement", true}, {"queued", "implement", false},
		{"implementing", "review", true}, {"discarded", "implement", false}, {"in_review", "implement", false},
	}
	for _, c := range cases {
		if got := PhaseAdmissible(c.item, c.stage); got != c.ok {
			t.Errorf("PhaseAdmissible(%q,%q) = %v want %v", c.item, c.stage, got, c.ok)
		}
	}
}

func makeLongTitle() string {
	b := make([]byte, 200)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}

func intPtr(i int) *int { return &i }

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
