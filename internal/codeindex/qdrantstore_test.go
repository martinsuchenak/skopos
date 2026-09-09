package codeindex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeQdrant implements just enough of the Qdrant REST contract for the
// adapter: collection CRUD, point upsert/get, and nearest-vector query.
type fakeQdrant struct {
	mu          sync.Mutex
	collections map[string]map[uint64][]float32 // collection -> pointID -> vector
	dims        map[string]uint64
	baseURL     string
}

func newFakeQdrant(t *testing.T) *fakeQdrant {
	t.Helper()
	f := &fakeQdrant{collections: map[string]map[uint64][]float32{}, dims: map[string]uint64{}}
	ts := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(ts.Close)
	f.baseURL = ts.URL
	return f
}

func (f *fakeQdrant) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON := func(v any) { json.NewEncoder(w).Encode(v) }
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case path == "/collections":
		names := make([]map[string]string, 0, len(f.collections))
		for name := range f.collections {
			names = append(names, map[string]string{"name": name})
		}
		writeJSON(map[string]any{"result": map[string]any{"collections": names}, "status": "ok"})
	case strings.HasPrefix(path, "/collections/"):
		rest := strings.TrimPrefix(path, "/collections/")
		parts := strings.Split(rest, "/")
		name := parts[0]
		if len(parts) == 1 {
			switch r.Method {
			case http.MethodDelete:
				delete(f.collections, name)
				delete(f.dims, name)
				writeJSON(map[string]any{"result": true, "status": "ok"})
			case http.MethodPut:
				var body struct {
					Vectors struct {
						Size uint64 `json:"size"`
					} `json:"vectors"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				f.collections[name] = map[uint64][]float32{}
				f.dims[name] = body.Vectors.Size
				writeJSON(map[string]any{"result": true, "status": "ok"})
			default: // GET collection info
				coll, ok := f.collections[name]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					writeJSON(map[string]any{"result": nil, "status": map[string]any{"error": "Not found"}})
					return
				}
				writeJSON(map[string]any{"result": map[string]any{
					"points_count": len(coll),
					"status":       "green",
					"config":       map[string]any{"params": map[string]any{"vectors": map[string]any{"size": f.dims[name]}}},
				}, "status": "ok"})
			}
			return
		}
		switch {
		case len(parts) == 2 && parts[1] == "points" && r.Method == http.MethodPost:
			var body struct {
				IDs []any `json:"ids"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			coll, ok := f.collections[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			points := make([]map[string]any, 0, len(body.IDs))
			for _, raw := range body.IDs {
				if id, ok := anyID(raw); ok {
					if _, exists := coll[uint64(id)]; exists {
						points = append(points, map[string]any{"id": id})
					}
				}
			}
			writeJSON(map[string]any{"result": points, "status": "ok"})
		case len(parts) == 3 && parts[1] == "points" && parts[2] == "query":
			coll, ok := f.collections[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body struct {
				Query struct {
					Nearest []float32 `json:"nearest"`
				} `json:"query"`
				Limit int `json:"limit"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			type scored struct {
				id    uint64
				score float32
			}
			var all []scored
			for id, v := range coll {
				var dot float32
				for i := range v {
					dot += v[i] * body.Query.Nearest[i]
				}
				all = append(all, scored{id, dot})
			}
			// sort desc
			for i := 0; i < len(all); i++ {
				for j := i + 1; j < len(all); j++ {
					if all[j].score > all[i].score {
						all[i], all[j] = all[j], all[i]
					}
				}
			}
			if body.Limit > 0 && len(all) > body.Limit {
				all = all[:body.Limit]
			}
			points := make([]map[string]any, 0, len(all))
			for _, s := range all {
				points = append(points, map[string]any{"id": s.id, "score": s.score})
			}
			writeJSON(map[string]any{"result": map[string]any{"points": points}, "status": "ok"})
		case len(parts) == 2 && parts[1] == "points" && r.Method == http.MethodPut:
			var body struct {
				Points []struct {
					ID     any       `json:"id"`
					Vector []float32 `json:"vector"`
				} `json:"points"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			coll, ok := f.collections[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			for _, p := range body.Points {
				if id, ok := anyID(p.ID); ok {
					coll[uint64(id)] = p.Vector
				}
			}
			writeJSON(map[string]any{"result": map[string]any{"status": "completed"}, "status": "ok"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestQdrantStoreAgainstFake(t *testing.T) {
	fake := newFakeQdrant(t)
	qs, err := NewQdrantVectorStore(fake.baseURL, "")
	if err != nil {
		t.Fatal(err)
	}
	if qs.Name() != "qdrant" {
		t.Fatalf("name: %s", qs.Name())
	}
	ctx := context.Background()

	ids := []int64{1, 2, 3}
	vecs := [][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0.9, 0.1}}

	// Everything missing before the collection exists.
	missing, err := qs.Missing(ctx, "ws", ids)
	if err != nil || len(missing) != 3 {
		t.Fatalf("missing pre-add: %v %v", missing, err)
	}
	if err := qs.Add(ctx, "ws", ids, vecs); err != nil {
		t.Fatal(err)
	}
	// Collection created with the right dims.
	if d, exists, _ := fakeCollectionDims(fake, "skopos-ws"); !exists || d != 3 {
		t.Fatalf("collection dims: %d %v", d, exists)
	}
	missing, err = qs.Missing(ctx, "ws", ids)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing post-add: %v %v", missing, err)
	}
	// Query shape: nearest vector first.
	found, err := qs.Search(ctx, "ws", []float32{0.95, 0.05, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0] != 1 {
		t.Fatalf("search: %v", found)
	}

	// Dimension change (embedding model swap) recreates the collection.
	if err := qs.Add(ctx, "ws", []int64{9}, [][]float32{{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := fakeCollectionDims(fake, "skopos-ws"); d != 4 {
		t.Fatalf("dims after model swap: %d", d)
	}

	// Drop is idempotent (404 tolerated).
	if err := qs.DropWorkspace(ctx, "ws"); err != nil {
		t.Fatal(err)
	}
	if err := qs.DropWorkspace(ctx, "ws"); err != nil {
		t.Fatalf("second drop must tolerate missing collection: %v", err)
	}
	if _, exists, _ := fakeCollectionDims(fake, "skopos-ws"); exists {
		t.Fatal("collection still exists after drop")
	}
}

func fakeCollectionDims(f *fakeQdrant, name string) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.dims[name]
	return d, ok, nil
}

func TestQdrantURLParsing(t *testing.T) {
	cases := []struct{ in, wantBase string }{
		{"https://qdrant.example.com", "https://qdrant.example.com"},
		{"http://localhost:6333", "http://localhost:6333"},
		{"localhost:6333", "http://localhost:6333"},
	}
	for _, c := range cases {
		qs, err := NewQdrantVectorStore(c.in, "")
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if qs.baseURL != c.wantBase {
			t.Errorf("%s -> %s, want %s", c.in, qs.baseURL, c.wantBase)
		}
	}
	if _, err := NewQdrantVectorStore("not a url at all", ""); err == nil {
		// "not a url at all" parses as a path-only URL (no host) — must be rejected
		t.Fatal("path-only url accepted")
	}
}
