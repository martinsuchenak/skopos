package codeindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// QdrantVectorStore is the external vector database backend for monorepo
// scale (brute force over SQLite BLOBs is the default). It speaks Qdrant's
// REST API — no gRPC, no extra Go dependencies, and it works behind any
// HTTP reverse proxy. One collection per workspace ("skopos-<slug>"); point
// IDs are the per-workspace symbol rowids. If the embedding model changes
// dimensions, the collection is dropped and rebuilt on the next Add.
type QdrantVectorStore struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewQdrantVectorStore accepts a base URL like https://qdrant.example.com or
// http://localhost:6333, plus an optional API key.
func NewQdrantVectorStore(baseURL, apiKey string) (*QdrantVectorStore, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("parsing qdrant url %q", baseURL)
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	return &QdrantVectorStore{
		baseURL: u.String(),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (q *QdrantVectorStore) Name() string { return "qdrant" }

func (q *QdrantVectorStore) collection(workspace string) string {
	return "skopos-" + slugOf(workspace)
}

// do performs a Qdrant REST request; a 404 yields (nil, nil, true).
func (q *QdrantVectorStore) do(ctx context.Context, method, path string, body any) (data []byte, err error, notFound bool) {
	var payload io.Reader
	if body != nil {
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, marshalErr, false
		}
		payload = bytes.NewReader(raw)
	}
	req, reqErr := http.NewRequestWithContext(ctx, method, q.baseURL+path, payload)
	if reqErr != nil {
		return nil, reqErr, false
	}
	req.Header.Set("Content-Type", "application/json")
	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}
	resp, doErr := q.client.Do(req)
	if doErr != nil {
		return nil, doErr, false
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err, false
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("qdrant %s %s: %s (%s)", method, path, resp.Status, clip(data)), false
	}
	return data, nil, false
}

func clip(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// collectionDims returns (dims, exists).
func (q *QdrantVectorStore) collectionDims(ctx context.Context, collection string) (uint64, bool, error) {
	data, err, notFound := q.do(ctx, http.MethodGet, "/collections/"+collection, nil)
	if err != nil {
		return 0, false, err
	}
	if notFound {
		return 0, false, nil
	}
	var info struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size uint64 `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return 0, false, err
	}
	return info.Result.Config.Params.Vectors.Size, true, nil
}

// ensureCollection creates the collection at the given dimensionality,
// recreating it when the dimensions changed (embedding model swap).
func (q *QdrantVectorStore) ensureCollection(ctx context.Context, collection string, dims uint64) error {
	existing, exists, err := q.collectionDims(ctx, collection)
	if err != nil {
		return err
	}
	if exists {
		if existing == dims {
			return nil
		}
		if _, err, _ := q.do(ctx, http.MethodDelete, "/collections/"+collection, nil); err != nil {
			return fmt.Errorf("qdrant dropping stale collection: %w", err)
		}
	}
	body := map[string]any{
		"vectors": map[string]any{"size": dims, "distance": "Cosine"},
	}
	if _, err, _ := q.do(ctx, http.MethodPut, "/collections/"+collection, body); err != nil {
		return fmt.Errorf("qdrant create collection: %w", err)
	}
	return nil
}

func (q *QdrantVectorStore) Missing(ctx context.Context, workspace string, symbolIDs []int64) ([]int64, error) {
	if len(symbolIDs) == 0 {
		return nil, nil
	}
	collection := q.collection(workspace)
	present := map[int64]bool{}
	const chunk = 250
	for start := 0; start < len(symbolIDs); start += chunk {
		end := start + chunk
		if end > len(symbolIDs) {
			end = len(symbolIDs)
		}
		ids := symbolIDs[start:end]
		anyIDs := make([]any, len(ids))
		for i, id := range ids {
			anyIDs[i] = id
		}
		data, err, notFound := q.do(ctx, http.MethodPost, "/collections/"+collection+"/points",
			map[string]any{"ids": anyIDs, "with_payload": false, "with_vector": false})
		if err != nil {
			return nil, fmt.Errorf("qdrant get points: %w", err)
		}
		if notFound {
			return symbolIDs, nil // collection not created yet: everything missing
		}
		var res struct {
			Result []struct {
				ID any `json:"id"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, err
		}
		for _, p := range res.Result {
			if id, ok := anyID(p.ID); ok {
				present[id] = true
			}
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

func anyID(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

func (q *QdrantVectorStore) Add(ctx context.Context, workspace string, symbolIDs []int64, vectors [][]float32) error {
	if len(symbolIDs) != len(vectors) {
		return fmt.Errorf("ids/vectors length mismatch: %d vs %d", len(symbolIDs), len(vectors))
	}
	var dims uint64
	points := make([]map[string]any, 0, len(symbolIDs))
	for i, id := range symbolIDs {
		if len(vectors[i]) == 0 {
			continue
		}
		if dims == 0 {
			dims = uint64(len(vectors[i]))
		}
		points = append(points, map[string]any{"id": id, "vector": vectors[i]})
	}
	if len(points) == 0 {
		return nil
	}
	collection := q.collection(workspace)
	if err := q.ensureCollection(ctx, collection, dims); err != nil {
		return err
	}
	const chunk = 250
	for start := 0; start < len(points); start += chunk {
		end := start + chunk
		if end > len(points) {
			end = len(points)
		}
		if _, err, _ := q.do(ctx, http.MethodPut, "/collections/"+collection+"/points?wait=true",
			map[string]any{"points": points[start:end]}); err != nil {
			return fmt.Errorf("qdrant upsert: %w", err)
		}
	}
	return nil
}

func (q *QdrantVectorStore) Search(ctx context.Context, workspace string, query []float32, k int) ([]int64, error) {
	collection := q.collection(workspace)
	data, err, notFound := q.do(ctx, http.MethodPost, "/collections/"+collection+"/points/query",
		map[string]any{"query": map[string]any{"nearest": normalize(query)}, "limit": k, "with_payload": false, "with_vector": false})
	if err != nil {
		return nil, fmt.Errorf("qdrant query: %w", err)
	}
	if notFound {
		return nil, nil
	}
	var res struct {
		Result struct {
			Points []struct {
				ID any `json:"id"`
			} `json:"points"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(res.Result.Points))
	for _, p := range res.Result.Points {
		if id, ok := anyID(p.ID); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (q *QdrantVectorStore) DropWorkspace(ctx context.Context, workspace string) error {
	_, err, _ := q.do(ctx, http.MethodDelete, "/collections/"+q.collection(workspace), nil)
	return err
}
