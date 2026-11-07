package runs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func logBody(recs ...string) *strings.Reader {
	return strings.NewReader(strings.Join(recs, "\n") + "\n")
}

// The claim holder appends; others are refused; lines after the report
// still land (the worker's last lines follow it); readers page by cursor.
func TestRunLogAppendAndRead(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})

	n, err := s.AppendLog(ctx, run.ID, "w1", run.LeaseToken, logBody(
		`{"t":1,"src":"worker","msg":"implement run"}`,
		`not json`,
		`{"t":2,"src":"nonsense","msg":"dropped"}`,
		`{"t":3,"src":"agent","msg":"▸ Bash go test ./...","err":false}`,
	))
	if err != nil || n != 2 {
		t.Fatalf("append: %d %v", n, err)
	}
	for name, try := range map[string]func() error{
		"other worker": func() error {
			_, err := s.AppendLog(ctx, run.ID, "w2", run.LeaseToken, logBody(`{"t":1,"src":"agent","msg":"x"}`))
			return err
		},
		"wrong lease": func() error {
			_, err := s.AppendLog(ctx, run.ID, "w1", "nope", logBody(`{"t":1,"src":"agent","msg":"x"}`))
			return err
		},
		"other key": func() error {
			_, err := s.AppendLog(scopedCtx("ws-a"), run.ID, "w1", run.LeaseToken, logBody(`{"t":1,"src":"agent","msg":"x"}`))
			return err
		},
	} {
		if err := try(); !errors.Is(err, ErrLeaseHeld) {
			t.Errorf("%s: %v, want ErrLeaseHeld", name, err)
		}
	}
	if _, err := s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded}, run.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AppendLog(ctx, run.ID, "w1", run.LeaseToken, logBody(`{"t":4,"src":"worker","msg":"implement succeeded"}`)); err != nil || n != 1 {
		t.Fatalf("after the report: %d %v", n, err)
	}

	page, err := s.ReadLog(ctx, run.ID, 0, 0, 0)
	if err != nil || len(page.Records) != 3 || page.Records[1].Msg != "▸ Bash go test ./..." || page.More || page.Truncated {
		t.Fatalf("read: %+v %v", page, err)
	}
	later, _ := s.ReadLog(ctx, run.ID, page.Cursor, 0, 0)
	if len(later.Records) != 0 || later.Cursor != page.Cursor {
		t.Fatalf("after the cursor: %+v", later)
	}
	tail, _ := s.ReadLog(ctx, run.ID, 0, 1, 0)
	if len(tail.Records) != 1 || tail.Records[0].Msg != "implement succeeded" || tail.Cursor != page.Cursor {
		t.Fatalf("tail: %+v", tail)
	}
	if _, err := s.ReadLog(scopedCtx("ws-b"), run.ID, 0, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out of scope read: %v", err)
	}
}

// A run's log stops at the cap with one marker line.
func TestRunLogCap(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	line := fmt.Sprintf(`{"t":1,"src":"agent","msg":%q}`, strings.Repeat("x", maxLogMsg))
	batch := strings.Repeat(line+"\n", 100) // ~400 KB
	for i := 0; i < 30; i++ {
		if _, err := s.AppendLog(ctx, run.ID, "w1", run.LeaseToken, strings.NewReader(batch)); err != nil {
			t.Fatal(err)
		}
	}
	used, _ := s.storage.LogBytes(ctx, run.ID)
	if used != MaxRunLogBytes {
		t.Fatalf("used %d, want the cap %d", used, MaxRunLogBytes)
	}
	page, _ := s.ReadLog(ctx, run.ID, 0, 3, 0)
	if !page.Truncated || page.Records[2].Msg != truncatedMsg {
		t.Fatalf("tail: truncated=%v %+v", page.Truncated, page.Records)
	}
	// Paging reads the whole log a page of batches at a time.
	total, after := 0, int64(0)
	for {
		p, err := s.ReadLog(ctx, run.ID, after, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		total += len(p.Records)
		after = p.Cursor
		if !p.More {
			break
		}
	}
	if total < 2000 {
		t.Fatalf("paged %d records", total)
	}
}

// A read with nothing new waits for the run's next batch (and only that
// run's); without one it returns empty after the wait.
func TestRunLogLongPoll(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	_ = mustQueue(t, s, ctx, "ws-a", "item-2", StagePlan)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	other, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Workspaces: []string{"ws-a"}})

	start := time.Now()
	if page, err := s.ReadLog(ctx, run.ID, 0, 0, 100*time.Millisecond); err != nil || len(page.Records) != 0 || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("empty wait: %+v %v after %s", page, err, time.Since(start))
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = s.AppendLog(ctx, other.ID, "w2", other.LeaseToken, logBody(`{"t":1,"src":"agent","msg":"not this run"}`))
		time.Sleep(50 * time.Millisecond)
		_, _ = s.AppendLog(ctx, run.ID, "w1", run.LeaseToken, logBody(`{"t":2,"src":"agent","msg":"woke"}`))
	}()
	start = time.Now()
	page, err := s.ReadLog(ctx, run.ID, 0, 0, 5*time.Second)
	if err != nil || len(page.Records) != 1 || page.Records[0].Msg != "woke" || time.Since(start) > 3*time.Second {
		t.Fatalf("woken read: %+v %v after %s", page, err, time.Since(start))
	}
}

// The first tail read decodes only the newest batches that hold the tail.
func TestRunLogTailReadsFromTheEnd(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	for i := 0; i < 10; i++ {
		_, _ = s.AppendLog(ctx, run.ID, "w1", run.LeaseToken, logBody(
			fmt.Sprintf(`{"t":%d,"src":"agent","msg":"a%d"}`, i, i), fmt.Sprintf(`{"t":%d,"src":"agent","msg":"b%d"}`, i, i)))
	}
	rows, err := s.storage.LogTailRows(ctx, run.ID, 3)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows for a tail of 3: %d %v", len(rows), err)
	}
	page, _ := s.ReadLog(ctx, run.ID, 0, 3, 0)
	var got []string
	for _, r := range page.Records {
		got = append(got, r.Msg)
	}
	all, _ := s.ReadLog(ctx, run.ID, 0, 0, 0)
	if strings.Join(got, ",") != "b8,a9,b9" || page.Cursor != all.Cursor {
		t.Fatalf("tail %v cursor %d (end %d)", got, page.Cursor, all.Cursor)
	}
}
