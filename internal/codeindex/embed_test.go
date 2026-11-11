package codeindex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

func embedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "svc.go"), []byte(`package main

func AuthenticateUser() bool { return checkToken() }

func checkToken() bool { return true }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEmbedPendingAndFusedSearch(t *testing.T) {
	store := newTestStore(t)
	buildInto(t, store, embedRepo(t), "main")
	svc := NewService(store)
	emb := &RandomEmbedder{Dims: 32}

	// Background pass embeds the pending symbols.
	n, err := svc.EmbedPending(context.Background(), "ws", emb)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("expected symbols to embed")
	}

	// The same text embeds to the same vector, so querying with a symbol's
	// description finds it via cosine even when FTS misses it: query text
	// differs from the identifier.
	res, err := svc.SemanticSearch(context.Background(), "ws", "main", "auth", "", 10, emb)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Semantic {
		t.Fatalf("expected semantic fusion, got %+v", res)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("fused search returned nothing: %+v", res)
	}

	// Plain search stays non-semantic.
	plain, err := svc.Search(context.Background(), "ws", "main", "auth", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Semantic {
		t.Fatal("plain search must not set semantic")
	}
}

func TestOpenAIEmbedderHTTP(t *testing.T) {
	var gotModel atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		gotModel.Store(body.Model)
		arr := make([]map[string]any, len(body.Input))
		for i := range body.Input {
			arr[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": arr})
	}))
	defer ts.Close()

	e := &OpenAIEmbedder{BaseURL: ts.URL + "/v1", ModelName: "test-model"}
	vecs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 {
		t.Fatalf("vecs: %+v", vecs)
	}
	if gotModel.Load() != "test-model" {
		t.Fatalf("model not sent: %v", gotModel.Load())
	}
}

func TestEmbeddingManagerEnqueue(t *testing.T) {
	store := newTestStore(t)
	buildInto(t, store, embedRepo(t), "main")
	svc := NewService(store)
	m := NewEmbeddingManager(svc, &RandomEmbedder{Dims: 16})
	m.Enqueue("ws")
	// The worker is async; wait briefly for embeddings to appear.
	db, err := store.DB("ws")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("embedding worker did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEmbedTextSplitsIdentifiers(t *testing.T) {
	got := embedText("likeEscape", "func likeEscape(s string) string {", "", nil)
	// The word "escape" must be reachable by the embedding model — the raw
	// camelCase token alone never surfaces it.
	if !strings.Contains(got, "escape") {
		t.Fatalf("split subtokens missing: %q", got)
	}
	if !strings.Contains(got, "likeEscape") || !strings.Contains(got, "func likeEscape") {
		t.Fatalf("name/signature dropped: %q", got)
	}
	// Plain lowercase names are not duplicated.
	if got := embedText("main", "", "", nil); got != "main" {
		t.Fatalf("plain name changed: %q", got)
	}
	// Attributes join the embedding input (routing semantics).
	if got := embedText("listUsers", "", "", []string{"@app.route('/users')"}); !strings.Contains(got, "/users") {
		t.Fatalf("attrs missing from embedding input: %q", got)
	}
	// Doc summaries join the embedding input (first paragraph only).
	withDoc := embedText("sendMail", "func sendMail(u User)", "Sends the password reset email.\n\nLonger paragraphs are cut.", nil)
	if !strings.Contains(withDoc, "Sends the password reset email.") {
		t.Fatalf("doc summary missing: %q", withDoc)
	}
	if strings.Contains(withDoc, "Longer paragraphs") {
		t.Fatalf("doc tail leaked into embedding input: %q", withDoc)
	}
}

// linearEmbedder maps a fixed query string to a fixed vector: exact control
// over similarities in SemanticSearch tests.
type linearEmbedder struct {
	q   string
	vec []float32
}

func (l *linearEmbedder) Model() string { return "linear-test" }
func (l *linearEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if t == l.q {
			out[i] = l.vec
		} else {
			out[i] = []float32{0, 1}
		}
	}
	return out, nil
}

func TestSemanticSearchDuplicateKeysDoNotPoolRRF(t *testing.T) {
	store := newTestStore(t)
	svc := NewService(store)

	// One strong match (cos ~1.0 to the query) and a weaker symbol defined
	// four times under the same name+path. Pooled RRF credit would let the
	// duplicates outrank the true best match.
	root := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(`package p
func Best() {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Four same-named definitions in ONE file (methods on different
	// receivers): one blob, four symbol rows, same name+path — the shape
	// of repeated CSS selectors.
	_ = os.WriteFile(filepath.Join(root, "b.go"), []byte(`package p
type t1 struct{}
type t2 struct{}
type t3 struct{}
type t4 struct{}
func (t1) Dup() {}
func (t2) Dup() {}
func (t3) Dup() {}
func (t4) Dup() {}
`), 0o644)
	ex := parse.NewExtractor()
	resA, _ := ex.ParseFile(filepath.Join(root, "a.go"))
	resB, _ := ex.ParseFile(filepath.Join(root, "b.go"))
	resA.Path, resB.Path = "a.go", "b.go"
	if err := store.AddBlob("ws", resA); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBlob("ws", resB); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit("ws", "main", "", "test", []FileEntry{{Path: "a.go", Hash: resA.Hash}, {Path: "b.go", Hash: resB.Hash}}); err != nil {
		t.Fatal(err)
	}
	// Vectors: Best aligns with the query; the four Dup rows are 45° off.
	q := []float32{1, 0}
	dup := []float32{0.70710678, 0.70710678}
	db, _ := store.DB("ws")
	var bestID int64
	if err := db.QueryRow(`SELECT id FROM symbols WHERE name='Best'`).Scan(&bestID); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT id FROM symbols WHERE name='Dup'`)
	if err != nil {
		t.Fatal(err)
	}
	dupIDs := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			dupIDs = append(dupIDs, id)
		}
	}
	_ = rows.Close()
	if err := svc.vectors.Add(context.Background(), "ws", []int64{bestID}, [][]float32{q}); err != nil {
		t.Fatal(err)
	}
	vecs := make([][]float32, len(dupIDs))
	for i := range vecs {
		vecs[i] = dup
	}
	if err := svc.vectors.Add(context.Background(), "ws", dupIDs, vecs); err != nil {
		t.Fatal(err)
	}

	res, err := svc.SemanticSearch(context.Background(), "ws", "main", "unique", "", 10, &linearEmbedder{q: "unique", vec: q})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 || res.Hits[0].Name != "Best" {
		t.Fatalf("expected Best to outrank the duplicated symbol, got %+v", res.Hits)
	}
}

// countingEmbedder counts what is sent to the embeddings endpoint.
type countingEmbedder struct {
	RandomEmbedder
	calls atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls.Add(1)
	return c.RandomEmbedder.Embed(ctx, texts)
}

// A workspace's runner policy covers the embeddings endpoint: when it does
// not allow it, nothing is sent — no symbols, no query — and the vectors
// the workspace had are dropped; when the answer is not known yet, nothing
// is sent and nothing is dropped; allowed again, it is embedded afresh.
func TestEmbeddingsFollowThePolicyGate(t *testing.T) {
	store := newTestStore(t)
	buildInto(t, store, embedRepo(t), "main")
	svc := NewService(store)
	emb := &countingEmbedder{RandomEmbedder: RandomEmbedder{Dims: 32}}
	ctx := context.Background()

	allowed, known := true, true
	svc.SetEmbeddingsGate(func(_ context.Context, ws string) (bool, bool) {
		if ws != "ws" {
			t.Errorf("gate asked about %q", ws)
		}
		return allowed, known
	})
	if n, err := svc.EmbedPending(ctx, "ws", emb); err != nil || n == 0 || !svc.Embedded(ctx, "ws") {
		t.Fatalf("allowed: the workspace is embedded: %d %v", n, err)
	}
	if res, err := svc.SemanticSearch(ctx, "ws", "main", "auth", "", 10, emb); err != nil || !res.Semantic {
		t.Fatalf("allowed: semantic search: %+v %v", res, err)
	}

	// Not known yet (the policy could not be read): nothing sent, nothing
	// dropped.
	known = false
	before := emb.calls.Load()
	if _, err := svc.EmbedPending(ctx, "ws", emb); err != ErrEmbeddingsHeld {
		t.Fatalf("unknown: the pass is held: %v", err)
	}
	if res, err := svc.SemanticSearch(ctx, "ws", "main", "auth", "", 10, emb); err != nil || res.Semantic || len(res.Hits) == 0 {
		t.Fatalf("unknown: the text search answers alone: %+v %v", res, err)
	}
	if emb.calls.Load() != before || !svc.Embedded(ctx, "ws") {
		t.Fatal("unknown: nothing may be sent and nothing dropped")
	}

	// Refused: nothing sent, the vectors go, the status says why.
	allowed, known = false, true
	if _, err := svc.EmbedPending(ctx, "ws", emb); err != ErrEmbeddingsHeld {
		t.Fatalf("refused: the pass is held: %v", err)
	}
	if svc.Embedded(ctx, "ws") {
		t.Fatal("refused: the workspace's vectors must be dropped")
	}
	if res, err := svc.SemanticSearch(ctx, "ws", "main", "auth", "", 10, emb); err != nil || res.Semantic {
		t.Fatalf("refused: no query goes to the endpoint: %+v %v", res, err)
	}
	if emb.calls.Load() != before {
		t.Fatalf("refused: %d calls reached the embedder", emb.calls.Load()-before)
	}
	if note, _ := store.GetMeta("ws", "embedding_error"); !strings.Contains(note, "runner policy does not allow") {
		t.Fatalf("the index status must say why it is not embedded: %q", note)
	}
	// The manager records neither success nor a failure for a held pass.
	m := NewEmbeddingManager(svc, emb)
	failed := false
	m.SetErrorHandler(func(string, error) { failed = true })
	m.Enqueue("ws")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		busy := m.jobs["ws"]
		m.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failed || svc.Embedded(ctx, "ws") {
		t.Fatal("a held pass is not a failure and embeds nothing")
	}
	if note, _ := store.GetMeta("ws", "embedding_error"); !strings.Contains(note, "runner policy") {
		t.Fatalf("a held pass must not clear the note as if it had succeeded: %q", note)
	}

	// Allowed again: embedded from scratch.
	allowed = true
	if n, err := svc.EmbedPending(ctx, "ws", emb); err != nil || n == 0 || !svc.Embedded(ctx, "ws") {
		t.Fatalf("allowed again: re-embedded: %d %v", n, err)
	}
}

// A pass asked for while one is running is not lost: another runs after
// it, so a policy change that arrives mid-pass is still applied.
func TestEmbeddingManagerRunsAgainWhenAskedMidPass(t *testing.T) {
	store := newTestStore(t)
	buildInto(t, store, embedRepo(t), "main")
	svc := NewService(store)
	var allowed atomic.Bool
	allowed.Store(true)
	started, release := make(chan struct{}, 4), make(chan struct{})
	first := true
	svc.SetEmbeddingsGate(func(context.Context, string) (bool, bool) {
		if first {
			first = false
			started <- struct{}{}
			<-release // hold the first pass open
		}
		return allowed.Load(), true
	})
	m := NewEmbeddingManager(svc, &RandomEmbedder{Dims: 16})
	m.Enqueue("ws")
	<-started
	// The policy changes while the first pass is in flight.
	allowed.Store(false)
	m.Enqueue("ws")
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		idle := !m.jobs["ws"] && !m.again["ws"]
		m.mu.Unlock()
		note, _ := store.GetMeta("ws", "embedding_error")
		if idle && strings.Contains(note, "runner policy") && !svc.Embedded(context.Background(), "ws") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second pass must apply the refusal: idle=%v note=%q embedded=%v", idle, note, svc.Embedded(context.Background(), "ws"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
