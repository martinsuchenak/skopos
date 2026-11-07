package runs

import (
	"slices"
	"testing"
)

// The claim answer's index duty: workspaces with the code_index switch on
// are enabled for every worker serving them; the default branch goes to one
// able worker — the smallest id among those whose claims say they can — and
// moves when that worker stops being able.
func TestIndexDuty(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {CodeIndex: true}, "ws-b": {}})
	ctx := rootCtx()
	poll := func(worker string, indexable ...string) IndexDuty {
		t.Helper()
		in := ClaimInput{WorkerID: worker, Workspaces: []string{"ws-a", "ws-b"}, Indexable: indexable}
		if _, err := s.ClaimNext(ctx, in); err != nil {
			t.Fatal(err)
		}
		return s.IndexDuty(ctx, in)
	}
	d := poll("w2", "ws-a", "ws-b")
	if !slices.Equal(d.Enabled, []string{"ws-a"}) || !slices.Equal(d.Default, []string{"ws-a"}) {
		t.Fatalf("only able worker: %+v", d)
	}
	d = poll("w1", "ws-a")
	if !slices.Equal(d.Default, []string{"ws-a"}) {
		t.Fatalf("w1 (smallest id) takes the default branch: %+v", d)
	}
	if d = poll("w2", "ws-a", "ws-b"); len(d.Default) != 0 || !slices.Equal(d.Enabled, []string{"ws-a"}) {
		t.Fatalf("w2 keeps item branches but not the default: %+v", d)
	}
	// w1 can no longer fetch: the duty moves back to w2.
	poll("w1")
	if d = poll("w2", "ws-a"); !slices.Equal(d.Default, []string{"ws-a"}) {
		t.Fatalf("duty moves to the remaining able worker: %+v", d)
	}
	// An indexable claim for a workspace the worker does not offer is
	// ignored.
	in := ClaimInput{WorkerID: "w3", Workspaces: []string{"ws-b"}, Indexable: []string{"ws-a"}}
	if _, err := s.ClaimNext(ctx, in); err != nil {
		t.Fatal(err)
	}
	if able, _ := s.storage.IndexableWorkers(ctx, "ws-a", s.now().Add(-indexDutyWindow)); slices.Contains(able, "w3") {
		t.Fatalf("w3 does not offer ws-a: %v", able)
	}
}
