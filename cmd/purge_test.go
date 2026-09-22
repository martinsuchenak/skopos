package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Server-backed lifecycle tests for the two bulk-purge CLI helpers, run
// against the full production wiring of keysTestServer.

func purgeSeedPost(t *testing.T, url, key, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("seed POST %s failed: %d", url, resp.StatusCode)
	}
}

func TestSessionsDoPurgeLifecycle(t *testing.T) {
	ts := keysTestServer(t, "root-key")
	ctx := context.Background()

	report := `{"agent_id":"a1","agent_type":"zcode","workspace":"ws-a","status":"running"}`
	purgeSeedPost(t, ts.URL+"/api/reports", "root-key", report)
	purgeSeedPost(t, ts.URL+"/api/reports", "root-key", report)
	purgeSeedPost(t, ts.URL+"/api/reports", "root-key",
		`{"agent_id":"a1","agent_type":"zcode","workspace":"ws-b","status":"running"}`)

	n, err := sessionsDoPurge(ctx, ts.URL, "root-key", "ws-a")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 ws-a sessions purged, got %d", n)
	}
	// Purge-everything (empty workspace) takes the rest.
	n, err = sessionsDoPurge(ctx, ts.URL, "root-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 remaining session purged, got %d", n)
	}
	// Empty server: 0, not an error.
	if n, err = sessionsDoPurge(ctx, ts.URL, "root-key", ""); err != nil || n != 0 {
		t.Fatalf("expected clean 0, got n=%d err=%v", n, err)
	}
}

func TestBlackboardDoPurgeLifecycle(t *testing.T) {
	ts := keysTestServer(t, "root-key")
	ctx := context.Background()

	purgeSeedPost(t, ts.URL+"/api/blackboard/entries", "root-key",
		`{"scope":"project","entry_type":"bug","title":"t1","content":"c","author_agent_id":"a1","workspace_id":"ws-a"}`)
	purgeSeedPost(t, ts.URL+"/api/blackboard/entries", "root-key",
		`{"scope":"project","entry_type":"bug","title":"t2","content":"c","author_agent_id":"a1","workspace_id":"ws-a"}`)
	purgeSeedPost(t, ts.URL+"/api/blackboard/entries", "root-key",
		`{"scope":"project","entry_type":"finding","title":"t3","content":"c","author_agent_id":"a1","workspace_id":"ws-a"}`)
	purgeSeedPost(t, ts.URL+"/api/blackboard/entries", "root-key",
		`{"scope":"project","entry_type":"bug","title":"t4","content":"c","author_agent_id":"a1","workspace_id":"ws-b"}`)

	n, err := blackboardDoPurge(ctx, ts.URL, "root-key", "ws-a", "bug")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 ws-a bugs purged, got %d", n)
	}
	if n, err = blackboardDoPurge(ctx, ts.URL, "root-key", "ws-a", "finding"); err != nil || n != 1 {
		t.Fatalf("expected 1 finding purged, got n=%d err=%v", n, err)
	}
	if n, err = blackboardDoPurge(ctx, ts.URL, "root-key", "ws-b", "bug"); err != nil || n != 1 {
		t.Fatalf("expected 1 ws-b bug purged, got n=%d err=%v", n, err)
	}
}

func TestBlackboardDoPurgeRejectsBadType(t *testing.T) {
	ts := keysTestServer(t, "root-key")
	if _, err := blackboardDoPurge(context.Background(), ts.URL, "root-key", "ws-a", "incident"); err == nil {
		t.Fatal("expected an error for an invalid entry type")
	}
}

func TestSessionsCmdRegistered(t *testing.T) {
	cmd := sessionsCmd()
	if cmd == nil || cmd.Name != "sessions" || len(cmd.Commands) == 0 {
		t.Fatalf("unexpected sessions command: %+v", cmd)
	}
}

func TestInboxCompleteAndPurgeLifecycle(t *testing.T) {
	ts := keysTestServer(t, "root-key")
	ctx := context.Background()
	K := "root-key"

	// Capture two items in ws-a.
	mk := `{"workspace_id":"ws-a","title":"t","content":"c","author_agent_id":"a"}`
	purgeSeedPost(t, ts.URL+"/api/inbox", K, mk)
	purgeSeedPost(t, ts.URL+"/api/inbox", K, mk)

	list := []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}{}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/inbox?workspace_id=ws-a", nil)
	req.Header.Set("Authorization", "Bearer root-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list) != 2 {
		t.Fatalf("expected 2 seeded items, got %d", len(list))
	}
	if err := inboxAction(ctx, ts.URL, "root-key", list[0].ID, "complete", []byte("{}"), "completing item"); err != nil {
		t.Fatal(err)
	}
	// Reopen undoes the manual complete.
	if err := inboxAction(ctx, ts.URL, "root-key", list[0].ID, "reopen", []byte("{}"), "reopening item"); err != nil {
		t.Fatal(err)
	}

	// Reopen undoes the manual complete, so no done items remain; the
	// all-statuses purge then takes both open items.
	if n, err := inboxDoPurge(ctx, ts.URL, "root-key", "ws-a", "done"); err != nil || n != 0 {
		t.Fatalf("done purge after reopen: n=%d err=%v", n, err)
	}
	if n, err := inboxDoPurge(ctx, ts.URL, "root-key", "ws-a", ""); err != nil || n != 2 {
		t.Fatalf("all purge: n=%d err=%v", n, err)
	}
	if n, err := inboxDoPurge(ctx, ts.URL, "root-key", "ws-a", "done"); err != nil || n != 0 {
		t.Fatalf("empty purge should be 0: n=%d err=%v", n, err)
	}
}
