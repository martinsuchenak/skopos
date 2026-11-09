package worker

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureRunLog is a run log whose lines land in a buffer (through its
// sink), for tests of what a stage tells the live view.
func captureRunLog(run *Run) (*runLog, func() string) {
	var mu sync.Mutex
	var buf bytes.Buffer
	h := slog.NewTextHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
			return slog.Attr{}
		}
		return a
	}})
	rl := &runLog{run: run, sink: slog.New(h), now: time.Now}
	return rl, func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// A step gets a line when it starts; a slow one a second when it ends.
func TestStepfNotesSlowSteps(t *testing.T) {
	rl, lines := captureRunLog(&Run{ID: "r1"})
	now := time.Unix(1000, 0)
	rl.now = func() time.Time { return now }
	rl.stepf("fetching origin/%s", "main")()
	done := rl.stepf("checking out %s", "agent/x")
	now = now.Add(72 * time.Second)
	done()
	got := lines()
	for _, want := range []string{"fetching origin/main …", "checking out agent/x …", "checking out agent/x: done in 1m12s"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fetching origin/main: done") {
		t.Errorf("a quick step gets one line:\n%s", got)
	}
	var nilLog *runLog
	nilLog.stepf("no log")() // no-op
}

// Waiting for a branch-checkout clone, and putting back a branch a killed
// run left, show in the live view — not only in the worker's own log.
func TestHoldCloneNotesTheWait(t *testing.T) {
	w, repo := branchCloneWorker(t, nil)
	gitOut(t, repo.Path, "checkout", "-q", "-b", "agent/left")
	release, ok, err := tryLockClone(t.Context(), repo.Path)
	if !ok || err != nil {
		t.Fatalf("lock: %v %v", ok, err)
	}
	run := &Run{ID: "r1"}
	rl, lines := captureRunLog(run)
	var skew atomic.Int64 // the clock jumps when the lock is let go
	rl.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	go func() {
		time.Sleep(300 * time.Millisecond)
		skew.Store(int64(10 * time.Second)) // the wait counts as slow
		release()
	}()
	got, err := w.holdClone(withRunLog(t.Context(), rl), repo, w.log)
	if err != nil {
		t.Fatal(err)
	}
	got()
	out := lines()
	for _, want := range []string{
		"waiting for the clone " + repo.Path + ": another worker process is using it …",
		"is using it: done in 1", // 10s, plus the retry interval
		"the clone was on agent/left with nothing uncommitted; switching it to main",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
