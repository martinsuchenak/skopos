package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"time"
)

// Storage owns pipeline_stages and pipeline_defaults. It also reads the
// workspace registry and group tables to resolve which groups hold a
// workspace (explicit members and path.Match patterns, as key scope does).
type Storage struct{ db *sql.DB }

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

// Get returns an owner's own settings (empty when nothing is stored).
func (s *Storage) Get(ctx context.Context, kind, id string) (Settings, error) {
	out := Settings{Stages: map[string]StageSetting{}}
	rows, err := s.db.QueryContext(ctx, `
		SELECT stage, members, COALESCE(policy, ''), early_exit, COALESCE(timeout_ms, 0)
		FROM pipeline_stages WHERE owner_kind = ? AND owner_id = ?`, kind, id)
	if err != nil {
		return out, fmt.Errorf("reading pipeline stages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var stage, members string
		var st StageSetting
		var early int64
		if err := rows.Scan(&stage, &members, &st.Policy, &early, &st.TimeoutMS); err != nil {
			return out, err
		}
		if err := json.Unmarshal([]byte(members), &st.Members); err != nil {
			return out, fmt.Errorf("pipeline %s %s %s: members: %w", kind, id, stage, err)
		}
		st.EarlyExit = early == 1
		out.Stages[stage] = st
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	var agents sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT agents_default FROM pipeline_defaults WHERE owner_kind = ? AND owner_id = ?`, kind, id).Scan(&agents)
	if err != nil && err != sql.ErrNoRows {
		return out, fmt.Errorf("reading pipeline defaults: %w", err)
	}
	out.AgentsDefault = agents.String
	return out, nil
}

// Replace stores an owner's whole settings document in one transaction.
func (s *Storage) Replace(ctx context.Context, kind, id string, st Settings, by string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_stages WHERE owner_kind = ? AND owner_id = ?`, kind, id); err != nil {
		return fmt.Errorf("clearing pipeline stages: %w", err)
	}
	ms := now.UnixMilli()
	for stage, set := range st.Stages {
		if err := insertStage(ctx, tx, kind, id, stage, set, by, ms); err != nil {
			return err
		}
	}
	if err := writeDefaults(ctx, tx, kind, id, st.AgentsDefault, by, ms); err != nil {
		return err
	}
	return tx.Commit()
}

// AddMissing stores only the stages (and agents default) the owner does not
// have yet — the one-time flag import. Returns what it added.
func (s *Storage) AddMissing(ctx context.Context, kind, id string, st Settings, by string, now time.Time) (Settings, error) {
	added := Settings{Stages: map[string]StageSetting{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return added, err
	}
	defer func() { _ = tx.Rollback() }()
	ms := now.UnixMilli()
	for stage, set := range st.Stages {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO pipeline_stages (owner_kind, owner_id, stage, members, policy, early_exit, timeout_ms, updated_ms, updated_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, stageArgs(kind, id, stage, set, by, ms)...)
		if err != nil {
			return added, fmt.Errorf("importing pipeline stage %s: %w", stage, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added.Stages[stage] = set
		}
	}
	if st.AgentsDefault != "" {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO pipeline_defaults (owner_kind, owner_id, agents_default, updated_ms, updated_by)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, kind, id, st.AgentsDefault, ms, by)
		if err != nil {
			return added, fmt.Errorf("importing agents default: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added.AgentsDefault = st.AgentsDefault
		}
	}
	return added, tx.Commit()
}

func stageArgs(kind, id, stage string, set StageSetting, by string, ms int64) []any {
	members, _ := json.Marshal(set.Members)
	var policy, timeout any
	if set.Policy != "" {
		policy = set.Policy
	}
	if set.TimeoutMS > 0 {
		timeout = set.TimeoutMS
	}
	early := 0
	if set.EarlyExit {
		early = 1
	}
	return []any{kind, id, stage, string(members), policy, early, timeout, ms, by}
}

func insertStage(ctx context.Context, tx *sql.Tx, kind, id, stage string, set StageSetting, by string, ms int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_stages (owner_kind, owner_id, stage, members, policy, early_exit, timeout_ms, updated_ms, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, stageArgs(kind, id, stage, set, by, ms)...); err != nil {
		return fmt.Errorf("writing pipeline stage %s: %w", stage, err)
	}
	return nil
}

func writeDefaults(ctx context.Context, tx *sql.Tx, kind, id, agents, by string, ms int64) error {
	if agents == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM pipeline_defaults WHERE owner_kind = ? AND owner_id = ?`, kind, id)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO pipeline_defaults (owner_kind, owner_id, agents_default, updated_ms, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (owner_kind, owner_id) DO UPDATE SET agents_default = excluded.agents_default,
			updated_ms = excluded.updated_ms, updated_by = excluded.updated_by`, kind, id, agents, ms, by)
	return err
}

// GroupsSettingStage lists the groups that set a stage (conflict check).
func (s *Storage) GroupsSettingStage(ctx context.Context, stage string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id FROM pipeline_stages WHERE owner_kind = 'group' AND stage = ?`, stage)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// WorkspaceExists: the workspace is registered.
func (s *Storage) WorkspaceExists(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// Group resolves a group by id or name.
func (s *Storage) Group(ctx context.Context, ref string) (GroupRef, error) {
	var g GroupRef
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM workspace_groups WHERE id = ? OR name = ?`, ref, ref).Scan(&g.ID, &g.Name)
	if err == sql.ErrNoRows {
		return g, fmt.Errorf("%w: group %s", ErrNotFound, ref)
	}
	return g, err
}

// GroupsContaining lists the groups that hold a workspace, as an explicit
// member or through a pattern, ordered by name.
func (s *Storage) GroupsContaining(ctx context.Context, ws string) ([]GroupRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.name, 'm', '' FROM workspace_groups g JOIN workspace_group_members m ON m.group_id = g.id WHERE m.workspace_id = ?
		UNION ALL
		SELECT g.id, g.name, 'p', p.pattern FROM workspace_groups g JOIN workspace_group_patterns p ON p.group_id = g.id
		ORDER BY 2`, ws)
	if err != nil {
		return nil, fmt.Errorf("resolving groups of %s: %w", ws, err)
	}
	defer func() { _ = rows.Close() }()
	var out []GroupRef
	seen := map[string]bool{}
	for rows.Next() {
		var g GroupRef
		var via, pattern string
		if err := rows.Scan(&g.ID, &g.Name, &via, &pattern); err != nil {
			return nil, err
		}
		if seen[g.ID] {
			continue
		}
		if via == "p" {
			if ok, err := path.Match(pattern, ws); err != nil || !ok {
				continue
			}
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupWorkspaces lists the registered workspaces a group holds (members
// plus pattern matches against the registry).
func (s *Storage) GroupWorkspaces(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var all []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, id)
	}
	_ = rows.Close()
	var out []string
	for _, ws := range all {
		groups, err := s.GroupsContaining(ctx, ws)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if g.ID == groupID {
				out = append(out, ws)
				break
			}
		}
	}
	return out, nil
}

// DropOrphans removes settings of deleted workspaces and groups.
func (s *Storage) DropOrphans(ctx context.Context) error {
	for _, table := range []string{"pipeline_stages", "pipeline_defaults"} {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
			DELETE FROM %s WHERE (owner_kind = 'workspace' AND owner_id NOT IN (SELECT id FROM workspaces))
				OR (owner_kind = 'group' AND owner_id NOT IN (SELECT id FROM workspace_groups))`, table)); err != nil {
			return fmt.Errorf("dropping orphaned pipeline settings: %w", err)
		}
	}
	return nil
}

// RunnersServing lists the runner names offered by workers that serve a
// workspace (the "no worker offers this runner" check stays in scope).
func (s *Storage) RunnersServing(ctx context.Context, ws string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT runners, workspaces FROM workers`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var rawRunners, rawWorkspaces string
		if err := rows.Scan(&rawRunners, &rawWorkspaces); err != nil {
			return nil, err
		}
		var runners, workspaces []string
		_ = json.Unmarshal([]byte(rawRunners), &runners)
		_ = json.Unmarshal([]byte(rawWorkspaces), &workspaces)
		for _, w := range workspaces {
			if w == ws {
				for _, r := range runners {
					out[r] = true
				}
				break
			}
		}
	}
	return out, rows.Err()
}
