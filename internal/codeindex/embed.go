package codeindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Embedder produces vectors for symbol descriptions. Any OpenAI-compatible
// /v1/embeddings endpoint works — including local Ollama / LM Studio, which
// keeps embeddings fully local without CGO.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

// OpenAIEmbedder talks to an OpenAI-compatible embeddings endpoint.
type OpenAIEmbedder struct {
	BaseURL   string // e.g. http://localhost:11434/v1 or https://api.openai.com/v1
	ModelName string
	APIKey    string // optional (local servers need none)
}

func (e *OpenAIEmbedder) Model() string { return e.ModelName }

func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.ModelName, "input": texts})
	url := strings.TrimRight(e.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding endpoint returned %s", resp.Status)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding embeddings: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding endpoint returned %d of %d vectors", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		vecs[d.Index] = normalize(d.Embedding)
	}
	return vecs, nil
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// ---- vector storage (brute-force SQLite BLOBs) ----

// embeddableKinds are the symbol kinds worth embedding (definitions, not
// every type alias).
var embeddableKinds = map[string]bool{
	"func": true, "method": true, "class": true, "interface": true,
	"struct": true, "enum": true, "trait": true,
}

func vecToBlob(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func blobToVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// EmbedPending embeds symbols of a workspace that lack vectors (bounded per
// run). Called asynchronously after commits and directly by tests.
func (s *Service) EmbedPending(ctx context.Context, workspace string, embedder Embedder) (int, error) {
	db, err := s.store.DB(workspace)
	if err != nil {
		return 0, err
	}
	rows, err := db.Query(`
		SELECT s.id, s.name, s.kind, s.signature FROM symbols s
		LEFT JOIN embeddings e ON e.symbol_id = s.id
		WHERE e.symbol_id IS NULL AND s.name != ''
		LIMIT 256`)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id   int
		text string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		var name, kind, sig string
		if err := rows.Scan(&p.id, &name, &kind, &sig); err != nil {
			rows.Close()
			return 0, err
		}
		if !embeddableKinds[kind] {
			continue
		}
		text := name
		if sig != "" {
			text += "\n" + sig
		}
		p.text = text
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	texts := make([]string, len(batch))
	for i, p := range batch {
		texts[i] = p.text
	}
	vecs, err := embedder.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for i, p := range batch {
		if len(vecs[i]) == 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO embeddings (symbol_id, vec) VALUES (?, ?)`, p.id, vecToBlob(vecs[i])); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(batch), nil
}

// VectorSearch returns top-k symbol ids (and cosine scores) closest to the
// query vector. Brute force over the workspace's stored vectors.
func (s *Service) VectorSearch(ctx context.Context, workspace string, query []float32, k int) ([]int, error) {
	db, err := s.store.DB(workspace)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT symbol_id, vec FROM embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	query = normalize(query)
	type scored struct {
		id int
		s  float32
	}
	var hits []scored
	for rows.Next() {
		var id int
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		v := blobToVec(blob)
		if len(v) != len(query) {
			continue // different embedding model dimensions: skip stale rows
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
	sort.Slice(hits, func(i, j int) bool { return hits[i].s > hits[j].s })
	if len(hits) > k {
		hits = hits[:k]
	}
	ids := make([]int, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids, nil
}

// SemanticSearch fuses FTS and vector results with Reciprocal Rank Fusion.
// When no embeddings exist the result degrades to the plain FTS search with
// semantic=false.
func (s *Service) SemanticSearch(ctx context.Context, workspace, branch, query string, limit int, embedder Embedder) (*SearchResults, error) {
	base, err := s.Search(ctx, workspace, branch, query, limit)
	if err != nil || embedder == nil {
		return base, err
	}
	vecs, err := embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		return base, nil // embedding unavailable: graceful degradation
	}
	db, err := s.store.DB(workspace)
	if err != nil {
		return base, nil
	}
	resolved := base.Branch
	if base.Fallback {
		// resolveBranch labeled the fallback; recompute the resolved name.
		resolved = s.store.DefaultBranch(workspace)
	}
	ids, err := s.VectorSearch(ctx, workspace, vecs[0], 50)
	if err != nil || len(ids) == 0 {
		return base, nil
	}
	// Map vector hits to symbols visible on the branch.
	qmarks := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, resolved)
	for i, id := range ids {
		qmarks[i] = "?"
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT DISTINCT s.id, s.name, s.kind, bf.path, s.line, s.signature, s.lang
		FROM symbols s
		JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?
		WHERE s.id IN (`+strings.Join(qmarks, ",")+`)
		ORDER BY bf.path, s.line`, args...)
	if err != nil {
		return base, nil
	}
	defer rows.Close()
	var vecHits []SymbolHit
	idOrder := map[int]int{}
	for i, id := range ids {
		idOrder[id] = i
	}
	for rows.Next() {
		var id int
		var h SymbolHit
		if err := rows.Scan(&id, &h.Name, &h.Kind, &h.Path, &h.Line, &h.Signature, &h.Lang); err != nil {
			return base, nil
		}
		h.rank = idOrder[id]
		vecHits = append(vecHits, h)
	}

	// RRF fusion (k=60) over FTS and vector rankings.
	const rrfK = 60
	score := map[string]float64{}
	merged := map[string]SymbolHit{}
	for i, h := range base.Hits {
		key := h.Name + "\x00" + h.Path
		score[key] += 1.0 / float64(rrfK+i+1)
		merged[key] = h
	}
	for i, h := range vecHits {
		key := h.Name + "\x00" + h.Path
		score[key] += 1.0 / float64(rrfK+i+1)
		if _, ok := merged[key]; !ok {
			merged[key] = h
		}
	}
	keys := make([]string, 0, len(score))
	for k := range score {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return score[keys[i]] > score[keys[j]] })
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := &SearchResults{Branch: base.Branch, Fallback: base.Fallback, Note: base.Note, Semantic: true}
	for _, k := range keys {
		out.Hits = append(out.Hits, merged[k])
	}
	return out, nil
}

// ---- async embedding worker ----

// EmbeddingManager runs embedding in the background after commits when an
// embedder is configured.
type EmbeddingManager struct {
	service  *Service
	embedder Embedder

	mu   sync.Mutex
	jobs map[string]bool
}

func NewEmbeddingManager(service *Service, embedder Embedder) *EmbeddingManager {
	return &EmbeddingManager{service: service, embedder: embedder, jobs: map[string]bool{}}
}

// Enqueue schedules an embedding pass for a workspace (deduplicated).
func (m *EmbeddingManager) Enqueue(workspace string) {
	if m == nil || m.embedder == nil {
		return
	}
	m.mu.Lock()
	if m.jobs[workspace] {
		m.mu.Unlock()
		return
	}
	m.jobs[workspace] = true
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.jobs, workspace)
			m.mu.Unlock()
		}()
		// Loop until the pending set is drained (commits race the worker).
		for i := 0; i < 100; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			n, err := m.service.EmbedPending(ctx, workspace, m.embedder)
			cancel()
			if err != nil || n == 0 {
				return
			}
		}
	}()
}

// RandomEmbedder is a deterministic test embedder: hash-seeded pseudo-random
// vectors (no network). NOT a real semantic model — test use only.
type RandomEmbedder struct{ Dims int }

func (r *RandomEmbedder) Model() string { return "random-test" }

func (r *RandomEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		rnd := rand.New(rand.NewSource(int64(hashText(t))))
		v := make([]float32, r.Dims)
		for j := range v {
			v[j] = rnd.Float32()
		}
		out[i] = normalize(v)
	}
	return out, nil
}

func hashText(s string) uint64 {
	var h uint64
	for _, c := range s {
		h = h*31 + uint64(c)
	}
	return h
}
