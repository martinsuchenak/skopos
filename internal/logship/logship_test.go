package logship

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/srvurl"
)

type sink struct {
	mu       sync.Mutex
	bodies   []string
	queries  []string
	types    []string
	headers  []http.Header
	statuses []int // answered in order; then 204
}

func (s *sink) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		status := http.StatusNoContent
		if len(s.statuses) > 0 {
			status, s.statuses = s.statuses[0], s.statuses[1:]
		}
		if status < 300 {
			s.bodies = append(s.bodies, string(b))
			s.queries = append(s.queries, r.URL.RawQuery)
			s.types = append(s.types, r.Header.Get("Content-Type"))
			s.headers = append(s.headers, r.Header.Clone())
		}
		s.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *sink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.bodies, "")
}

// A slog JSON handler writes through the writer; Close sends what is left,
// in ndjson with VictoriaLogs' field mapping, the stream and the headers.
func TestNDJSONThroughSlog(t *testing.T) {
	var s sink
	srv := s.server(t)
	w, err := New(Config{URL: srv.URL + "/insert/jsonline", Stream: "skopos", Headers: map[string]string{"Authorization": "Basic x"}, FlushEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(w, nil))
	log.Info("claimed", "run_id", "r1")
	log.Warn("second")
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(s.all()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	var rec map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &rec)
	if rec["msg"] != "claimed" || rec["run_id"] != "r1" || rec["stream"] != "skopos" || rec["time"] == nil {
		t.Fatalf("record: %v", rec)
	}
	q := s.queries[0]
	for _, want := range []string{"_msg_field=msg", "_time_field=time", "_stream_fields=stream"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %s", q, want)
		}
	}
	if s.headers[0].Get("Authorization") != "Basic x" {
		t.Fatal("header not sent")
	}
}

// A full batch goes out before the flush interval.
func TestBatchSizeFlushesEarly(t *testing.T) {
	var s sink
	srv := s.server(t)
	w, _ := New(Config{URL: srv.URL, Format: Raw, BatchSize: 3, FlushEvery: time.Hour})
	defer func() { _ = w.Close(context.Background()) }()
	_, _ = w.Write([]byte("a\nb\nc\n"))
	deadline := time.Now().Add(5 * time.Second)
	for s.all() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.all() != "a\nb\nc\n" {
		t.Fatalf("raw body %q", s.all())
	}
}

// A failing sink keeps the lines for the next flush; a 4xx drops them.
func TestFailureKeepsLinesRefusalDrops(t *testing.T) {
	var s sink
	s.statuses = []int{http.StatusServiceUnavailable, http.StatusBadRequest}
	srv := s.server(t)
	var errs []error
	w, _ := New(Config{URL: srv.URL, Format: Raw, FlushEvery: time.Hour, OnError: func(err error) { errs = append(errs, err) }})
	_, _ = w.Write([]byte("kept\n"))
	if err := w.Flush(context.Background()); err == nil {
		t.Fatal("503 must fail")
	}
	_, _ = w.Write([]byte("also\n"))
	if err := w.Flush(context.Background()); err == nil {
		t.Fatal("400 must fail")
	}
	if w.Dropped() != 2 || len(errs) != 2 {
		t.Fatalf("dropped %d errs %v", w.Dropped(), errs)
	}
	_, _ = w.Write([]byte("later\n"))
	_ = w.Close(context.Background())
	if s.all() != "later\n" {
		t.Fatalf("body %q", s.all())
	}
}

// The buffer is bounded: the oldest lines go first.
func TestMaxBuffered(t *testing.T) {
	w, _ := New(Config{URL: "http://127.0.0.1:1", Format: Raw, MaxBuffered: 2, FlushEvery: time.Hour, BatchSize: 100})
	_, _ = w.Write([]byte("1\n2\n3\n"))
	if w.Dropped() != 1 || string(w.buf[0]) != "2" {
		t.Fatalf("dropped %d buf %q", w.Dropped(), w.buf)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Close(ctx)
}

func TestLokiAndElasticsearch(t *testing.T) {
	now := time.Unix(100, 0)
	lines := [][]byte{
		[]byte(`{"time":"2026-11-07T10:00:00Z","level":"INFO","msg":"one"}`),
		[]byte(`{"msg":"two","stream":"other"}`),
		[]byte(`not json`),
	}
	body, _ := encode(Loki, "skopos", lines, now)
	var push struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(body, &push); err != nil || len(push.Streams) != 2 {
		t.Fatalf("loki: %s %v", body, err)
	}
	first := push.Streams[0]
	if first.Stream["job"] != "skopos" || first.Stream["label"] != "skopos" || len(first.Values) != 2 {
		t.Fatalf("first stream: %+v", first)
	}
	if first.Values[0][0] != "1794045600000000000" || !strings.Contains(first.Values[0][1], `"msg":"one"`) || strings.Contains(first.Values[0][1], "time") {
		t.Fatalf("value: %v", first.Values[0])
	}
	if first.Values[1][0] != "100000000000" || !strings.Contains(first.Values[1][1], `"msg":"not json"`) {
		t.Fatalf("non-JSON line: %v", first.Values[1])
	}
	if push.Streams[1].Stream["job"] != "other" {
		t.Fatalf("record stream wins: %+v", push.Streams[1])
	}

	es, ctype := encode(Elasticsearch, "skopos", lines[:1], now)
	want := `{"index":{"_index":"skopos"}}` + "\n" + `{"@timestamp":"2026-11-07T10:00:00Z","level":"INFO","message":"one"}` + "\n"
	if string(es) != want || ctype != "application/x-ndjson" {
		t.Fatalf("es: %q", es)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "localhost:9428"} {
		if _, err := New(Config{URL: u}); err == nil {
			t.Errorf("%q must be refused", u)
		}
	}
	if _, err := ParseFormat("gelf"); err == nil {
		t.Fatal("unknown format accepted")
	}
	if f, _ := ParseFormat(""); f != NDJSON {
		t.Fatal("default format")
	}
	h, err := ParseHeaders("Authorization: Basic abc; X-Scope-OrgID: 1")
	if err != nil || h["Authorization"] != "Basic abc" || h["X-Scope-OrgID"] != "1" {
		t.Fatalf("headers %v %v", h, err)
	}
	if _, err := ParseHeaders("nocolon"); err == nil {
		t.Fatal("bad header accepted")
	}
}

// A srv+ log output URL goes through the SRV record.
func TestSRVLogOutput(t *testing.T) {
	srvurl.Install()
	var s sink
	srv := s.server(t)
	h, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(p)
	defer srvurl.SetLookup(func(_ context.Context, name string) ([]*net.SRV, error) {
		return []*net.SRV{{Target: h + ".", Port: uint16(port)}}, nil
	})()
	w, err := New(Config{URL: "srv+http://victorialogs.service.consul/insert/jsonline", Format: Raw, FlushEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("hello\n"))
	if err := w.Close(context.Background()); err != nil || s.all() != "hello\n" {
		t.Fatalf("close %v body %q", err, s.all())
	}
}
