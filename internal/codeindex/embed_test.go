package codeindex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
