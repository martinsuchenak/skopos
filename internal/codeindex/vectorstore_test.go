package codeindex

import (
	"context"
	"os"
	"testing"
)

// runVectorStoreConformance exercises the VectorStore contract against the
// given implementation. The SQLite store runs this in CI; the Qdrant store
// runs it only when SKOPOS_QDRANT_URL is set.
func runVectorStoreConformance(t *testing.T, vs VectorStore, svc *Service, store *Store) {
	t.Helper()
	ctx := context.Background()

	// Build an index so symbols exist with stable rowids.
	buildInto(t, store, embedRepo(t), "main")

	db, err := store.DB("ws")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	rows, err := db.Query(`SELECT id FROM symbols WHERE name IN ('AuthenticateUser','checkToken') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("expected 2 symbol ids, got %v", ids)
	}

	// Initially everything is missing.
	missing, err := vs.Missing(ctx, "ws", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 {
		t.Fatalf("expected 2 missing, got %v", missing)
	}

	// Add orthogonal vectors; Missing drains to zero.
	vecs := [][]float32{{1, 0, 0, 0}, {0, 1, 0, 0}}
	if err := vs.Add(ctx, "ws", ids, vecs); err != nil {
		t.Fatal(err)
	}
	missing, err = vs.Missing(ctx, "ws", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("expected none missing after Add, got %v", missing)
	}

	// Search finds the closest match.
	found, err := vs.Search(ctx, "ws", []float32{0.9, 0.1, 0, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0] != ids[0] {
		t.Fatalf("search expected %d, got %v", ids[0], found)
	}

	// Semantic search through the service works with this backend.
	svc.SetVectorStore(vs)
	if _, err := svc.EmbedPending(ctx, "ws", &RandomEmbedder{Dims: 8}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SemanticSearch(ctx, "ws", "main", "auth", 10, &RandomEmbedder{Dims: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("semantic search found nothing via %s", vs.Name())
	}

	// DropWorkspace clears the backend.
	if err := vs.DropWorkspace(ctx, "ws"); err != nil {
		t.Fatal(err)
	}
	missing, err = vs.Missing(ctx, "ws", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 {
		t.Fatalf("expected missing to refill after DropWorkspace, got %v", missing)
	}
}

func TestSQLiteVectorStoreConformance(t *testing.T) {
	store := newTestStore(t)
	svc := NewService(store)
	runVectorStoreConformance(t, NewSQLiteVectorStore(store), svc, store)
}

func TestQdrantVectorStoreConformance(t *testing.T) {
	addr := os.Getenv("SKOPOS_QDRANT_URL")
	if addr == "" {
		t.Skip("set SKOPOS_QDRANT_URL (e.g. localhost:6334) to run the Qdrant vector-store integration test")
	}
	vs, err := NewQdrantVectorStore(addr, os.Getenv("SKOPOS_QDRANT_API_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	svc := NewService(store)
	runVectorStoreConformance(t, vs, svc, store)
}

func TestServiceDropWorkspace(t *testing.T) {
	store := newTestStore(t)
	buildInto(t, store, embedRepo(t), "main")
	svc := NewService(store)

	dir := store.dir
	if err := svc.DropWorkspace(context.Background(), "ws"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath(dir, "ws") + ".db"); !os.IsNotExist(err) {
		t.Fatalf("index db still exists after DropWorkspace")
	}
	// Status on the dropped workspace starts empty again.
	status, err := svc.Status(context.Background(), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 0 {
		t.Fatalf("status after drop: %+v", status)
	}
}
