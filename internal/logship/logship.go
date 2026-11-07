// Package logship sends log lines to an HTTP log intake in a standard
// format. It is an io.Writer for a logger's JSON handler (paularlott/logger's
// slog backend, or log/slog directly): every Write is one JSON record, kept
// in memory and POSTed in batches by a background goroutine — the approach
// knot takes for its log output, so the flags and formats match.
//
// Formats:
//   - ndjson: newline-delimited JSON, records unchanged. VictoriaLogs
//     (/insert/jsonline — the field-mapping query parameters are added),
//     Vector's http_server source, OpenObserve.
//   - loki: the Loki push API. Loki, Grafana Alloy, VictoriaLogs
//     (/insert/loki/api/v1/push), a knot space's agent
//     (http://localhost:12201/loki/api/v1/push).
//   - elasticsearch: the bulk API. Elasticsearch, OpenSearch, Quickwit.
//   - raw: the lines exactly as written (skopos's own run-log endpoint).
//
// A sink that is down never blocks the caller: batches that fail stay
// buffered (up to MaxBuffered lines, oldest dropped first) and go with the
// next flush; a 4xx other than 429 drops the batch (the sink will never
// take it).
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/srvurl"
)

// Format names the wire format.
type Format string

const (
	NDJSON        Format = "ndjson"
	Loki          Format = "loki"
	Elasticsearch Format = "elasticsearch"
	Raw           Format = "raw"
)

// ParseFormat accepts a format name ("" = ndjson).
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case "", "json", "jsonline":
		return NDJSON, nil
	case NDJSON, Loki, Elasticsearch, Raw:
		return f, nil
	}
	return "", fmt.Errorf("unknown log output format %q (ndjson, loki, elasticsearch)", s)
}

// Config describes one sink.
type Config struct {
	URL    string
	Format Format
	// Stream names the log stream: VictoriaLogs' stream field, Loki's job
	// label, the Elasticsearch index. A record's own "stream" field wins.
	Stream string
	// Headers go on every request (an Authorization for a sink behind a
	// proxy, the skopos key and lease for run logs).
	Headers map[string]string
	// BatchSize lines trigger an early flush (default 100); FlushEvery is
	// the longest a line waits (default 2s).
	BatchSize  int
	FlushEvery time.Duration
	// MaxBuffered bounds the lines kept while the sink fails (default 10000).
	MaxBuffered int
	Client      *http.Client
	// OnError hears every failed POST (the caller rate-limits its warning).
	OnError func(error)
}

// Writer batches records and POSTs them.
type Writer struct {
	cfg     Config
	url     string
	mu      sync.Mutex
	flushMu sync.Mutex // one flush at a time: batches arrive in order
	buf     [][]byte
	dropped int
	kick    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// New validates the sink and starts its flusher.
func New(cfg Config) (*Writer, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil {
		return nil, errors.New("log output url: not a URL; want http(s)://host/path")
	}
	if !srvurl.HTTPScheme(u.Scheme) || u.Host == "" {
		return nil, fmt.Errorf("log output url %q: want http(s)://host/path (or srv+http(s)://name/path)", u.Redacted())
	}
	if cfg.Format == "" {
		cfg.Format = NDJSON
	}
	if _, err := ParseFormat(string(cfg.Format)); err != nil {
		return nil, err
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 2 * time.Second
	}
	if cfg.MaxBuffered <= 0 {
		cfg.MaxBuffered = 10000
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Format == NDJSON {
		// VictoriaLogs reads slog's field names through these; other ndjson
		// intakes ignore query parameters they do not know.
		q := u.Query()
		for k, v := range map[string]string{"_msg_field": "msg", "_time_field": "time"} {
			if q.Get(k) == "" {
				q.Set(k, v)
			}
		}
		if cfg.Stream != "" && q.Get("_stream_fields") == "" {
			q.Set("_stream_fields", "stream")
		}
		u.RawQuery = q.Encode()
	}
	w := &Writer{cfg: cfg, url: u.String(), kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go w.run()
	return w, nil
}

// Write queues p: one record per line (slog writes one per call).
func (w *Writer) Write(p []byte) (int, error) {
	n := len(p)
	full := false
	w.mu.Lock()
	for line := range bytes.SplitSeq(p, []byte{'\n'}) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			w.buf = append(w.buf, bytes.Clone(line))
		}
	}
	w.trimLocked()
	full = len(w.buf) >= w.cfg.BatchSize
	w.mu.Unlock()
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
	return n, nil
}

// trimLocked drops the oldest lines past MaxBuffered.
func (w *Writer) trimLocked() {
	if over := len(w.buf) - w.cfg.MaxBuffered; over > 0 {
		w.buf = w.buf[over:]
		w.dropped += over
	}
}

// Dropped is how many lines were discarded (buffer full or refused).
func (w *Writer) Dropped() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

func (w *Writer) run() {
	defer close(w.done)
	t := time.NewTicker(w.cfg.FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-w.kick:
		case <-w.stop:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), w.cfg.Client.Timeout+time.Second)
		_ = w.Flush(ctx)
		cancel()
	}
}

// Flush sends everything buffered now, batch by batch; it stops at the
// first failure (the lines stay buffered) and returns that error.
func (w *Writer) Flush(ctx context.Context) error {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	for {
		w.mu.Lock()
		n := min(len(w.buf), w.cfg.BatchSize*10)
		lines := w.buf[:n:n]
		w.buf = w.buf[n:]
		w.mu.Unlock()
		if n == 0 {
			return nil
		}
		err := w.post(ctx, lines)
		if err == nil {
			continue
		}
		w.mu.Lock()
		if _, refused := errors.AsType[*refusedError](err); refused {
			w.dropped += len(lines)
		} else {
			w.buf = append(lines, w.buf...)
			w.trimLocked()
		}
		w.mu.Unlock()
		if w.cfg.OnError != nil {
			w.cfg.OnError(err)
		}
		return err
	}
}

// Close stops the flusher and sends what is left (bounded by ctx).
func (w *Writer) Close(ctx context.Context) error {
	w.once.Do(func() { close(w.stop) })
	<-w.done
	return w.Flush(ctx)
}

// refusedError: the sink answered 4xx (not 429) — resending won't help.
type refusedError struct{ status int }

func (e *refusedError) Error() string {
	return "log output refused the batch: HTTP " + strconv.Itoa(e.status)
}

func (w *Writer) post(ctx context.Context, lines [][]byte) error {
	body, ctype := encode(w.cfg.Format, w.cfg.Stream, lines, time.Now())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	for k, v := range w.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("log output: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return &refusedError{status: resp.StatusCode}
	}
	return fmt.Errorf("log output: HTTP %d", resp.StatusCode)
}

// encode renders a batch in the sink's format.
func encode(f Format, stream string, lines [][]byte, now time.Time) ([]byte, string) {
	var buf bytes.Buffer
	switch f {
	case Raw:
		for _, l := range lines {
			buf.Write(l)
			buf.WriteByte('\n')
		}
		return buf.Bytes(), "application/x-ndjson"
	case Loki:
		return encodeLoki(stream, lines, now), "application/json"
	case Elasticsearch:
		for _, l := range lines {
			rec := record(l)
			index := streamOf(rec, stream)
			if index == "" {
				index = "skopos"
			}
			rename(rec, "msg", "message")
			rename(rec, "time", "@timestamp")
			meta, _ := json.Marshal(map[string]any{"index": map[string]string{"_index": index}})
			b, _ := json.Marshal(rec)
			buf.Write(meta)
			buf.WriteByte('\n')
			buf.Write(b)
			buf.WriteByte('\n')
		}
		return buf.Bytes(), "application/x-ndjson"
	}
	for _, l := range lines {
		rec := record(l)
		if s := streamOf(rec, stream); s != "" {
			rec["stream"] = s
		}
		b, _ := json.Marshal(rec)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), "application/stream+json"
}

// encodeLoki groups the batch by stream: one Loki stream per name, labelled
// job and service_name, plus label — the field a knot agent names the
// service by in the space's log window.
func encodeLoki(stream string, lines [][]byte, now time.Time) []byte {
	type lokiStream struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	var order []string
	groups := map[string]*lokiStream{}
	for _, l := range lines {
		rec := record(l)
		ts := now
		if s, ok := rec["time"].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				ts = t
			}
			delete(rec, "time")
		}
		name := streamOf(rec, stream)
		if name == "" {
			name = "skopos"
		}
		delete(rec, "stream")
		g := groups[name]
		if g == nil {
			g = &lokiStream{Stream: map[string]string{"job": name, "service_name": name, "label": name}}
			groups[name] = g
			order = append(order, name)
		}
		b, _ := json.Marshal(rec)
		g.Values = append(g.Values, [2]string{strconv.FormatInt(ts.UnixNano(), 10), string(b)})
	}
	out := struct {
		Streams []*lokiStream `json:"streams"`
	}{}
	for _, name := range order {
		out.Streams = append(out.Streams, groups[name])
	}
	b, _ := json.Marshal(out)
	return b
}

// record decodes a JSON line; a line that is not JSON becomes its msg.
func record(line []byte) map[string]any {
	var rec map[string]any
	if json.Unmarshal(line, &rec) != nil || rec == nil {
		rec = map[string]any{"msg": string(line)}
	}
	return rec
}

func streamOf(rec map[string]any, fallback string) string {
	if s, ok := rec["stream"].(string); ok && s != "" {
		return s
	}
	return fallback
}

func rename(rec map[string]any, from, to string) {
	if v, ok := rec[from]; ok {
		rec[to] = v
		delete(rec, from)
	}
}

// ParseHeaders reads "Name: value" pairs separated by newlines or ";".
func ParseHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ';' }) {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("log output header %q: want Name: value", part)
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return out, nil
}
