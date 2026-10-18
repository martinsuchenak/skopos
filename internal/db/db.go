package db

import (
	"database/sql"
	"embed"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/dbtime"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fortix/go-libs/netx/dns"
	"github.com/paularlott/logger"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// DBAddr is a resolved host:port pair, produced by ResolveHost.
type DBAddr struct {
	Host string
	Port int
}

// ResolveHost resolves a host string to a network address.
// If the host starts with "_", it is treated as an SRV name and resolved via DNS.
// Otherwise, it is parsed as host:port (e.g., "localhost:5432").
func ResolveHost(log logger.Logger, host string) (*DBAddr, error) {
	if strings.HasPrefix(host, "_") {
		resolver := dns.NewDNSResolver(dns.ResolverConfig{Logger: log})
		addrs, err := resolver.LookupSRV(host)
		if err != nil {
			return nil, fmt.Errorf("SRV lookup failed for %s: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no addresses found for SRV %s", host)
		}
		return &DBAddr{
			Host: addrs[0].IP.String(),
			Port: addrs[0].Port,
		}, nil
	}

	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return nil, fmt.Errorf("invalid host:port format %s: %w", host, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return nil, fmt.Errorf("invalid port %s: %w", p, err)
	}
	return &DBAddr{Host: h, Port: port}, nil
}

// sqliteDSN builds a SQLite connection string with pragmas applied per-connection.
// foreign_keys must be in the DSN (not a separate PRAGMA call) so that every
// pooled connection enforces it, otherwise CASCADE deletes silently no-op.
// _txlock=immediate takes the write lock at BEGIN: DEFERRED read-then-write
// transactions (plans AddItem/AddDependency/UpdateItem) deadlock on contention
// and fail with SQLITE_BUSY immediately — busy_timeout does not apply to lock
// upgrades — silently dropping the write. Immediate transactions queue on
// busy_timeout instead.
func sqliteDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
}

// Connect opens the SQLite database at path and applies connection-pool tuning.
func Connect(log logger.Logger, path string) (*sql.DB, error) {
	sqlDB, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	// Cap the pool so SQLite write contention is bounded; WAL allows readers to
	// proceed concurrently while busy_timeout serialises writers.
	conns := runtime.NumCPU()
	if conns < 4 {
		conns = 4
	}
	sqlDB.SetMaxOpenConns(conns)
	sqlDB.SetMaxIdleConns(conns)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	log.Info("connected to database", "path", path)
	return sqlDB, nil
}

// RunMigrations executes the embedded schema. Statements use CREATE ... IF NOT
// EXISTS, so this is safe to run on every start. Additive column changes to
// existing tables cannot be expressed there, so ensureColumns applies them
// explicitly (no-op when the column already exists).
func RunMigrations(db *sql.DB) error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("reading schema: %w", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		return fmt.Errorf("executing schema: %w", err)
	}
	if err := ensureColumn(db, "workspaces", "git_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "api_keys", "approver", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "runs", "lease_token_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "runs", "claimed_key_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "runs", "resume_after", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "inbox_items", "slack_thread_ts", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Capability profiles: how each run was executed, and the skills a plan
	// selected (plan + revision snapshot — part of the approved content).
	if err := ensureColumn(db, "runs", "executor", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plans", "skills", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plan_revisions", "skills_json", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Stage groups (multi-agent stages): membership, the reviewer's focus
	// brief, what the run cost, and its structured findings (JSON). NULL =
	// not grouped / no focus / not reported. SQLite only allows adding a
	// REFERENCES column whose default is NULL — these are.
	if err := ensureColumn(db, "runs", "group_id", "TEXT REFERENCES run_groups(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	if err := ensureColumn(db, "runs", "focus", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumn(db, "runs", "cost_usd", "REAL"); err != nil {
		return err
	}
	if err := ensureColumn(db, "runs", "findings", "TEXT CHECK (findings IS NULL OR json_valid(findings))"); err != nil {
		return err
	}
	// Plan candidates (multi-agent planning): a candidate is a revision with
	// a group; its state moves open → approved | superseded. NULL for every
	// revision outside a plan group (all existing ones).
	if err := ensureColumn(db, "plan_revisions", "group_id", "TEXT REFERENCES run_groups(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plan_revisions", "label", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumn(db, "plan_revisions", "candidate_state", "TEXT CHECK (candidate_state IS NULL OR candidate_state IN ('open', 'superseded', 'approved'))"); err != nil {
		return err
	}
	// Worker registry rows belong to the API key that wrote them ('' =
	// root, NULL = written before this column; adopted by the next writer).
	if err := ensureColumn(db, "workers", "key_id", "TEXT"); err != nil {
		return err
	}
	// Worker presence: what a worker reports on every poll (its version and
	// poll interval) and its clean-shutdown goodbye.
	for _, col := range [][2]string{{"version", "TEXT"}, {"poll_ms", "INTEGER"}, {"offline_at", "TEXT"}} {
		if err := ensureColumn(db, "workers", col[0], col[1]); err != nil {
			return err
		}
	}
	// Per-item multi-agent switch: NULL = the repository default.
	if err := ensureColumn(db, "inbox_items", "agents", "TEXT CHECK (agents IS NULL OR agents IN ('solo', 'panel'))"); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_plan_revisions_one_approved ON plan_revisions(group_id) WHERE candidate_state = 'approved'`); err != nil {
		return fmt.Errorf("indexing plan candidates: %w", err)
	}
	// Timestamps: rewrite rows stored before the fixed-width format (see
	// internal/dbtime) so old and new values compare and sort together.
	if err := normalizeTimestamps(db); err != nil {
		return err
	}
	// After the column exists (schema.sql runs before ensureColumn).
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_runs_group ON runs(group_id) WHERE group_id IS NOT NULL`); err != nil {
		return fmt.Errorf("indexing runs.group_id: %w", err)
	}
	return nil
}

func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return fmt.Errorf("adding %s.%s: %w", table, column, err)
	}
	return nil
}

// timestampColumns are every text timestamp column (run_groups stores Unix
// milliseconds and is not listed).
var timestampColumns = []struct{ table, column string }{
	{"sessions", "started_at"}, {"sessions", "updated_at"},
	{"agents", "first_seen_at"}, {"agents", "last_seen_at"},
	{"agent_states", "updated_at"}, {"agent_states", "stuck_at"},
	{"events", "created_at"},
	{"blackboard_entries", "created_at"}, {"blackboard_entries", "updated_at"},
	{"plans", "created_at"}, {"plans", "updated_at"},
	{"plan_items", "created_at"}, {"plan_items", "updated_at"},
	{"plan_revisions", "created_at"}, {"plan_revisions", "locked_at"},
	{"approvals", "created_at"},
	{"workspaces", "created_at"},
	{"inbox_items", "created_at"}, {"inbox_items", "updated_at"},
	{"api_keys", "created_at"}, {"api_keys", "last_used_at"}, {"api_keys", "revoked_at"},
	{"workspace_groups", "created_at"},
	{"audit_log", "created_at"},
	{"runs", "lease_expires_at"}, {"runs", "resume_after"}, {"runs", "created_at"}, {"runs", "updated_at"},
	{"workers", "last_seen"}, {"workers", "updated_at"}, {"workers", "offline_at"},
}

// normalizeTimestamps rewrites every stored timestamp that is not yet in
// the fixed-width format, one transaction per column. Idempotent: only
// non-empty values of the wrong width are touched, so after the first run
// each startup is a cheap scan. Values that do not parse are left alone.
func normalizeTimestamps(db *sql.DB) error {
	for _, tc := range timestampColumns {
		if err := normalizeColumn(db, tc.table, tc.column); err != nil {
			return fmt.Errorf("normalizing %s.%s: %w", tc.table, tc.column, err)
		}
	}
	return nil
}

func normalizeColumn(db *sql.DB, table, column string) error {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT rowid, %[1]s FROM %[2]s WHERE %[1]s IS NOT NULL AND %[1]s != '' AND length(%[1]s) != ?`, column, table), dbtime.Width)
	if err != nil {
		return err
	}
	type fix struct {
		rowid int64
		value string
	}
	var fixes []fix
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		if t, perr := dbtime.Parse(raw); perr == nil {
			fixes = append(fixes, fix{id, dbtime.Format(t)})
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(fixes) == 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt := fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, column)
	for _, f := range fixes {
		if _, err := tx.Exec(stmt, f.value, f.rowid); err != nil {
			return err
		}
	}
	return tx.Commit()
}
