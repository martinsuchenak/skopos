package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/rest"
)

// A run skopos cancels (the item was rejected) stops at the next heartbeat:
// the stage's context ends, nothing is reported, and the run log says why.
// Here the run is blocked waiting for its clone, which the test holds.
func TestCancelledRunStops(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })

	var mu sync.Mutex
	var calls []string
	var logged strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/runs/r1/heartbeat":
			// What runs.Handler answers for a cancelled run.
			rest.RespondError(w, http.StatusGone, fmt.Sprintf("run cancelled: %s", "item rejected"))
		case "/api/runs/r1/log":
			sc := bufio.NewScanner(r.Body)
			for sc.Scan() {
				var rec RunLogRecord
				if json.Unmarshal(sc.Bytes(), &rec) == nil {
					mu.Lock()
					logged.WriteString(rec.Msg + "\n")
					mu.Unlock()
				}
			}
			_, _ = w.Write([]byte(`{"stored":1}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "k")
	client.leases["r1"] = "lease-1" // the claim's; the run log needs it
	w, repo := branchCloneWorker(t, client)
	w.cfg.ID = "w1"
	release, ok, err := tryLockClone(t.Context(), repo.Path)
	if !ok || err != nil {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer release()

	done := make(chan struct{})
	go func() {
		w.execute(t.Context(), &Run{ID: "r1", ItemID: "i1", WorkspaceID: "ws", Stage: "implement", Attempt: 1})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled run did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.HasSuffix(c, "/report") || strings.HasSuffix(c, "/pause") {
			t.Fatalf("a cancelled run reports nothing: %v", calls)
		}
	}
	for _, want := range []string{"stopping: skopos cancelled the run (item rejected)", "stopped: item rejected"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("run log lacks %q:\n%s", want, logged.String())
		}
	}
}
