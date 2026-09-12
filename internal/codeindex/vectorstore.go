package codeindex

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

// VectorStore stores and searches symbol embeddings, scoped per workspace.
// The default implementation is brute-force cosine over SQLite BLOBs
// (comfortable to ~300k vectors per workspace); an external vector database
// can be plugged in for monorepo scale (see QdrantVectorStore). Symbol IDs
// are the per-workspace index DB rowids.
type VectorStore interface {
	Name() string

	// Missing returns the subset of symbolIDs that has no stored vector.
	Missing(ctx context.Context, workspace string, symbolIDs []int64) ([]int64, error)

	// Add stores vectors for the given symbol IDs (same length slices).
	Add(ctx context.Context, workspace string, symbolIDs []int64, vectors [][]float32) error

	// Search returns up to k symbol IDs closest to the query vector.
	Search(ctx context.Context, workspace string, query []float32, k int) ([]int64, error)

	// DropWorkspace removes all vectors of a workspace (index teardown).
	DropWorkspace(ctx context.Context, workspace string) error
}

// ---- SQLite brute-force implementation (default) ----

// SQLiteVectorStore keeps vectors in the per-workspace index DB (`embeddings`
// table) as float32 BLOBs and scans them at query time.
type SQLiteVectorStore struct {
	store *Store
}

func NewSQLiteVectorStore(store *Store) *SQLiteVectorStore {
	return &SQLiteVectorStore{store: store}
}

func (s *SQLiteVectorStore) Name() string { return "sqlite" }

func (s *SQLiteVectorStore) Missing(ctx context.Context, workspace string, symbolIDs []int64) ([]int64, error) {
	if len(symbolIDs) == 0 {
		return nil, nil
	}
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	present := map[int64]bool{}
	const chunk = 400
	for start := 0; start < len(symbolIDs); start += chunk {
		end := start + chunk
		if end > len(symbolIDs) {
			end = len(symbolIDs)
		}
		ids := symbolIDs[start:end]
		qmarks := make([]string, len(ids))
		args := make([]any, 0, len(ids)+1)
		args = append(args, workspace)
		for i, id := range ids {
			qmarks[i] = "?"
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx,
			`SELECT e.symbol_id FROM embeddings e WHERE e.symbol_id IN (`+strings.Join(qmarks, ",")+`)`, args[1:]...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			present[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var missing []int64
	for _, id := range symbolIDs {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

func (s *SQLiteVectorStore) Add(ctx context.Context, workspace string, symbolIDs []int64, vectors [][]float32) error {
	if len(symbolIDs) != len(vectors) {
		return fmt.Errorf("ids/vectors length mismatch: %d vs %d", len(symbolIDs), len(vectors))
	}
	db, err := s.store.DB(workspace)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range symbolIDs {
		if len(vectors[i]) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO embeddings (symbol_id, vec) VALUES (?, ?)`, id, vecToBlob(vectors[i])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteVectorStore) Search(ctx context.Context, workspace string, query []float32, k int) ([]int64, error) {
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT symbol_id, vec FROM embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	query = normalize(query)
	type scored struct {
		id int64
		s  float32
	}
	var hits []scored
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		v := blobToVec(blob)
		if len(v) != len(query) {
			continue // stale rows from a different embedding model
		}
		var dot float32
		for i := range v {
			dot += v[i] * query[i]
		}
		hits = append(hits, scored{id, dot})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].s == hits[j].s {
			return hits[i].id < hits[j].id
		}
		return hits[i].s > hits[j].s
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids, nil
}

func (s *SQLiteVectorStore) DropWorkspace(ctx context.Context, workspace string) error {
	db, err := s.store.DB(workspace)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM embeddings`)
	return err
}

var (
	_ VectorStore = (*SQLiteVectorStore)(nil)
	_ VectorStore = (*QdrantVectorStore)(nil)
)

// unused guards (keeps math/sql imports stable across impl edits)
var _ = math.Float32bits
var _ = sql.ErrNoRows
