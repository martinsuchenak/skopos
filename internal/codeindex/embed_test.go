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
	os.WriteFile(filepath.Join(root, "svc.go"), []byte(`package main

func AuthenticateUser() bool { return checkToken() }

func checkToken() bool { return true }
`), 0o644)
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
	res, err := svc.SemanticSearch(context.Background(), "ws", "main", "auth", 10, emb)
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
	plain, err := svc.Search(context.Background(), "ws", "main", "auth", 10)
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
		json.NewEncoder(w).Encode(map[string]any{"data": arr})
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
		db.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&n)
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
	got := embedText("likeEscape", "func likeEscape(s string) string {")
	// The word "escape" must be reachable by the embedding model — the raw
	// camelCase token alone never surfaces it.
	if !strings.Contains(got, "escape") {
		t.Fatalf("split subtokens missing: %q", got)
	}
	if !strings.Contains(got, "likeEscape") || !strings.Contains(got, "func likeEscape") {
		t.Fatalf("name/signature dropped: %q", got)
	}
	// Plain lowercase names are not duplicated.
	if got := embedText("main", ""); got != "main" {
		t.Fatalf("plain name changed: %q", got)
	}
}

// linearEmbedder maps a fixed query string to a fixed vector: exact control
// over similarities in SemanticSearch tests.
type linearEmbedder struct{ q string; vec []float32 }

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
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "a.go"), []byte(`package p
func Best() {}
`), 0o644)
	// Four same-named definitions in ONE file (methods on different
	// receivers): one blob, four symbol rows, same name+path — the shape
	// of repeated CSS selectors.
	os.WriteFile(filepath.Join(root, "b.go"), []byte(`package p
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
	rows.Close()
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

	res, err := svc.SemanticSearch(context.Background(), "ws", "main", "unique", 10, &linearEmbedder{q: "unique", vec: q})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 || res.Hits[0].Name != "Best" {
		t.Fatalf("expected Best to outrank the duplicated symbol, got %+v", res.Hits)
	}
}
