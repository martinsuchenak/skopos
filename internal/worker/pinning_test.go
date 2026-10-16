package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func pinnedConfig(now time.Time) *Config {
	glm := &Account{Name: "glm", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"}
	qwen := &Account{Name: "qwen", Runner: "opencode-qwen", Model: "llmrouter/qwen/qwen3.8-27b"}
	qwen2 := &Account{Name: "qwen-2", Runner: "opencode-qwen", Priority: 2}
	qwen.exhaustedUntil = now.Add(2 * time.Hour)
	qwen2.exhaustedUntil = now.Add(40 * time.Minute)
	return &Config{
		Runners:  []string{"opencode-glm", "opencode-qwen", "gemini"},
		Accounts: []*Account{glm, qwen, qwen2, {Name: "legacy"}}, // legacy: runner "" = claude
	}
}

// A run that names a runner never falls back to another runner's account;
// a run with no preference still may.
func TestPickAccountPinsToRunner(t *testing.T) {
	now := time.Now()
	w := &Worker{cfg: pinnedConfig(now), accounts: map[string]string{}, sessions: map[string]string{}}
	log := slog.Default()
	if a := w.pickAccount(&Run{ID: "r1", Runner: "opencode-qwen"}, log); a != nil {
		t.Fatalf("an exhausted pinned runner must not borrow %q", a.Name)
	}
	if a := w.pickAccount(&Run{ID: "r2", Runner: "opencode-glm"}, log); a == nil || a.Name != "glm" {
		t.Fatalf("pinned glm: %+v", a)
	}
	if a := w.pickAccount(&Run{ID: "r3"}, log); a == nil {
		t.Fatal("an unpinned run may use any available account")
	}
	if a := w.cfg.AccountFor("claude", now); a == nil || a.Name != "legacy" {
		t.Fatalf("an account without a runner feeds claude: %+v", a)
	}
}

// No account for a pinned run: a quota pause until the runner's EARLIEST
// reset — or a clear failure when no account feeds that runner at all.
func TestNoAccountOutcome(t *testing.T) {
	now := time.Now()
	w := &Worker{cfg: pinnedConfig(now)}
	var q *quotaError
	err := w.noAccount(&Run{Runner: "opencode-qwen"}, "implementation")
	if !errors.As(err, &q) || !strings.Contains(q.detail, "opencode-qwen") {
		t.Fatalf("exhausted runner must pause: %v", err)
	}
	if d := q.resetAt.Sub(now.Add(40 * time.Minute)); d < -time.Second || d > time.Second {
		t.Fatalf("pause must last until the earliest reset (qwen-2, 40m): %v", q.resetAt)
	}
	err = w.noAccount(&Run{Runner: "gemini"}, "review")
	if errors.As(err, &q) || err == nil || !strings.Contains(err.Error(), `no account feeds runner "gemini"`) {
		t.Fatalf("a runner no account feeds must fail, not pause forever: %v", err)
	}
	if err := w.noAccount(&Run{}, "planning"); !errors.As(err, &q) {
		t.Fatalf("unpinned: quota pause: %v", err)
	}
}

// Claims offer only runners with a usable account right now.
func TestAvailableRunners(t *testing.T) {
	now := time.Now()
	cfg := pinnedConfig(now)
	if got := strings.Join(cfg.AvailableRunners(now), ","); got != "opencode-glm" {
		t.Fatalf("available runners: %q (qwen exhausted, gemini has no account)", got)
	}
	if got := strings.Join(cfg.AvailableRunners(now.Add(time.Hour)), ","); got != "opencode-glm,opencode-qwen" {
		t.Fatalf("after qwen-2's reset: %q", got)
	}
}

// A quota pause with other accounts still usable returns at once (the
// worker keeps claiming); it only sleeps when nothing at all is usable.
func TestQuotaPauseDoesNotStallOtherAccounts(t *testing.T) {
	paused := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pause") {
			paused++
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	now := time.Now()
	w := &Worker{cfg: pinnedConfig(now), client: NewClient(srv.URL, "k"), executors: map[string]string{}}
	w.cfg.ID = "worker-t"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	w.finish(ctx, &Run{ID: "r1", Runner: "opencode-qwen"}, slog.Default(), &quotaError{detail: "x", resetAt: now.Add(40 * time.Minute)})
	if time.Since(start) > time.Second || paused != 1 {
		t.Fatalf("must pause the run and return at once while glm is usable (took %s, pauses %d)", time.Since(start), paused)
	}
}
