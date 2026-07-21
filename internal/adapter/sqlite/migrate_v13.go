package sqlite

import (
	"context"
	"database/sql"
)

func migrateV13(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 13, []string{
		`CREATE TABLE canvas_operations (
			id TEXT PRIMARY KEY,
			conversation_key TEXT NOT NULL,
			actor TEXT NOT NULL,
			title TEXT NOT NULL,
			content_sha256 TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'ready',
			canvas_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(actor) > 0),
			CHECK (length(title) > 0),
			CHECK (status IN ('ready', 'creating', 'completed', 'failed', 'unknown'))
		) WITHOUT ROWID`,
		`CREATE INDEX canvas_operations_by_status ON canvas_operations (status, created_at DESC)`,
	})
}
