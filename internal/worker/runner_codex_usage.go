package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// What a codex run used. codex exec ends a turn with turn.completed, whose
// usage is the whole session's so far: a resumed session counts its earlier
// turns again. The session file (CODEX_HOME/sessions/YYYY/MM/DD/
// rollout-…-<thread id>.jsonl) records that total after every model call
// and, in each turn's context, the model codex used — which a run that
// names no model learns nowhere else.

// codexUsage turns turn.completed's usage into the run's counts. Codex
// counts cached input inside input_tokens; here input_tokens is the part
// not read from the cache, as for every runner.
func codexUsage(raw json.RawMessage) map[string]int64 {
	c := usageFrom(raw)
	if c == nil {
		return nil
	}
	if in, cached := c["input_tokens"], c["cache_read_tokens"]; cached > 0 && in >= cached {
		c["input_tokens"] = in - cached
	}
	return c
}

// codexSessionFile finds a thread's session file. The thread id is a
// UUIDv7, so it says when the session began, and the session's folder is
// that day's (local time; the next and previous day cover a time zone
// difference).
func codexSessionFile(home, threadID string) string {
	id, err := uuid.Parse(threadID)
	if err != nil || home == "" || id.Version() != 7 {
		return ""
	}
	sec, nsec := id.Time().UnixTime()
	began := time.Unix(sec, nsec)
	for _, t := range []time.Time{began, began.UTC(), began.AddDate(0, 0, -1), began.AddDate(0, 0, 1)} {
		dir := filepath.Join(home, "sessions", t.Format("2006"), t.Format("01"), t.Format("02"))
		matches, _ := filepath.Glob(filepath.Join(dir, "rollout-*-"+threadID+".jsonl"))
		if len(matches) == 1 {
			return matches[0]
		}
	}
	return ""
}

// codexSession is what a session file says: the model of its newest turn
// and the session's token total after its newest model call.
type codexSession struct {
	Model string
	Total map[string]int64
}

// codexTail: how much of a session file's end is read for the total the
// session had used before a resumed call (a token count follows every
// model call, so the last one is near the end).
const codexTail = 1 << 20

// readCodexSession reads a session file from offset from (0 = all; a
// partial first line is skipped) to its end, line by line: only lines that
// can carry a turn's model or a token count are decoded, and a line that
// does not decode is passed over, never the end of the read.
func readCodexSession(path string, from int64) codexSession {
	var s codexSession
	if path == "" {
		return s
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer func() { _ = f.Close() }()
	partial := false
	if from > 0 {
		// The byte before from says whether from starts a line.
		b := make([]byte, 1)
		if _, err := f.ReadAt(b, from-1); err != nil {
			return s
		}
		partial = b[0] != '\n'
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return s
		}
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if partial {
			partial = false
		} else if bytes.Contains(line, []byte(`"turn_context"`)) || bytes.Contains(line, []byte(`"token_count"`)) {
			var l struct {
				Type    string `json:"type"`
				Payload struct {
					Type  string `json:"type"`
					Model string `json:"model"`
					Info  struct {
						Total json.RawMessage `json:"total_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &l) == nil {
				switch {
				case l.Type == "turn_context" && l.Payload.Model != "":
					s.Model = strings.TrimSpace(l.Payload.Model)
				case l.Type == "event_msg" && l.Payload.Type == "token_count" && len(l.Payload.Info.Total) > 0:
					if t := codexUsage(l.Payload.Info.Total); t != nil {
						s.Total = t
					}
				}
			}
		}
		if err != nil {
			return s
		}
	}
}

// codexSessionBefore is what a session file says before a resumed call:
// the session's total, read from the file's end (all of it when the end
// holds no token count), and the file's size, where the call's own lines
// will start.
func codexSessionBefore(path string) (codexSession, int64) {
	fi, err := os.Stat(path)
	if path == "" || err != nil {
		return codexSession{}, 0
	}
	size := fi.Size()
	s := readCodexSession(path, max(0, size-codexTail))
	if s.Total == nil && size > codexTail {
		s = readCodexSession(path, 0)
	}
	return s, size
}

// codexRunUsage is what this call used: the turn's usage (turn, as
// parseCodexEvents read it: the session's total, and the call's turns)
// less what the session had used before the call (before: the session
// file when a resumed call started), under the model the session file
// names after it. A resumed call whose earlier total is unknown reports
// no tokens: the session's total would count the earlier calls again.
func codexRunUsage(turn *Usage, resumed bool, before, after codexSession) *Usage {
	if turn == nil {
		return nil
	}
	u := &Usage{}
	for k, v := range turn.Counts {
		if k == "turns" {
			u.Count(k, v)
		} else if !resumed || before.Total != nil {
			u.Count(k, v-before.Total[k])
		}
	}
	if after.Model != "" {
		u.ForModel(after.Model)
	}
	if u.Empty() {
		return nil
	}
	return u
}
