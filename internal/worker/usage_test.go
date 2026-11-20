package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Claude Code's result event (2.1.318, trimmed, from a run whose subagent
// ran after the first result): the counts are modelUsage's, summed over the
// models (the result's own usage leaves the subagent out and covers only
// the last result's turns), each model keeps its counts without its limits
// or price, and the turns are every result event's.
func TestClaudeUsage(t *testing.T) {
	var out claudeJSON
	if err := json.Unmarshal([]byte(`{"type":"result","num_turns":1,"duration_api_ms":4752,"total_cost_usd":0.0057,
		"usage":{"input_tokens":4,"cache_creation_input_tokens":714,"cache_read_input_tokens":33404,"output_tokens":16,
			"server_tool_use":{"web_search_requests":0},"iterations":[{"input_tokens":2}],"fallback_credit":null},
		"modelUsage":{
			"claude-haiku-5-5":{"inputTokens":12,"outputTokens":410,"cacheReadInputTokens":126014,"cacheCreationInputTokens":30018,
				"costUSD":0.0057,"contextWindow":1000000,"maxOutputTokens":128000,"thinkingTokens":7,"webSearchRequests":1,"canonicalModel":"claude-haiku-5-5"},
			"claude-opus-5-5":{"inputTokens":3,"outputTokens":5,"costUSD":1.0}},
		"subagent_stats":{"spawned":1}}`), &out); err != nil {
		t.Fatal(err)
	}
	u := out.usage(4)
	want := map[string]int64{
		"input_tokens": 15, "output_tokens": 415, "cache_read_tokens": 126014, "cache_write_tokens": 30018, "reasoning_tokens": 7,
		"web_search_requests": 1, "turns": 4, "api_ms": 4752, "subagents": 1,
	}
	if len(u.Counts) != len(want) {
		t.Fatalf("counts: %v", u.Counts)
	}
	for k, v := range want {
		if u.Counts[k] != v {
			t.Fatalf("%s = %d, want %d: %v", k, u.Counts[k], v, u.Counts)
		}
	}
	m := u.Models["claude-haiku-5-5"]
	if m["input_tokens"] != 12 || m["cache_write_tokens"] != 30018 || m["context_window"] != 0 || m["max_output_tokens"] != 0 || m["cost_usd"] != 0 ||
		u.Models["claude-opus-5-5"]["cost_usd"] != 0 || u.Models["claude-opus-5-5"]["output_tokens"] != 5 {
		t.Fatalf("each model's counts, without limits or price: %v", u.Models)
	}
	// A CLI that prints no modelUsage: the result's own usage, and its turns.
	var old claudeJSON
	_ = json.Unmarshal([]byte(`{"type":"result","num_turns":2,"usage":{"input_tokens":2,"cache_read_input_tokens":9,"output_tokens":4,"output_tokens_details":{"thinking_tokens":1}}}`), &old)
	if u := old.usage(0); u.Counts["input_tokens"] != 2 || u.Counts["cache_read_tokens"] != 9 || u.Counts["reasoning_tokens"] != 1 || u.Counts["turns"] != 2 {
		t.Fatalf("without modelUsage: %v", u.Counts)
	}
}

// The catcher sums the turns of every result event.
func TestClaudeCatcherSumsTurns(t *testing.T) {
	c := &claudeResultCatcher{}
	_, _ = c.Write([]byte(`{"type":"result","num_turns":3}` + "\n" + `{"type":"assistant"}` + "\n" + `{"type":"result","num_turns":1}`))
	if r := c.result(); !strings.Contains(string(r), `"num_turns":1`) || c.turnsTotal() != 4 {
		t.Fatalf("the last result, every result's turns: %s %d", r, c.turnsTotal())
	}
}

// codex's turn.completed usage is the session's total: a new session's is
// the call's own (cached input taken out of input_tokens), a resumed one's
// less what the session file held before the call, and a resumed call
// whose earlier total is unknown reports turns only. The model comes from
// the session file.
func TestCodexUsage(t *testing.T) {
	ev := parseCodexEvents([]byte(`{"type":"thread.started","thread_id":"t"}
{"type":"turn.completed","usage":{"input_tokens":33328,"cached_input_tokens":23680,"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":3}}
`), time.Now())
	if ev.Usage.Counts["input_tokens"] != 33328-23680 || ev.Usage.Counts["cache_read_tokens"] != 23680 ||
		ev.Usage.Counts["reasoning_tokens"] != 3 || ev.Usage.Counts["turns"] != 1 {
		t.Fatalf("the turn's usage: %v", ev.Usage.Counts)
	}
	after := codexSession{Model: "gpt-6-sol"}
	u := codexRunUsage(ev.Usage, false, codexSession{}, after)
	if u.Counts["output_tokens"] != 10 || u.Models["gpt-6-sol"]["output_tokens"] != 10 {
		t.Fatalf("a new session: %+v", u)
	}
	before := codexSession{Total: map[string]int64{"input_tokens": 16655 - 7168, "cache_read_tokens": 7168, "output_tokens": 5}}
	u = codexRunUsage(ev.Usage, true, before, after)
	if u.Counts["input_tokens"] != (33328-23680)-(16655-7168) || u.Counts["cache_read_tokens"] != 23680-7168 ||
		u.Counts["output_tokens"] != 5 || u.Counts["reasoning_tokens"] != 3 {
		t.Fatalf("a resumed session: the call's share: %v", u.Counts)
	}
	u = codexRunUsage(ev.Usage, true, codexSession{}, codexSession{})
	if len(u.Counts) != 1 || u.Counts["turns"] != 1 {
		t.Fatalf("a resumed session with no earlier total: turns only: %v", u.Counts)
	}
}

// The session file is found by the thread id's day and read for the
// newest turn's model and the newest total.
func TestCodexSessionFile(t *testing.T) {
	home := t.TempDir()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	sec, nsec := id.Time().UnixTime()
	day := time.Unix(sec, nsec)
	dir := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-11-20T08-38-05-"+id.String()+".jsonl")
	lines := `{"type":"session_meta","payload":{"model_provider":"openai"}}
{"type":"turn_context","payload":{"model":"gpt-6-sol","effort":"medium"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":16655,"cached_input_tokens":7168,"output_tokens":5}}}}
{"type":"turn_context","payload":{"model":{"unexpected":"object"}}}
{"type":"turn_context","payload":{"model":"gpt-6-luna"}}
{"type":"event_msg","payload":{"type":"token_count","info":"not an object"}}
{"type":"event_msg","payload":{"type":"token_count","info":null}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":33328,"cached_input_tokens":23680,"output_tokens":10}}}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexSessionFile(home, id.String()); got != path {
		t.Fatalf("found by the thread's day: %q", got)
	}
	s := readCodexSession(path, 0)
	if s.Model != "gpt-6-luna" || s.Total["cache_read_tokens"] != 23680 || s.Total["input_tokens"] != 33328-23680 {
		t.Fatalf("the newest model and total, past a line that does not decode: %+v", s)
	}
	// Before a resumed call: the total at the end, and where the call's
	// lines will start; after it, only those lines are read.
	before, offset := codexSessionBefore(path)
	if before.Total["output_tokens"] != 10 || offset != int64(len(lines)) {
		t.Fatalf("before: %+v %d", before, offset)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"turn_context","payload":{"model":"gpt-6-sol"}}` + "\n")
	_ = f.Close()
	if after := readCodexSession(path, offset); after.Model != "gpt-6-sol" || after.Total != nil {
		t.Fatalf("after: only the call's own lines: %+v", after)
	}
	// From the middle of a line: that line is skipped, not misread.
	if mid := readCodexSession(path, int64(strings.Index(lines, "gpt-6-luna"))); mid.Model != "gpt-6-sol" || mid.Total["output_tokens"] != 10 {
		t.Fatalf("from inside a line: %+v", mid)
	}
	if codexSessionFile(home, "not-a-uuid") != "" || codexSessionFile(home, uuid.NewString()) != "" {
		t.Fatal("no file for an id that is not a v7 or not there")
	}
}

// opencode reports each model call's tokens on step_finish; agy its usage
// object and turns.
func TestOpencodeAndAgyUsage(t *testing.T) {
	res := parseOpencodeEvents([]byte(`{"type":"step_finish","part":{"cost":0.1,"tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":300,"write":40}}}}
{"type":"step_finish","part":{"cost":0.2,"tokens":{"input":50,"output":10,"reasoning":0,"cache":{"read":0,"write":0}}}}
`))
	c := res.Usage.Counts
	if c["input_tokens"] != 150 || c["output_tokens"] != 30 || c["reasoning_tokens"] != 5 || c["cache_read_tokens"] != 300 ||
		c["cache_write_tokens"] != 40 || c["turns"] != 2 {
		t.Fatalf("opencode: %v", c)
	}
	agy := antigravityJSON{NumTurns: 2, Usage: json.RawMessage(`{"input_tokens":30903,"output_tokens":12}`)}
	if u := agy.usage(); u.Counts["input_tokens"] != 30903 || u.Counts["turns"] != 2 {
		t.Fatalf("agy: %v", u.Counts)
	}
}

// The run log's line, and claude's camelCase names.
func TestUsageBrief(t *testing.T) {
	u := &Usage{Counts: map[string]int64{"input_tokens": 12345, "cache_read_tokens": 4_100_000, "output_tokens": 980}}
	if got := usageBrief(u); got != "tokens: 12k in, 4.1M from cache, 980 out" {
		t.Fatalf("brief: %q", got)
	}
	u.ForModel("sonnet")
	if got := usageBrief(u); !strings.HasSuffix(got, " (sonnet)") {
		t.Fatalf("brief with the model: %q", got)
	}
	if usageBrief(&Usage{Counts: map[string]int64{"agent_calls": 1}}) != "" {
		t.Fatal("no tokens, no line")
	}
	if snake("costUSD") != "cost_usd" || snake("cacheCreationEphemeral5mInputTokens") != "cache_creation_ephemeral5m_input_tokens" {
		t.Fatal("snake")
	}
}
