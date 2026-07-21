package sqlite

import (
	"context"
	"database/sql"
)

func migrateV14(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 14, []string{
		`CREATE TABLE generated_file_operations (
			id TEXT PRIMARY KEY,
			conversation_key TEXT NOT NULL,
			actor TEXT NOT NULL,
			filename TEXT NOT NULL,
			media_type TEXT NOT NULL,
			content_sha256 TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending_confirmation',
			slack_file_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(actor) > 0),
			CHECK (length(filename) > 0),
			CHECK (size_bytes > 0),
			CHECK (status IN ('pending_confirmation', 'url_requested', 'bytes_uploaded', 'completed', 'failed', 'unknown'))
		) WITHOUT ROWID`,
		`CREATE INDEX generated_file_operations_by_status ON generated_file_operations (status, created_at DESC)`,
	})
}
