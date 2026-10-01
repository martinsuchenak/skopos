package apikeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// Storage persists API keys. Plaintext keys never reach this layer — only
// their SHA-256 hash (auth.HashKey) and a display prefix.
type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

// LookupKey implements auth.KeyLookup: it resolves a key hash to its scope.
// Revoked and unknown keys both return (nil, nil) — indistinguishable to the
// caller, which rejects with 401 either way.
func (s *Storage) LookupKey(ctx context.Context, keyHash string) (*auth.KeyInfo, error) {
	var (
		id, name string
		all      int
		approver int
		revoked  sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, all_workspaces, approver, revoked_at FROM api_keys WHERE key_hash = ?`, keyHash,
	).Scan(&id, &name, &all, &approver, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up api key: %w", err)
	}
	if revoked.Valid {
		return nil, nil
	}
	workspaces, err := s.listWorkspaces(ctx, id)
	if err != nil {
		return nil, err
	}
	if all == 0 {
		// Workspace groups widen the scope at lookup time: group members
		// plus registered workspaces matching group patterns. Resolution
		// lives here so Principal and every CanAccess check stay unchanged
		// (docs/design/agent-pipeline.md §3).
		groupScoped, err := s.groupWorkspacesForKey(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(groupScoped) > 0 {
			seen := make(map[string]struct{}, len(workspaces)+len(groupScoped))
			for _, w := range workspaces {
				seen[w] = struct{}{}
			}
			for _, w := range groupScoped {
				if _, ok := seen[w]; !ok {
					seen[w] = struct{}{}
					workspaces = append(workspaces, w)
				}
			}
			sort.Strings(workspaces)
		}
	}
	return &auth.KeyInfo{
		ID:            id,
		Name:          name,
		AllWorkspaces: all != 0,
		Approver:      approver != 0,
		Workspaces:    workspaces,
	}, nil
}

func (s *Storage) listWorkspaces(ctx context.Context, keyID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id FROM api_key_workspaces WHERE api_key_id = ? ORDER BY workspace_id`, keyID)
	if err != nil {
		return nil, fmt.Errorf("listing api key workspaces: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// groupWorkspacesForKey returns the union of members of the key's groups and
// registered workspaces matching its groups' patterns.
func (s *Storage) groupWorkspacesForKey(ctx context.Context, keyID string) ([]string, error) {
	out := map[string]struct{}{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.workspace_id
		FROM workspace_group_members m
		JOIN api_key_groups kg ON kg.group_id = m.group_id
		WHERE kg.api_key_id = ?`, keyID)
	if err != nil {
		return nil, fmt.Errorf("listing group members for key: %w", err)
	}
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			rows.Close()
			return nil, err
		}
		out[ws] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	patterns, err := s.keyPatterns(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if len(patterns) > 0 {
		registered, err := s.RegisteredWorkspaces(ctx)
		if err != nil {
			return nil, err
		}
		for _, pat := range patterns {
			for _, ws := range registered {
				if ok, err := path.Match(pat, ws); err == nil && ok {
					out[ws] = struct{}{}
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	list := make([]string, 0, len(out))
	for ws := range out {
		list = append(list, ws)
	}
	sort.Strings(list)
	return list, nil
}

// keyPatterns returns the distinct patterns of the key's groups.
func (s *Storage) keyPatterns(ctx context.Context, keyID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT p.pattern
		FROM workspace_group_patterns p
		JOIN api_key_groups kg ON kg.group_id = p.group_id
		WHERE kg.api_key_id = ? ORDER BY p.pattern`, keyID)
	if err != nil {
		return nil, fmt.Errorf("listing group patterns for key: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RegisteredWorkspaces lists every id in the workspace registry.
func (s *Storage) RegisteredWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing registered workspaces: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// WorkspaceExists reports whether the workspace registry contains id.
func (s *Storage) WorkspaceExists(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking workspace: %w", err)
	}
	return true, nil
}

// Write inserts a key with its hash, explicit workspace scope and groups.
func (s *Storage) Write(ctx context.Context, key Key, keyHash string, groupIDs []string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (id, name, key_hash, key_prefix, all_workspaces, approver, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.Name, keyHash, key.Prefix, boolToInt(key.AllWorkspaces), boolToInt(key.Approver), formatTime(key.CreatedAt)); err != nil {
		return fmt.Errorf("inserting api key: %w", err)
	}
	for _, ws := range key.Workspaces {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO api_key_workspaces (api_key_id, workspace_id) VALUES (?, ?)`, key.ID, ws); err != nil {
			return fmt.Errorf("inserting api key workspace: %w", err)
		}
	}
	if err := s.writeKeyGroups(ctx, key.ID, groupIDs); err != nil {
		return err
	}
	return nil
}

// List returns all keys (including revoked), newest first, without hashes.
func (s *Storage) List(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_prefix, all_workspaces, approver, created_at, last_used_at, revoked_at
		FROM api_keys ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var all, approver int
		var createdAt string
		var lastUsed, revoked sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &all, &approver, &createdAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		k.AllWorkspaces = all != 0
		k.Approver = approver != 0
		k.CreatedAt = parseTime(createdAt)
		if lastUsed.Valid {
			t := parseTime(lastUsed.String)
			k.LastUsedAt = &t
		}
		if revoked.Valid {
			t := parseTime(revoked.String)
			k.RevokedAt = &t
		}
		k.Workspaces, err = s.listWorkspaces(ctx, k.ID)
		if err != nil {
			return nil, err
		}
		// All-workspaces keys carry no explicit scope rows; emit [] rather
		// than null so every client can treat workspaces as a list.
		if k.Workspaces == nil {
			k.Workspaces = []string{}
		}
		if k.Groups, err = s.keyGroupNames(ctx, k.ID); err != nil {
			return nil, err
		}
		if k.Groups == nil {
			k.Groups = []string{}
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Revoke soft-deletes by id and reports whether this call performed the
// transition (false = already revoked; unknown ids return ErrNotFound).
func (s *Storage) Revoke(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		formatTime(timeNowUTC()), id)
	if err != nil {
		return false, fmt.Errorf("revoking api key: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrNotFound
			}
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }

func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// TouchKey records last_used_at (best-effort, throttled by the authenticator).
func (s *Storage) TouchKey(ctx context.Context, keyID string) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ? AND revoked_at IS NULL`,
		formatTime(timeNowUTC()), keyID)
}

// Get returns one key by id (no secret — the hash never leaves storage).
func (s *Storage) Get(ctx context.Context, id string) (Key, error) {
	var (
		k         Key
		all       int
		approver  int
		createdAt string
		lastUsed  sql.NullString
		revoked   sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, key_prefix, all_workspaces, approver, created_at, last_used_at, revoked_at
		FROM api_keys WHERE id = ?`, id,
	).Scan(&k.ID, &k.Name, &k.Prefix, &all, &approver, &createdAt, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	if err != nil {
		return k, fmt.Errorf("getting api key: %w", err)
	}
	k.AllWorkspaces = all != 0
	k.Approver = approver != 0
	k.CreatedAt = parseTime(createdAt)
	if lastUsed.Valid {
		t := parseTime(lastUsed.String)
		k.LastUsedAt = &t
	}
	if revoked.Valid {
		t := parseTime(revoked.String)
		k.RevokedAt = &t
	}
	if k.Workspaces, err = s.listWorkspaces(ctx, id); err != nil {
		return k, err
	}
	if k.Workspaces == nil {
		k.Workspaces = []string{}
	}
	if k.Groups, err = s.keyGroupNames(ctx, id); err != nil {
		return k, err
	}
	if k.Groups == nil {
		k.Groups = []string{}
	}
	return k, nil
}

// Update changes a key's name and scope atomically: the row is updated, the
// workspace scope replaced, and the group membership replaced (nil groupIDs
// leaves groups unchanged) in one transaction. approver is the effective
// flag; the service resolves "unchanged" before calling.
func (s *Storage) Update(ctx context.Context, id, name string, all, approver bool, workspaces []string, groupIDs *[]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE api_keys SET name = ?, all_workspaces = ?, approver = ? WHERE id = ?`,
		name, boolToInt(all), boolToInt(approver), id)
	if err != nil {
		return fmt.Errorf("updating api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_workspaces WHERE api_key_id = ?`, id); err != nil {
		return fmt.Errorf("clearing api key workspaces: %w", err)
	}
	for _, ws := range workspaces {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO api_key_workspaces (api_key_id, workspace_id) VALUES (?, ?)`, id, ws); err != nil {
			return fmt.Errorf("inserting api key workspace: %w", err)
		}
	}
	if groupIDs != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_groups WHERE api_key_id = ?`, id); err != nil {
			return fmt.Errorf("clearing key groups: %w", err)
		}
		for _, gid := range *groupIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO api_key_groups (api_key_id, group_id) VALUES (?, ?)`, id, gid); err != nil {
				return fmt.Errorf("inserting key group: %w", err)
			}
		}
	}
	return tx.Commit()
}

// Delete hard-deletes a key and (via FK cascade) its scope rows.
func (s *Storage) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- workspace groups (docs/design/agent-pipeline.md §3) ---

// ErrDuplicateName reports a UNIQUE(name) violation on workspace_groups.
var ErrDuplicateName = errors.New("a group with that name already exists")

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// WriteGroup inserts a group with its members and patterns.
func (s *Storage) WriteGroup(ctx context.Context, g Group) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_groups (id, name, description, created_at)
		VALUES (?, ?, ?, ?)`,
		g.ID, g.Name, g.Description, formatTime(g.CreatedAt)); err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateName
		}
		return fmt.Errorf("inserting workspace group: %w", err)
	}
	if err := s.writeGroupRows(ctx, g); err != nil {
		return err
	}
	return nil
}

func (s *Storage) writeGroupRows(ctx context.Context, g Group) error {
	for _, ws := range g.Members {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO workspace_group_members (group_id, workspace_id) VALUES (?, ?)`, g.ID, ws); err != nil {
			return fmt.Errorf("inserting group member: %w", err)
		}
	}
	for _, p := range g.Patterns {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO workspace_group_patterns (group_id, pattern) VALUES (?, ?)`, g.ID, p); err != nil {
			return fmt.Errorf("inserting group pattern: %w", err)
		}
	}
	return nil
}

// ListGroups returns every group with its members and patterns, by name.
func (s *Storage) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, created_at FROM workspace_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing workspace groups: %w", err)
	}
	var out []Group
	for rows.Next() {
		var g Group
		var createdAt string
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		g.CreatedAt = parseTime(createdAt)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Members, err = s.groupMembers(ctx, out[i].ID); err != nil {
			return nil, err
		}
		if out[i].Patterns, err = s.groupPatterns(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetGroup returns one group by id.
func (s *Storage) GetGroup(ctx context.Context, id string) (Group, error) {
	var g Group
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, created_at FROM workspace_groups WHERE id = ?`, id,
	).Scan(&g.ID, &g.Name, &g.Description, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, fmt.Errorf("getting workspace group: %w", err)
	}
	g.CreatedAt = parseTime(createdAt)
	if g.Members, err = s.groupMembers(ctx, id); err != nil {
		return g, err
	}
	if g.Patterns, err = s.groupPatterns(ctx, id); err != nil {
		return g, err
	}
	return g, nil
}

// ResolveGroupRef resolves a group by id first, then by unique name — the CLI
// accepts either.
func (s *Storage) ResolveGroupRef(ctx context.Context, ref string) (Group, error) {
	g, err := s.GetGroup(ctx, ref)
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return g, err
	}
	var id string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM workspace_groups WHERE name = ?`, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, fmt.Errorf("resolving group by name: %w", err)
	}
	return s.GetGroup(ctx, id)
}

func (s *Storage) groupMembers(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id FROM workspace_group_members WHERE group_id = ? ORDER BY workspace_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (s *Storage) groupPatterns(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pattern FROM workspace_group_patterns WHERE group_id = ? ORDER BY pattern`, groupID)
	if err != nil {
		return nil, fmt.Errorf("listing group patterns: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateGroup replaces a group's fields and, in one transaction, its member
// and pattern rows (nil lists leave the corresponding rows unchanged).
func (s *Storage) UpdateGroup(ctx context.Context, g Group, members, patterns *[]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE workspace_groups SET name = ?, description = ? WHERE id = ?`, g.Name, g.Description, g.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateName
		}
		return fmt.Errorf("updating workspace group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if members != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_group_members WHERE group_id = ?`, g.ID); err != nil {
			return fmt.Errorf("clearing group members: %w", err)
		}
		for _, ws := range *members {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO workspace_group_members (group_id, workspace_id) VALUES (?, ?)`, g.ID, ws); err != nil {
				return fmt.Errorf("inserting group member: %w", err)
			}
		}
	}
	if patterns != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_group_patterns WHERE group_id = ?`, g.ID); err != nil {
			return fmt.Errorf("clearing group patterns: %w", err)
		}
		for _, p := range *patterns {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO workspace_group_patterns (group_id, pattern) VALUES (?, ?)`, g.ID, p); err != nil {
				return fmt.Errorf("inserting group pattern: %w", err)
			}
		}
	}
	return tx.Commit()
}

// DeleteGroup removes a group; memberships cascade via FK.
func (s *Storage) DeleteGroup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspace_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting workspace group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// KeyGroups returns the full group rows a key holds.
func (s *Storage) KeyGroups(ctx context.Context, keyID string) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.name, g.description, g.created_at
		FROM workspace_groups g
		JOIN api_key_groups kg ON kg.group_id = g.id
		WHERE kg.api_key_id = ? ORDER BY g.name`, keyID)
	if err != nil {
		return nil, fmt.Errorf("listing groups for key: %w", err)
	}
	var out []Group
	for rows.Next() {
		var g Group
		var createdAt string
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		g.CreatedAt = parseTime(createdAt)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Members, err = s.groupMembers(ctx, out[i].ID); err != nil {
			return nil, err
		}
		if out[i].Patterns, err = s.groupPatterns(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// keyGroupNames lists the names of the groups a key holds (Key.Groups).
func (s *Storage) keyGroupNames(ctx context.Context, keyID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.name FROM workspace_groups g
		JOIN api_key_groups kg ON kg.group_id = g.id
		WHERE kg.api_key_id = ? ORDER BY g.name`, keyID)
	if err != nil {
		return nil, fmt.Errorf("listing group names for key: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// writeKeyGroups replaces a key's group membership rows.
func (s *Storage) writeKeyGroups(ctx context.Context, keyID string, groupIDs []string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_key_groups WHERE api_key_id = ?`, keyID); err != nil {
		return fmt.Errorf("clearing key groups: %w", err)
	}
	for _, gid := range groupIDs {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO api_key_groups (api_key_id, group_id) VALUES (?, ?)`, keyID, gid); err != nil {
			return fmt.Errorf("inserting key group: %w", err)
		}
	}
	return nil
}

// KeysInGroup lists the ids of active (non-revoked) keys holding a group.
func (s *Storage) KeysInGroup(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT k.id FROM api_keys k
		JOIN api_key_groups kg ON kg.api_key_id = k.id
		WHERE kg.group_id = ? AND k.revoked_at IS NULL`, groupID)
	if err != nil {
		return nil, fmt.Errorf("listing keys in group: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// KeysInGroupsMatching lists the ids of active keys holding any group whose
// patterns match wsID — the newly-registered-workspace trigger.
func (s *Storage) KeysInGroupsMatching(ctx context.Context, wsID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT group_id, pattern FROM workspace_group_patterns`)
	if err != nil {
		return nil, fmt.Errorf("listing group patterns: %w", err)
	}
	type gp struct{ id, pattern string }
	var pats []gp
	for rows.Next() {
		var p gp
		if err := rows.Scan(&p.id, &p.pattern); err != nil {
			rows.Close()
			return nil, err
		}
		pats = append(pats, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	seenGroups := map[string]struct{}{}
	for _, p := range pats {
		if ok, err := path.Match(p.pattern, wsID); err == nil && ok {
			seenGroups[p.id] = struct{}{}
		}
	}
	if len(seenGroups) == 0 {
		return nil, nil
	}
	out := map[string]struct{}{}
	for gid := range seenGroups {
		keyIDs, err := s.KeysInGroup(ctx, gid)
		if err != nil {
			return nil, err
		}
		for _, id := range keyIDs {
			out[id] = struct{}{}
		}
	}
	list := make([]string, 0, len(out))
	for id := range out {
		list = append(list, id)
	}
	sort.Strings(list)
	return list, nil
}

// KeysReachingWorkspace is the reverse query: every active key that can
// access wsID, with how (explicit row, group member, or group pattern).
func (s *Storage) KeysReachingWorkspace(ctx context.Context, wsID string) ([]KeyReach, error) {
	reach := map[string]*KeyReach{}
	add := func(k Key, via string) {
		if _, ok := reach[k.ID]; !ok {
			k.Workspaces = nil // per-reach view: the via field carries the scope story
			k.Groups = nil
			reach[k.ID] = &KeyReach{Key: k, Via: via}
		}
	}

	// Explicit scope.
	rows, err := s.db.QueryContext(ctx, `
		SELECT k.id, k.name, k.key_prefix, k.all_workspaces, k.created_at
		FROM api_keys k
		JOIN api_key_workspaces w ON w.api_key_id = k.id
		WHERE w.workspace_id = ? AND k.revoked_at IS NULL`, wsID)
	if err != nil {
		return nil, fmt.Errorf("querying explicit reach: %w", err)
	}
	if err := scanKeyRows(rows, func(k Key) { add(k, "explicit") }); err != nil {
		return nil, err
	}

	// Group members.
	rows, err = s.db.QueryContext(ctx, `
		SELECT k.id, k.name, k.key_prefix, k.all_workspaces, k.created_at, g.name
		FROM api_keys k
		JOIN api_key_groups kg ON kg.api_key_id = k.id
		JOIN workspace_group_members m ON m.group_id = kg.group_id
		JOIN workspace_groups g ON g.id = kg.group_id
		WHERE m.workspace_id = ? AND k.revoked_at IS NULL`, wsID)
	if err != nil {
		return nil, fmt.Errorf("querying member reach: %w", err)
	}
	for rows.Next() {
		var k Key
		var all int
		var createdAt, groupName string
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &all, &createdAt, &groupName); err != nil {
			rows.Close()
			return nil, err
		}
		k.AllWorkspaces = all != 0
		k.CreatedAt = parseTime(createdAt)
		add(k, "group:"+groupName)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Group patterns.
	keyIDs, err := s.KeysInGroupsMatching(ctx, wsID)
	if err != nil {
		return nil, err
	}
	for _, id := range keyIDs {
		if _, seen := reach[id]; seen {
			continue
		}
		k, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		via := "pattern:?"
		groups, err := s.KeyGroups(ctx, id)
		if err == nil {
			for _, g := range groups {
				for _, p := range g.Patterns {
					if ok, err := path.Match(p, wsID); err == nil && ok {
						via = "pattern:" + g.Name + ":" + p
						break
					}
				}
				if via != "pattern:?" {
					break
				}
			}
		}
		add(k, via)
	}

	out := make([]KeyReach, 0, len(reach))
	for _, r := range reach {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.Name < out[j].Key.Name })
	return out, nil
}

// scanKeyRows drains a 5-column key rowset (id, name, prefix, all, created).
func scanKeyRows(rows *sql.Rows, fn func(Key)) error {
	for rows.Next() {
		var k Key
		var all int
		var createdAt string
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &all, &createdAt); err != nil {
			rows.Close()
			return err
		}
		k.AllWorkspaces = all != 0
		k.CreatedAt = parseTime(createdAt)
		fn(k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return nil
}
