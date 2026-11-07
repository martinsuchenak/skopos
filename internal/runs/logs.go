package runs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/textcut"
)

// Run logs: the worker streams what a run's agent, setup and checks print
// (internal/worker/runlog.go) in batches of newline-delimited records under
// the claim's lease; skopos keeps them per run (run_logs, gzipped per batch,
// capped per run). Readers long-poll (?wait=): a read with nothing new waits
// for the next batch of that run — only those readers hear about it, not
// every dashboard on the event stream.

// LogRecord is one line of a run log.
type LogRecord struct {
	T   int64  `json:"t"`             // Unix ms (the worker's clock)
	Src string `json:"src"`           // worker | setup | check | agent
	Msg string `json:"msg"`           // one line
	Err bool   `json:"err,omitempty"` // stderr or a failure
}

// LogPage is what GET /api/runs/{id}/log answers.
type LogPage struct {
	Records []LogRecord `json:"records"`
	// Cursor: pass as ?after= to read only what came later.
	Cursor int64 `json:"cursor"`
	// More: rows past this page exist (read again with after=cursor).
	More bool `json:"more"`
	// Truncated: the run reached the size cap; later lines were dropped.
	Truncated bool `json:"truncated"`
}

const (
	// MaxRunLogBytes caps one run's log (uncompressed records).
	MaxRunLogBytes = 8 << 20
	// maxLogMsg bounds one record's text.
	maxLogMsg = 4000
	// logPageRows: batches read per page.
	logPageRows  = 200
	truncatedMsg = "(the log reached its size cap; later lines were dropped)"
)

var logSources = map[string]bool{"worker": true, "setup": true, "check": true, "agent": true}

// MaxLogWait bounds a long-poll read (under the server's 30s write timeout).
const MaxLogWait = 25 * time.Second

// logSignal is closed when the run's next batch is stored: every reader
// waiting on that run wakes.
func (s *Service) logSignal(runID string) <-chan struct{} {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logWake == nil {
		s.logWake = map[string]chan struct{}{}
	}
	ch, ok := s.logWake[runID]
	if !ok {
		ch = make(chan struct{})
		s.logWake[runID] = ch
	}
	return ch
}

func (s *Service) logStored(runID string) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if ch, ok := s.logWake[runID]; ok {
		close(ch)
		delete(s.logWake, runID)
	}
}

// AppendLog stores one batch of the run's log from the worker holding its
// claim (worker, key and lease token must match the claim; the run may
// have finished since — the last lines follow the report). Records that do
// not parse are skipped; text is cut to maxLogMsg.
func (s *Service) AppendLog(ctx context.Context, runID, workerID, leaseToken string, body io.Reader) (int, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || strings.TrimSpace(workerID) == "" || leaseToken == "" {
		return 0, fmt.Errorf("%w: run id, worker and lease are required", ErrInvalidInput)
	}
	if err := s.checkScope(ctx, runID); err != nil {
		return 0, err
	}
	ws, err := s.storage.ClaimHolder(ctx, runID, workerID, callerKeyID(ctx), hashLeaseToken(leaseToken))
	if err != nil {
		return 0, err
	}
	recs, err := parseLogRecords(body)
	if err != nil {
		return 0, err
	}
	if len(recs) == 0 {
		return 0, nil
	}
	used, err := s.storage.LogBytes(ctx, runID)
	if err != nil {
		return 0, err
	}
	if used >= MaxRunLogBytes {
		return 0, nil // already cut short
	}
	var buf bytes.Buffer
	n, size := 0, 0
	for _, r := range recs {
		b, _ := json.Marshal(r)
		if used+size+len(b)+1 > MaxRunLogBytes {
			b, _ = json.Marshal(LogRecord{T: r.T, Src: "worker", Msg: truncatedMsg, Err: true})
			buf.Write(b)
			buf.WriteByte('\n')
			n++
			size = MaxRunLogBytes - used // the run is full from here on
			break
		}
		buf.Write(b)
		buf.WriteByte('\n')
		n++
		size += len(b) + 1
	}
	if err := s.storage.AppendLog(ctx, runID, ws, n, size, buf.Bytes(), s.now().UTC()); err != nil {
		return 0, err
	}
	s.logStored(runID)
	return n, nil
}

// parseLogRecords reads newline-delimited records (at most what the
// handler's body limit let through).
func parseLogRecords(body io.Reader) ([]LogRecord, error) {
	var out []LogRecord
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r LogRecord
		if json.Unmarshal(line, &r) != nil || !logSources[r.Src] {
			continue
		}
		r.Msg = strings.ToValidUTF8(r.Msg, "\uFFFD")
		if len(r.Msg) > maxLogMsg {
			r.Msg = textcut.Truncate(r.Msg, maxLogMsg) + "…"
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: run log body: %v", ErrInvalidInput, err)
	}
	return out, nil
}

// ReadLog returns the run's log after the cursor (0 = from the start), a
// page at a time; tail > 0 on the first read (after = 0) returns only the
// last tail records, with the cursor at the end. wait > 0 (at most
// MaxLogWait): when nothing came after the cursor yet, wait that long for
// the run's next batch.
func (s *Service) ReadLog(ctx context.Context, runID string, after int64, tail int, wait time.Duration) (*LogPage, error) {
	runID = strings.TrimSpace(runID)
	if err := s.checkScope(ctx, runID); err != nil {
		return nil, err
	}
	wake := s.logSignal(runID) // before the read: a batch stored in between still wakes us
	page, err := s.readLog(ctx, runID, after, tail)
	if err != nil || len(page.Records) > 0 || wait <= 0 {
		return page, err
	}
	timer := time.NewTimer(min(wait, MaxLogWait))
	defer timer.Stop()
	select {
	case <-wake:
		return s.readLog(ctx, runID, after, tail)
	case <-timer.C:
	case <-ctx.Done():
	}
	return page, nil
}

func (s *Service) readLog(ctx context.Context, runID string, after int64, tail int) (*LogPage, error) {
	page := &LogPage{Records: []LogRecord{}, Cursor: after}
	used, err := s.storage.LogBytes(ctx, runID)
	if err != nil {
		return nil, err
	}
	page.Truncated = used >= MaxRunLogBytes
	var rows []logRow
	if after == 0 && tail > 0 {
		rows, err = s.storage.LogTailRows(ctx, runID, tail)
	} else {
		rows, page.More, err = s.storage.LogRows(ctx, runID, after, logPageRows)
	}
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		recs, err := gunzipRecords(row.body)
		if err != nil {
			return nil, err
		}
		page.Records = append(page.Records, recs...)
		page.Cursor = row.seq
	}
	if tail > 0 && after == 0 && len(page.Records) > tail {
		page.Records = page.Records[len(page.Records)-tail:]
	}
	return page, nil
}

func gunzipRecords(body []byte) ([]LogRecord, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("run log batch: %w", err)
	}
	defer func() { _ = zr.Close() }()
	var out []LogRecord
	dec := json.NewDecoder(zr)
	for {
		var r LogRecord
		if err := dec.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("run log batch: %w", err)
		}
		out = append(out, r)
	}
}

// --- storage ---

// ClaimHolder returns the run's workspace when worker, key and lease token
// match its claim (any status: the last lines follow the report).
func (s *Storage) ClaimHolder(ctx context.Context, id, workerID, keyID, tokenHash string) (string, error) {
	var ws string
	err := s.db.QueryRowContext(ctx, `
		SELECT workspace_id FROM runs
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND lease_token_hash != ''`,
		id, workerID, keyID, tokenHash).Scan(&ws)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrLeaseHeld
	}
	if err != nil {
		return "", fmt.Errorf("run log claim: %w", err)
	}
	return ws, nil
}

// LogBytes is how much of the run's log is stored (uncompressed).
func (s *Storage) LogBytes(ctx context.Context, runID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes), 0) FROM run_logs WHERE run_id = ?`, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("run log size: %w", err)
	}
	return n, nil
}

// AppendLog stores one batch, gzipped.
func (s *Storage) AppendLog(ctx context.Context, runID, workspaceID string, lines, size int, records []byte, now time.Time) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(records); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_logs (run_id, workspace_id, created_ms, lines, bytes, body) VALUES (?, ?, ?, ?, ?, ?)`,
		runID, workspaceID, now.UnixMilli(), lines, size, buf.Bytes())
	if err != nil {
		return fmt.Errorf("storing run log: %w", err)
	}
	return nil
}

type logRow struct {
	seq  int64
	body []byte
}

// LogTailRows reads the newest batches holding at least tail records,
// oldest first (the line counts pick them; older batches are not read).
func (s *Storage) LogTailRows(ctx context.Context, runID string, tail int) ([]logRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, lines, body FROM run_logs WHERE run_id = ? ORDER BY seq DESC`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading run log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []logRow
	have := 0
	for have < tail && rows.Next() {
		var r logRow
		var lines int
		if err := rows.Scan(&r.seq, &lines, &r.body); err != nil {
			return nil, err
		}
		out = append(out, r)
		have += lines
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}

// LogRows reads batches after seq, oldest first; limit 0 = all.
func (s *Storage) LogRows(ctx context.Context, runID string, after int64, limit int) ([]logRow, bool, error) {
	q := `SELECT seq, body FROM run_logs WHERE run_id = ? AND seq > ? ORDER BY seq`
	args := []any{runID, after}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit+1)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("reading run log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.seq, &r.body); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if limit > 0 && len(out) > limit {
		out, more = out[:limit], true
	}
	return out, more, nil
}
