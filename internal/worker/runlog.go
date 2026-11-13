package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/logship"
	"github.com/martinsuchenak/skopos/internal/textcut"
)

// Run logs: while a run executes, what its agent, setup and checks print is
// sent as it happens — to skopos (POST /api/runs/{id}/log, the dashboard's
// live view and the saved copy) and to the log output when one is
// configured (--log-output-url; records carry run_id, item_id, workspace,
// stage). The raw agent output still lands in <state>/runs/<id>/<phase>.log.
//
// Lines are readable text, not the agents' raw JSON: an event formatter per
// runner keeps the agent's words, its tool calls and a few lines of each
// tool result. Every line passes the redactor first (the worker's keys, push
// tokens, account credentials, env-file values).

// RunLogRecord is one line of a run log on the wire (skopos's endpoint).
type RunLogRecord struct {
	T   int64  `json:"t"`             // Unix ms
	Src string `json:"src"`           // worker | setup | check | agent
	Msg string `json:"msg"`           // one line of text
	Err bool   `json:"err,omitempty"` // stderr, or a failure
}

// maxRunLogLine: longer lines are cut into pieces of this many bytes.
const maxRunLogLine = 4000

// runLog is one run's stream; nil is a valid no-op (no server, no sink).
type runLog struct {
	run    *Run
	server *logship.Writer
	sink   *slog.Logger
	redact *strings.Replacer
	now    func() time.Time
	mu     sync.Mutex
}

type runLogKey struct{}

func withRunLog(ctx context.Context, l *runLog) context.Context {
	return context.WithValue(ctx, runLogKey{}, l)
}

func runLogFrom(ctx context.Context) *runLog {
	l, _ := ctx.Value(runLogKey{}).(*runLog)
	return l
}

// openRunLog starts the run's stream: skopos's endpoint (when the client can
// reach one) and the log output.
func (w *Worker) openRunLog(run *Run) *runLog {
	l := &runLog{run: run, redact: w.redactor(), now: time.Now}
	if w.client != nil {
		srv, err := w.client.RunLogWriter(run.ID, w.cfg.ID)
		if err != nil {
			w.log.Warn("run log: no live stream to skopos", "run", run.ID, "err", err)
		} else {
			l.server = srv
		}
	}
	if w.logOutput != nil {
		l.sink = slog.New(slog.NewJSONHandler(w.logOutput, nil)).With(
			"worker", w.cfg.ID, "run_id", run.ID, "item_id", run.ItemID, "workspace", run.WorkspaceID, "stage", run.Stage)
	}
	if l.server == nil && l.sink == nil {
		return nil
	}
	return l
}

// close sends what is left (bounded); call it before the run's final report
// is out of the way — skopos takes lines under the claim's lease.
func (l *runLog) close() {
	if l == nil || l.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = l.server.Close(ctx)
}

// flush sends what is buffered now (bounded) — before the report, which may
// end the lease the endpoint checks.
func (l *runLog) flush() {
	if l == nil || l.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = l.server.Flush(ctx)
}

// line records one or more lines of text from src.
// setRedact replaces the stream's redactor: a run learned of more secrets
// after its log opened (a credentials file that setup wrote).
func (l *runLog) setRedact(r *strings.Replacer) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.redact = r
	l.mu.Unlock()
}

func (l *runLog) line(src, text string, isErr bool) {
	if l == nil {
		return
	}
	text = strings.ToValidUTF8(strings.TrimRight(text, "\r\n"), "\uFFFD")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.redact != nil {
		text = l.redact.Replace(text)
	}
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimRight(ln, "\r")
		for _, piece := range splitBytes(ln, maxRunLogLine) {
			l.emit(src, piece, isErr)
		}
	}
}

func (l *runLog) emit(src, msg string, isErr bool) {
	if l.server != nil {
		b, _ := json.Marshal(RunLogRecord{T: l.now().UnixMilli(), Src: src, Msg: msg, Err: isErr})
		_, _ = l.server.Write(b)
	}
	if l.sink != nil {
		if isErr {
			l.sink.Warn(msg, "src", src)
		} else {
			l.sink.Info(msg, "src", src)
		}
	}
}

// notef records a worker line (what the worker is doing).
func (l *runLog) notef(format string, args ...any) {
	l.line("worker", fmt.Sprintf(format, args...), false)
}

// slowStep is how long a step may take before its end gets a line too.
const slowStep = 5 * time.Second

// stepf notes a step that may take a while (a fetch, a checkout) and
// returns its end: a slow one gets a second line with how long it took, so
// a quiet stretch in the live view always has a name.
func (l *runLog) stepf(format string, args ...any) (done func()) {
	if l == nil {
		return func() {}
	}
	what := fmt.Sprintf(format, args...)
	l.notef("%s …", what)
	start := l.now()
	return func() {
		if d := l.now().Sub(start); d >= slowStep {
			l.notef("%s: done in %s", what, d.Round(time.Second))
		}
	}
}

// splitBytes cuts s into pieces of at most n bytes on rune boundaries; an
// empty s is one empty line (blank lines keep the output's shape).
func splitBytes(s string, n int) []string {
	out := []string{}
	for {
		piece := textcut.Truncate(s, n)
		out = append(out, piece)
		if s = s[len(piece):]; s == "" {
			return out
		}
	}
}

// writer is an io.Writer for a process's output: complete lines go through
// format (nil = the line itself) into the log. Flush at the end sends a
// last line without a newline.
func (l *runLog) writer(src string, isErr bool, format func([]byte) []string) *lineWriter {
	return &lineWriter{log: l, src: src, isErr: isErr, format: format}
}

type lineWriter struct {
	log    *runLog
	src    string
	isErr  bool
	format func([]byte) []string
	mu     sync.Mutex
	buf    []byte
	// skipping: inside an event line too large to format, dropped up to its
	// newline.
	skipping bool
}

// A line still waiting for its newline: plain output (setup, checks) goes
// out in pieces past maxPending; an agent event is one JSON line that must
// be whole to format, so it may grow to maxPendingEvent, and past that it
// is left out with a note — never sent as raw JSON (a base64 image would
// fill the run's log).
const (
	maxPending      = 1 << 20
	maxPendingEvent = 32 << 20
)

func (w *lineWriter) Write(p []byte) (int, error) {
	if w == nil || w.log == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if !w.skipping {
			w.handle(w.buf[:i])
		}
		w.skipping = false
		w.buf = w.buf[i+1:]
	}
	switch {
	case w.format == nil && len(w.buf) > maxPending:
		w.handle(w.buf)
		w.buf = nil
	case w.format != nil && len(w.buf) > maxPendingEvent && !w.skipping:
		w.log.line(w.src, fmt.Sprintf("(an agent event over %d MB was left out of the log)", maxPendingEvent>>20), false)
		w.buf, w.skipping = nil, true
	case w.skipping:
		w.buf = nil
	}
	return len(p), nil
}

// Flush sends a trailing partial line.
func (w *lineWriter) Flush() {
	if w == nil || w.log == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(bytes.TrimSpace(w.buf)) > 0 && !w.skipping {
		w.handle(w.buf)
	}
	w.buf, w.skipping = nil, false
}

func (w *lineWriter) handle(line []byte) {
	if w.format == nil {
		w.log.line(w.src, string(line), w.isErr)
		return
	}
	for _, out := range w.format(line) {
		w.log.line(w.src, out, w.isErr)
	}
}

// teeOutput: the process's stdout and stderr go to their buffers and, live,
// to the run log (stdout through the runner's formatter). done flushes.
func teeOutput(ctx context.Context, stdout, stderr io.Writer, format func([]byte) []string) (io.Writer, io.Writer, func()) {
	l := runLogFrom(ctx)
	if l == nil {
		return stdout, stderr, func() {}
	}
	out := l.writer("agent", false, format)
	errw := l.writer("agent", true, nil)
	return io.MultiWriter(stdout, out), io.MultiWriter(stderr, errw), func() { out.Flush(); errw.Flush() }
}

// --- redaction ---

// AddSecrets registers values the run logs must never carry (the worker's
// keys, the log output's header); the env-file values, push tokens and
// account credentials are added by the worker itself.
func (w *Worker) AddSecrets(values ...string) { w.extraSecrets = append(w.extraSecrets, values...) }

// redactor replaces every secret value with [redacted]: values of 8+
// characters (shorter ones would mangle ordinary text), longest first.
func (w *Worker) redactor() *strings.Replacer {
	seen := map[string]bool{}
	var vals []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if len(v) >= 8 && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
			// The same value as it stands inside JSON (a quote, a
			// backslash, <, > or & in it are escaped there): agent CLIs
			// print their config and their events as JSON.
			if b, err := json.Marshal(v); err == nil {
				if esc := string(b[1 : len(b)-1]); esc != v && !seen[esc] {
					seen[esc] = true
					vals = append(vals, esc)
				}
			}
		}
	}
	for _, v := range w.extraSecrets {
		add(v)
	}
	for _, v := range w.runSecrets {
		add(v)
	}
	for _, k := range w.cfg.TokenEnvs() {
		add(os.Getenv(k))
	}
	for _, k := range w.envFileKeys {
		add(os.Getenv(k))
	}
	for _, a := range w.cfg.FileAccounts() {
		for k, v := range a.Env {
			if secretName(k) {
				add(v)
			}
		}
	}
	if len(vals) == 0 {
		return nil
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	pairs := make([]string, 0, 2*len(vals))
	for _, v := range vals {
		pairs = append(pairs, v, "[redacted]")
	}
	return strings.NewReplacer(pairs...)
}

// secretName: an account env variable that holds a credential (an API key,
// a token) rather than a path or a model name.
func secretName(k string) bool {
	k = strings.ToUpper(k)
	return strings.Contains(k, "KEY") || strings.Contains(k, "TOKEN") || strings.Contains(k, "SECRET") || strings.Contains(k, "PASS")
}

// --- agent event formatters: one JSON event line → readable lines ---

// toolResultLines is how much of a tool's output the log keeps.
const toolResultLines = 6

// claudeEventLines formats Claude Code's stream-json events: the agent's
// text, each tool call (name and its main argument) and the head of each
// result. System, rate-limit and result events print nothing (the worker
// notes the outcome itself).
func claudeEventLines(line []byte) []string {
	var ev struct {
		Type     string          `json:"type"`
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Message  json.RawMessage `json:"message"` // an object on assistant/user, text on some system events
	}
	if json.Unmarshal(line, &ev) != nil {
		return unparsed(line)
	}
	if ev.Type == "system" && ev.Subtype == "permission_denied" {
		// The tool result that follows carries the denial's text.
		return []string{"▸ " + ev.ToolName + " denied (not in this stage's tools)"}
	}
	if ev.Type != "assistant" && ev.Type != "user" {
		return nil
	}
	var msg struct {
		Content []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"is_error"`
		} `json:"content"`
	}
	if json.Unmarshal(ev.Message, &msg) != nil {
		return nil
	}
	var out []string
	for _, c := range msg.Content {
		switch {
		case ev.Type == "assistant" && c.Type == "text":
			if t := strings.TrimSpace(c.Text); t != "" {
				out = append(out, t)
			}
		case ev.Type == "assistant" && c.Type == "tool_use" && c.Name == "StructuredOutput":
			out = append(out, "▸ returns the stage result") // the worker reports what it holds
		case ev.Type == "assistant" && c.Type == "tool_use":
			out = append(out, "▸ "+c.Name+toolArg(c.Input))
		case ev.Type == "user" && c.Type == "tool_result":
			out = append(out, resultHead(toolResultText(c.Content), c.IsError)...)
		}
	}
	return out
}

// unparsed: a line that is not an event is shown as it is, unless it looks
// like one (a cut-off or malformed event is never shown as raw JSON).
func unparsed(line []byte) []string {
	if t := bytes.TrimSpace(line); len(t) > 0 && t[0] == '{' {
		return nil
	}
	return []string{string(line)}
}

// toolArg picks the argument that says what a tool call does.
func toolArg(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil || len(in) == 0 {
		return ""
	}
	for _, k := range []string{"command", "file_path", "filePath", "path", "pattern", "query", "url", "description", "q"} {
		if v, ok := in[k].(string); ok && v != "" {
			return " " + oneLine(v, 300)
		}
	}
	b, _ := json.Marshal(in)
	return " " + oneLine(string(b), 300)
}

// toolResultText reads a tool_result's content: a string or text blocks.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// resultHead is the first lines of a tool result, indented, with a count of
// what was left out.
func resultHead(text string, isErr bool) []string {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	mark := "  ⎿ "
	if isErr {
		mark = "  ⎿ error: "
	}
	var out []string
	for i, l := range lines {
		if i == toolResultLines {
			out = append(out, fmt.Sprintf("    … %d more lines", len(lines)-toolResultLines))
			break
		}
		prefix := "    "
		if i == 0 {
			prefix = mark
		}
		out = append(out, prefix+oneLine(l, 400))
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	if len(s) > n {
		s = textcut.Truncate(s, n) + "…"
	}
	return s
}

// opencodeEventLines formats opencode's --format json events.
func opencodeEventLines(line []byte) []string {
	var ev struct {
		Type string `json:"type"`
		Part struct {
			Text  string `json:"text"`
			Tool  string `json:"tool"`
			State struct {
				Status string          `json:"status"`
				Input  json.RawMessage `json:"input"`
				Output string          `json:"output"`
				Error  string          `json:"error"`
			} `json:"state"`
		} `json:"part"`
		Error struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return unparsed(line)
	}
	switch ev.Type {
	case "text":
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			return []string{t}
		}
	case "tool_use":
		out := []string{"▸ " + ev.Part.Tool + toolArg(ev.Part.State.Input)}
		if ev.Part.State.Error != "" {
			return append(out, resultHead(ev.Part.State.Error, true)...)
		}
		return append(out, resultHead(ev.Part.State.Output, false)...)
	case "error":
		return []string{"error: " + strings.TrimSpace(ev.Error.Name+": "+ev.Error.Data.Message)}
	}
	return nil
}

// codexEventLines formats codex exec --json events (completed items).
func codexEventLines(line []byte) []string {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type             string `json:"type"`
			Text             string `json:"text"`
			Command          string `json:"command"`
			AggregatedOutput string `json:"aggregated_output"`
			ExitCode         *int   `json:"exit_code"`
			Server           string `json:"server"`
			Tool             string `json:"tool"`
			Query            string `json:"query"`
			Changes          []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return unparsed(line)
	}
	switch ev.Type {
	case "item.started":
		if ev.Item.Type == "command_execution" {
			return []string{"▸ shell " + oneLine(ev.Item.Command, 300)}
		}
	case "item.completed":
		it := ev.Item
		switch it.Type {
		case "agent_message", "reasoning":
			if t := strings.TrimSpace(it.Text); t != "" {
				return []string{t}
			}
		case "command_execution":
			failed := it.ExitCode != nil && *it.ExitCode != 0
			return resultHead(it.AggregatedOutput, failed)
		case "mcp_tool_call":
			return []string{"▸ " + it.Server + "." + it.Tool}
		case "web_search":
			return []string{"▸ web search " + oneLine(it.Query, 300)}
		case "file_change":
			var out []string
			for _, c := range it.Changes {
				out = append(out, "▸ "+c.Kind+" "+c.Path)
			}
			return out
		}
	case "error":
		return []string{"error: " + oneLine(ev.Message, 400)}
	case "turn.failed":
		return []string{"error: " + oneLine(ev.Error.Message, 400)}
	}
	return nil
}
