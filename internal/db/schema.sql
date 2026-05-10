-- Schema for skopos
-- Uses UUIDv7 for primary keys
CREATE TABLE IF NOT EXISTS samples (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
