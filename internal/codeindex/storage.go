// Package codeindex stores per-workspace symbol indexes in SQLite and serves
// structural queries (search, outline, call graph) over them. Each workspace
// gets its own database file under the index directory, so an index can be
// dropped or rebuilt without touching coordination data.
package codeindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS blobs (
  hash    TEXT PRIMARY KEY,
  payload TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS symbols (
  id         INTEGER PRIMARY KEY,
  hash       TEXT NOT NULL,
  name       TEXT NOT NULL,
  qual_name  TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL,
  line       INTEGER NOT NULL,
  start_byte INTEGER NOT NULL,
  end_byte   INTEGER NOT NULL,
  signature  TEXT NOT NULL DEFAULT '',
  lang       TEXT NOT NULL DEFAULT '',
  name_parts TEXT NOT NULL DEFAULT '',
  doc        TEXT NOT NULL DEFAULT '',
  modifiers  TEXT NOT NULL DEFAULT '',
  attrs      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_symbols_dedup ON symbols(hash, name, kind, line, start_byte);
CREATE TABLE IF NOT EXISTS edges (
  id     INTEGER PRIMARY KEY,
  hash   TEXT NOT NULL,
  caller TEXT NOT NULL DEFAULT '',
  callee TEXT NOT NULL,
  kind   TEXT NOT NULL,
  line   INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_edges_dedup ON edges(hash, caller, callee, line);
CREATE TABLE IF NOT EXISTS branch_files (
  branch TEXT NOT NULL,
  path   TEXT NOT NULL,
  hash   TEXT NOT NULL,
  PRIMARY KEY (branch, path)
);
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS state (
  branch       TEXT PRIMARY KEY,
  head_sha     TEXT NOT NULL DEFAULT '',
  built_at     TEXT NOT NULL,
  source       TEXT NOT NULL DEFAULT '',
  file_count   INTEGER NOT NULL DEFAULT 0,
  symbol_count INTEGER NOT NULL DEFAULT 0
);
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
  name, name_parts, signature, kind, doc, modifiers, attrs,
  content='symbols', content_rowid='id', tokenize='porter unicode61'
);
CREATE TABLE IF NOT EXISTS file_cache (
  path      TEXT PRIMARY KEY,
  mtime     INTEGER NOT NULL,
  size      INTEGER NOT NULL,
  hash      TEXT NOT NULL,
  extractor TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS embeddings (
  symbol_id INTEGER PRIMARY KEY,
  vec       BLOB NOT NULL
);
CREATE TRIGGER IF NOT EXISTS symbols_ai AFTER INSERT ON symbols BEGIN
  INSERT INTO symbols_fts(rowid, name, name_parts, signature, kind, doc, modifiers, attrs)
  VALUES (new.id, new.name, new.name_parts, new.signature, new.kind, new.doc, new.modifiers, new.attrs);
END;
`

// Store manages one SQLite database per workspace under dir.
const maxOpenIndexDBs = 64

type Store struct {
	dir string

	mu             sync.Mutex
	dbs            map[string]*sql.DB
	lru            []string // most recently used last; bounds open handles
	cacheWorkspace string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating index dir: %w", err)
	}
	return &Store{dir: dir, dbs: map[string]*sql.DB{}}, nil
}

// AsBuildCache turns the store into a parse cache rooted at one workspace's
// DB (typically the one being built into; the cache table is per-DB).
func (st *Store) AsBuildCache(workspace string) *Store {
	st.cacheWorkspace = workspace
	return st
}

// parseCacheVersion keys cache entries to the extractor: a bump invalidates
// every cached parse without a migration.
func parseCacheVersion() string {
	return "v" + parse.ExtractorVersion
}

// slugOf derives a stable, filesystem-safe slug from a workspace id (or any
// identifier such as a git URL). When sanitization changed the input (or
// truncated it), a short digest is appended so distinct ids cannot collide
// onto the same index file or vector collection ("foo/bar" vs "foo-bar").
func slugOf(workspace string) string {
	safe := make([]rune, 0, len(workspace))
	changed := false
	for _, r := range strings.ToLower(workspace) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			safe = append(safe, r)
		default:
			safe = append(safe, '-')
			changed = true
		}
	}
	s := strings.Trim(strings.Join(strings.Fields(string(safe)), "-"), "-")
	if len(s) > 80 {
		s = s[:80]
		changed = true
	}
	if s == "" {
		return "default"
	}
	if s == "." || s == ".." {
		// Dot-only slugs make filepath.Join traverse out of the index
		// directory; replace them with a digest-derived safe name.
		sum := sha256.Sum256([]byte(workspace))
		return "ws-" + hex.EncodeToString(sum[:4])
	}
	if changed {
		sum := sha256.Sum256([]byte(workspace))
		s += "-" + hex.EncodeToString(sum[:4])
	}
	return s
}

// dbPath returns the index database path for a workspace id.
func dbPath(dir, workspace string) string {
	return filepath.Join(dir, slugOf(workspace)+".db")
}

// ensureColumn adds a column (no-op when present) for additive schema changes
// to existing index DBs.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// ensureFTSShape upgrades FTS tables created before a column existed
// (doc, modifiers, attrs): the virtual table is recreated with the full
// column set and repopulated.
func ensureFTSShape(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(symbols_fts)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if have["doc"] && have["modifiers"] && have["attrs"] {
		return nil
	}
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS symbols_ai`,
		`DROP TABLE IF EXISTS symbols_fts`,
		`CREATE VIRTUAL TABLE symbols_fts USING fts5(
		  name, name_parts, signature, kind, doc, modifiers, attrs,
		  content='symbols', content_rowid='id', tokenize='porter unicode61')`,
		`INSERT INTO symbols_fts(rowid, name, name_parts, signature, kind, doc, modifiers, attrs)
		  SELECT id, name, name_parts, signature, kind, doc, modifiers, attrs FROM symbols`,
		`CREATE TRIGGER symbols_ai AFTER INSERT ON symbols BEGIN
		  INSERT INTO symbols_fts(rowid, name, name_parts, signature, kind, doc, modifiers, attrs)
		  VALUES (new.id, new.name, new.name_parts, new.signature, new.kind, new.doc, new.modifiers, new.attrs);
		END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("fts shape migration: %w", err)
		}
	}
	return nil
}

func (st *Store) DB(workspace string) (*sql.DB, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if db, ok := st.dbs[workspace]; ok {
		st.markUsed(workspace)
		return db, nil
	}
	// Same DSN pragmas as the coordination DB: WAL, busy timeout, and
	// per-connection foreign keys are irrelevant here but consistency is free.
	dsn := dbPath(st.dir, workspace) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening index db for %q: %w", workspace, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating index db for %q: %w", workspace, err)
	}
	if err := ensureColumn(db, "symbols", "qual_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(db, "symbols", "doc", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(db, "symbols", "modifiers", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(db, "symbols", "attrs", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureFTSShape(db); err != nil {
		db.Close()
		return nil, err
	}
	st.dbs[workspace] = db
	st.markUsed(workspace)
	st.evictDBs()
	return db, nil
}

// markUsed records a workspace as most recently used.
func (st *Store) markUsed(workspace string) {
	st.forgetDB(workspace)
	st.lru = append(st.lru, workspace)
}

// forgetDB drops a workspace from the LRU list.
func (st *Store) forgetDB(workspace string) {
	kept := st.lru[:0]
	for _, ws := range st.lru {
		if ws != workspace {
			kept = append(kept, ws)
		}
	}
	st.lru = kept
}

// evictDBs closes least-recently-used handles beyond maxOpenIndexDBs so a
// client touching arbitrarily many workspace ids cannot exhaust the process
// file-descriptor limit. The build-cache workspace is kept where possible.
func (st *Store) evictDBs() {
	for len(st.dbs) > maxOpenIndexDBs {
		victimIdx := -1
		for i := len(st.lru) - 1; i >= 0; i-- {
			if st.lru[i] != st.cacheWorkspace {
				victimIdx = i
				break
			}
		}
		if victimIdx < 0 {
			return
		}
		victim := st.lru[victimIdx]
		st.lru = append(st.lru[:victimIdx], st.lru[victimIdx+1:]...)
		if db, ok := st.dbs[victim]; ok {
			db.Close()
			delete(st.dbs, victim)
		}
	}
}

// DeleteWorkspace removes a workspace's entire index database. The caller
// is responsible for dropping external vector stores first (Service).
func (st *Store) DeleteWorkspace(workspace string) error {
	path := dbPath(st.dir, workspace)
	st.mu.Lock()
	if db, ok := st.dbs[workspace]; ok {
		db.Close()
		delete(st.dbs, workspace)
		st.forgetDB(workspace)
	}
	st.mu.Unlock()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (st *Store) Close() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, db := range st.dbs {
		db.Close()
	}
	st.dbs = map[string]*sql.DB{}
	st.lru = nil
}

// FileEntry is one file of a branch's index state.
type FileEntry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// HasBlobs returns which of the given hashes are missing from the blob store
// (manifest negotiation: the client uploads only these).
func (st *Store) HasBlobs(workspace string, hashes []string) (missing []string, err error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	for _, h := range hashes {
		var one int
		if err := db.QueryRow(`SELECT 1 FROM blobs WHERE hash = ?`, h).Scan(&one); err == sql.ErrNoRows {
			missing = append(missing, h)
		} else if err != nil {
			return nil, fmt.Errorf("checking blob %s: %w", h, err)
		}
	}
	return missing, nil
}

// AddBlob stores a parsed file payload (the FileResult JSON) and materializes
// its symbols/edges. Idempotent.
func (st *Store) AddBlob(workspace string, res *parse.FileResult) error {
	db, err := st.DB(workspace)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var one int
	if err := tx.QueryRow(`SELECT 1 FROM blobs WHERE hash = ?`, res.Hash).Scan(&one); err == nil {
		return nil // already stored
	} else if err != sql.ErrNoRows {
		return err
	}
	payload := marshalJSON(res)
	if _, err := tx.Exec(`INSERT INTO blobs (hash, payload) VALUES (?, ?)`, res.Hash, payload); err != nil {
		return fmt.Errorf("storing blob: %w", err)
	}
	for _, sym := range res.Symbols {
		if sym.Name == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO symbols (hash, name, qual_name, kind, line, start_byte, end_byte, signature, lang, name_parts, doc, modifiers, attrs)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			res.Hash, sym.Name, sym.Qual, sym.Kind, sym.Line, sym.StartByte, sym.EndByte, sym.Signature, sym.Lang,
			parse.SplitIdentifier(namePartsInput(sym)), sym.Doc, marshalJSON(sym.Modifiers), marshalJSON(sym.Attrs)); err != nil {
			return fmt.Errorf("inserting symbol %q: %w", sym.Name, err)
		}
	}
	for _, e := range res.Edges {
		if e.Callee == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO edges (hash, caller, callee, kind, line)
			VALUES (?, ?, ?, ?, ?)`, res.Hash, e.Caller, e.Callee, e.Kind, e.Line); err != nil {
			return fmt.Errorf("inserting edge %q->%q: %w", e.Caller, e.Callee, err)
		}
	}
	return tx.Commit()
}

// Commit atomically points a branch at a new file set.
func (st *Store) Commit(workspace, branch, headSHA, source string, files []FileEntry) error {
	db, err := st.DB(workspace)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM branch_files WHERE branch = ?`, branch); err != nil {
		return err
	}
	var symbolCount int
	for _, f := range files {
		if _, err := tx.Exec(`INSERT INTO branch_files (branch, path, hash) VALUES (?, ?, ?)`, branch, f.Path, f.Hash); err != nil {
			return fmt.Errorf("committing %s: %w", f.Path, err)
		}
	}
	// Symbol count for the branch's current file set (for status display).
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM symbols s
		JOIN branch_files bf ON bf.hash = s.hash
		WHERE bf.branch = ?`, branch).Scan(&symbolCount); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO state (branch, head_sha, built_at, source, file_count, symbol_count)
		VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'), ?, ?, ?)
		ON CONFLICT(branch) DO UPDATE SET
		  head_sha=excluded.head_sha, built_at=excluded.built_at,
		  source=excluded.source, file_count=excluded.file_count,
		  symbol_count=excluded.symbol_count`,
		branch, headSHA, source, len(files), symbolCount); err != nil {
		return err
	}
	return tx.Commit()
}

// DropBranch removes a branch's index state (blobs/symbols are shared and kept).
func (st *Store) DropBranch(workspace, branch string) error {
	db, err := st.DB(workspace)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM branch_files WHERE branch = ?`, branch); err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM state WHERE branch = ?`, branch)
	return err
}

// SymbolHit is a query result row.
type SymbolHit struct {
	Name      string `json:"name"`
	Qualified string `json:"qualified,omitempty"` // Class::method when nested in a type
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Signature string   `json:"signature,omitempty"`
	Doc       string   `json:"doc,omitempty"`                // doc comment (summary + non-signature tags)
	Modifiers []string `json:"modifiers,omitempty"`           // visibility/static/… from the declaration
	Attrs     []string `json:"attrs,omitempty"`               // attributes, annotations, decorators
	MatchedBy string   `json:"matched_by,omitempty"`          // semantic fusion: fts, vector, or both
	Lang      string   `json:"lang,omitempty"`

	rank int // vector rank when fused (internal)
}

// Search runs a full-text query over a branch's symbols. The query is
// prefix-expanded when it has no FTS operators.
func (st *Store) Search(ctx context.Context, workspace, branch, query, pathPrefix string, limit int) ([]SymbolHit, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// FQN queries ("Class::method") go through an exact/prefix qualified
	// lookup: the FTS phrase for such a query tokenizes as
	// [class, method] which matches neither the one-token short name nor
	// the fully-split name_parts column.
	if strings.Contains(query, "::") {
		filterCond, filterArgs := pathFilter(pathPrefix)
		args := append([]any{branch}, filterArgs...)
		rows, err := db.QueryContext(ctx, `
			SELECT s.name, s.qual_name, s.kind, bf.path, s.line, s.signature, s.lang, s.doc, s.modifiers, s.attrs
			FROM symbols s
			JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?`+filterCond+`
			WHERE s.qual_name = ? COLLATE NOCASE OR s.qual_name LIKE ? COLLATE NOCASE ESCAPE '\'
			ORDER BY (s.qual_name = ? COLLATE NOCASE) DESC, bf.path, s.line
			LIMIT ?`, append(args, query, likeEscape(query)+"%", query, limit)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanHits(rows)
	}
	q := query
	if !strings.ContainsAny(q, `:*"^()`) {
		q = q + "*"
	}
	filterCond, filterArgs := pathFilter(pathPrefix)
	args := append([]any{branch}, filterArgs...)
	rows, err := db.QueryContext(ctx, `
		SELECT s.name, s.qual_name, s.kind, bf.path, s.line, s.signature, s.lang, s.doc, s.modifiers, s.attrs
		FROM symbols_fts f
		JOIN symbols s ON s.id = f.rowid
		JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?`+filterCond+`
		WHERE symbols_fts MATCH ?
		-- Column weights: exact-name matches must outrank doc mentions
		-- (name, name_parts, signature, kind, doc, modifiers, attrs).
		ORDER BY bm25(symbols_fts, 10.0, 10.0, 5.0, 2.0, 4.0, 1.0, 4.0)
		LIMIT ?`, append(args, q, limit)...)
	if err != nil {
		// Bad FTS syntax: retry as a plain quoted prefix query.
		rows, err = db.QueryContext(ctx, `
			SELECT s.name, s.qual_name, s.kind, bf.path, s.line, s.signature, s.lang, s.doc, s.modifiers, s.attrs
			FROM symbols_fts f
			JOIN symbols s ON s.id = f.rowid
			JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?`+filterCond+`
			WHERE symbols_fts MATCH ?
			ORDER BY bm25(symbols_fts, 10.0, 10.0, 5.0, 2.0, 4.0, 1.0, 4.0)
			LIMIT ?`, append(args, `"`+strings.ReplaceAll(query, `"`, "")+`"*`, limit)...)
		if err != nil {
			return nil, fmt.Errorf("search %q: %w", query, err)
		}
	}
	defer rows.Close()
	return scanHits(rows)
}

// pathFilter is the SQL fragment + args for an optional path-prefix scope.
// Empty prefix means no filtering.
func pathFilter(prefix string) (string, []any) {
	if prefix == "" {
		return "", nil
	}
	return " AND (bf.path = ? OR bf.path LIKE ? ESCAPE '\\')", []any{prefix, likeEscape(prefix) + "%"}
}

// Symbol returns exact-name definitions on a branch.
func (st *Store) Symbol(workspace, branch, name string, limit int) ([]SymbolHit, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit, 50, 500)
	rows, err := db.Query(`
		SELECT s.name, s.qual_name, s.kind, bf.path, s.line, s.signature, s.lang, s.doc, s.modifiers, s.attrs
		FROM symbols s
		JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?
		WHERE s.name = ? COLLATE NOCASE OR s.qual_name = ? COLLATE NOCASE
		ORDER BY bf.path, s.line
		LIMIT ?`, branch, name, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHits(rows)
}

// Outline lists a file's symbols in source order.
func (st *Store) Outline(workspace, branch, path string) ([]SymbolHit, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT s.name, s.qual_name, s.kind, bf.path, s.line, s.signature, s.lang, s.doc, s.modifiers, s.attrs
		FROM symbols s
		JOIN branch_files bf ON bf.hash = s.hash AND bf.branch = ?
		WHERE bf.path = ?
		ORDER BY s.line`, branch, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHits(rows)
}

// EdgeHit is a graph edge with the file it occurs in.
type EdgeHit struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Kind   string `json:"kind,omitempty"` // call (default), new, extends, implements, uses, embeds
}

// Callers returns edges calling the given name on a branch.
func (st *Store) Callers(ctx context.Context, workspace, branch, name, pathPrefix string, limit int) ([]EdgeHit, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit, 100, 500)
	filterCond, filterArgs := pathFilter(pathPrefix)
	args := append([]any{branch}, filterArgs...)
	rows, err := db.Query(`
		SELECT e.caller, e.callee, bf.path, e.line, e.kind
		FROM edges e
		JOIN branch_files bf ON bf.hash = e.hash AND bf.branch = ?`+filterCond+`
		WHERE (e.callee = ? COLLATE NOCASE OR e.callee LIKE '%::' || ? ESCAPE '\')
		ORDER BY bf.path, e.line
		LIMIT ?`, append(args, name, likeEscape(name), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEdges(rows)
}

// Callees returns edges called from the given symbol on a branch.
func (st *Store) Callees(ctx context.Context, workspace, branch, name, pathPrefix string, limit int) ([]EdgeHit, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit, 100, 500)
	filterCond, filterArgs := pathFilter(pathPrefix)
	args := append([]any{branch}, filterArgs...)
	rows, err := db.Query(`
		SELECT e.caller, e.callee, bf.path, e.line, e.kind
		FROM edges e
		JOIN branch_files bf ON bf.hash = e.hash AND bf.branch = ?`+filterCond+`
		WHERE (e.caller = ? COLLATE NOCASE OR e.caller LIKE '%::' || ? ESCAPE '\')
		ORDER BY bf.path, e.line
		LIMIT ?`, append(args, name, likeEscape(name), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEdges(rows)
}

// BranchStatus describes one indexed branch. Embedding fields are
// workspace-wide (vectors are not per-branch) and repeated on each row.
type BranchStatus struct {
	Branch      string `json:"branch"`
	HeadSHA     string `json:"head_sha,omitempty"`
	BuiltAt     string `json:"built_at"`
	Source      string `json:"source,omitempty"`
	FileCount   int    `json:"file_count"`
	SymbolCount int    `json:"symbol_count"`

	Embeddable     int    `json:"embeddable,omitempty"`
	Embedded       int    `json:"embedded,omitempty"`
	EmbeddingError string `json:"embedding_error,omitempty"`
}

// GC removes index content no branch references: blobs, symbols, and edges
// whose content hash is absent from every branch_files row (left behind by
// extractor-version bumps and dropped branches). Returns reclaimed symbol
// ids so callers can drop their vectors too. Irreversible.
func (st *Store) GC(ctx context.Context, workspace string) (reclaimedIDs []int64, err error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id FROM symbols s
		WHERE NOT EXISTS (SELECT 1 FROM branch_files bf WHERE bf.hash = s.hash)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		reclaimedIDs = append(reclaimedIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, stmt := range []string{
		`DELETE FROM edges WHERE hash NOT IN (SELECT hash FROM branch_files)`,
		`DELETE FROM symbols WHERE hash NOT IN (SELECT hash FROM branch_files)`,
		`DELETE FROM blobs WHERE hash NOT IN (SELECT hash FROM branch_files)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reclaimedIDs, nil
}

// CachePrune removes cached blob payloads whose hash no file_cache row
// references — content versions from before edits, safe to reclaim.
func (st *Store) CachePrune(ctx context.Context) (int64, error) {
	db, err := st.DB(st.cacheWorkspace)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, `DELETE FROM blobs WHERE hash NOT IN (SELECT hash FROM file_cache)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CacheWipe clears every file_cache row and blob payload.
func (st *Store) CacheWipe(ctx context.Context) error {
	db, err := st.DB(st.cacheWorkspace)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM file_cache; DELETE FROM blobs;`); err != nil {
		return err
	}
	return nil
}

// EmbeddableCount counts symbols eligible for embedding on any branch.
func (st *Store) EmbeddableCount(ctx context.Context, workspace string) int {
	db, err := st.DB(workspace)
	if err != nil {
		return 0
	}
	placeholders := make([]string, 0, len(embeddableKinds))
	args := make([]any, 0, len(embeddableKinds))
	for k := range embeddableKinds {
		placeholders = append(placeholders, "?")
		args = append(args, k)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM symbols WHERE kind IN (`+strings.Join(placeholders, ",")+`)`, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// Status lists indexed branches for a workspace.
func (st *Store) Status(workspace string) ([]BranchStatus, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT branch, head_sha, built_at, source, file_count, symbol_count FROM state ORDER BY built_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BranchStatus
	for rows.Next() {
		var s BranchStatus
		if err := rows.Scan(&s.Branch, &s.HeadSHA, &s.BuiltAt, &s.Source, &s.FileCount, &s.SymbolCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BranchIndexed reports whether a branch has index state.
func (st *Store) BranchIndexed(workspace, branch string) (bool, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return false, err
	}
	var one int
	err = db.QueryRow(`SELECT 1 FROM state WHERE branch = ?`, branch).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// DefaultBranch returns the diff base: the recorded git default (meta hint
// captured at build time — handles trunks like PROD/develop), else main,
// else master, else the OLDEST built branch (the trunk is built before any
// feature branch; the newest build is by construction the feature branch).
func (st *Store) DefaultBranch(workspace string) string {
	db, err := st.DB(workspace)
	if err != nil {
		return "main"
	}
	if v, ok := st.GetMeta(workspace, "default_branch"); ok && v != "" {
		var one int
		if db.QueryRow(`SELECT 1 FROM state WHERE branch = ?`, v).Scan(&one) == nil {
			return v
		}
	}
	for _, cand := range []string{"main", "master"} {
		var one int
		if db.QueryRow(`SELECT 1 FROM state WHERE branch = ?`, cand).Scan(&one) == nil {
			return cand
		}
	}
	var branch string
	if db.QueryRow(`SELECT branch FROM state ORDER BY built_at ASC LIMIT 1`).Scan(&branch) == nil {
		return branch
	}
	return "main"
}

// GetMeta reads a per-workspace metadata value.
func (st *Store) GetMeta(workspace, key string) (string, bool) {
	db, err := st.DB(workspace)
	if err != nil {
		return "", false
	}
	var v string
	if db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v) == nil {
		return v, true
	}
	return "", false
}

// SetMeta writes a per-workspace metadata value.
func (st *Store) SetMeta(workspace, key, value string) error {
	db, err := st.DB(workspace)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// likeEscape escapes LIKE wildcards in user input.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// exactCallers returns edges whose callee equals name exactly (no suffix
// fallback) — used for graph traversal so nodes never merge across types.
func (st *Store) exactCallers(db *sql.DB, branch, name string, limit int) ([]EdgeHit, error) {
	rows, err := db.Query(`
		SELECT e.caller, e.callee, bf.path, e.line, e.kind
		FROM edges e
		JOIN branch_files bf ON bf.hash = e.hash AND bf.branch = ?
		WHERE e.callee = ? COLLATE NOCASE
		ORDER BY bf.path, e.line
		LIMIT ?`, branch, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEdges(rows)
}

// FileDeps lists one file's imports (module dependencies).
type FileDeps struct {
	Path    string   `json:"path"`
	Imports []string `json:"imports"`
}

// Dependencies lists per-file import edges on a branch.
func (st *Store) Dependencies(ctx context.Context, workspace, branch, pathPrefix string) ([]FileDeps, error) {
	db, err := st.DB(workspace)
	if err != nil {
		return nil, err
	}
	filterCond, filterArgs := pathFilter(pathPrefix)
	args := append([]any{branch}, filterArgs...)
	rows, err := db.QueryContext(ctx, `
		SELECT bf.path, e.callee
		FROM edges e
		JOIN branch_files bf ON bf.hash = e.hash AND bf.branch = ?`+filterCond+`
		WHERE e.kind = 'import' AND (e.caller = '' OR e.caller IS NULL)
		ORDER BY bf.path, e.callee`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileDeps
	byPath := map[string]int{}
	for rows.Next() {
		var path, imp string
		if err := rows.Scan(&path, &imp); err != nil {
			return nil, err
		}
		i, ok := byPath[path]
		if !ok {
			out = append(out, FileDeps{Path: path})
			i = len(out) - 1
			byPath[path] = i
		}
		out[i].Imports = append(out[i].Imports, imp)
	}
	return out, rows.Err()
}

// clampLimit bounds query limits to a sane default and ceiling.
func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	return limit
}

// BuildCacher short-circuits re-parsing files whose (mtime, size, extractor)
// triple is unchanged — the parsed result is already in the blob store,
// keyed by content hash. Implemented by Store; any DB with the schema works.
type BuildCacher interface {
	CacheLookup(ctx context.Context, path string, mtime, size int64) (hash string, ok bool)
	CachePut(ctx context.Context, path string, mtime, size int64, hash string) error
	// CacheBlob reconstructs the full parsed result stored for a content
	// hash. A lookup hit without a materializable payload (blob pruned, or
	// a cache written before payloads were stored) must fall back to a
	// fresh parse — a stub would upload/commit an empty file index.
	CacheBlob(hash string) (*parse.FileResult, bool)
}

// CacheBlob loads a stored blob payload back into a FileResult.
func (st *Store) CacheBlob(hash string) (*parse.FileResult, bool) {
	db, err := st.DB(st.cacheWorkspace)
	if err != nil {
		return nil, false
	}
	var payload string
	if err := db.QueryRow(`SELECT payload FROM blobs WHERE hash = ?`, hash).Scan(&payload); err != nil {
		return nil, false
	}
	var res parse.FileResult
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		return nil, false
	}
	return &res, true
}

// CacheLookup reports a cached parse for an unchanged file.
func (st *Store) CacheLookup(ctx context.Context, path string, mtime, size int64) (string, bool) {
	db, err := st.DB(st.cacheWorkspace)
	if err != nil {
		return "", false
	}
	var hash string
	err = db.QueryRowContext(ctx,
		`SELECT hash FROM file_cache WHERE path = ? AND mtime = ? AND size = ? AND extractor = ?`,
		path, mtime, size, parseCacheVersion()).Scan(&hash)
	if err != nil {
		return "", false
	}
	return hash, true
}

// CachePut records a file's parse under its current stat + extractor version.
func (st *Store) CachePut(ctx context.Context, path string, mtime, size int64, hash string) error {
	db, err := st.DB(st.cacheWorkspace)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO file_cache (path, mtime, size, hash, extractor) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET mtime=excluded.mtime, size=excluded.size,
		  hash=excluded.hash, extractor=excluded.extractor`,
		path, mtime, size, hash, parseCacheVersion())
	return err
}

// namePartsInput picks the text to tokenize for FTS: the qualified name when
// present (its tokens include the short name's tokens) so "newuser validate"
// and bare "validate" both match.
func namePartsInput(sym parse.Symbol) string {
	if sym.Qual != "" {
		return sym.Qual
	}
	return sym.Name
}

// scanHits scans the uniform hit columns: name, qual, kind, path, line,
// signature, lang, doc, modifiers, attrs.
func scanHits(rows *sql.Rows) ([]SymbolHit, error) { return scanHitsN(rows) }

// unmarshalStrings decodes a stored JSON string array; empty/null yields nil.
func unmarshalStrings(s string) []string {
	if s == "" || s == "null" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func scanHitsN(rows *sql.Rows) ([]SymbolHit, error) {
	var out []SymbolHit
	for rows.Next() {
		var h SymbolHit
		var mods, attrs string
		if err := rows.Scan(&h.Name, &h.Qualified, &h.Kind, &h.Path, &h.Line, &h.Signature, &h.Lang, &h.Doc, &mods, &attrs); err != nil {
			return nil, err
		}
		h.Modifiers, h.Attrs = unmarshalStrings(mods), unmarshalStrings(attrs)
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanEdges(rows *sql.Rows) ([]EdgeHit, error) {
	var out []EdgeHit
	for rows.Next() {
		var e EdgeHit
		if err := rows.Scan(&e.Caller, &e.Callee, &e.Path, &e.Line, &e.Kind); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
