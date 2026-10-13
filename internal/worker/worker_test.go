package worker

import (
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
	cases := map[string]string{
		"Extend DataApi with request/response logging": "agent/extend-dataapi-with-request-response-log",
		"  Ünïcödé & SPECIAL!!  ":                      "agent/n-c-d-special",
		"":                                             "agent/item",
	}
	for in, want := range cases {
		if got := normalizeBranch(in, "agent/"); got != want {
			t.Errorf("normalizeBranch(%q) = %q want %q", in, got, want)
		}
	}
	long := normalizeBranch(makeLongTitle(), "agent/")
	if len(long) > len("agent/")+40 {
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
