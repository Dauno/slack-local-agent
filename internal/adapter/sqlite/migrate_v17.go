package sqlite

import (
	"context"
	"database/sql"
)

func migrateV17(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 17, []string{
		`CREATE TABLE standard_incremental_operations (
			id TEXT PRIMARY KEY,
			conversation_key TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			thread_ts TEXT NOT NULL,
			message_ts TEXT NOT NULL DEFAULT '',
			renderer_version TEXT NOT NULL,
			latest_sequence INTEGER NOT NULL DEFAULT 0,
			prefix_digest TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'prepared',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(channel_id) > 0),
			CHECK (length(thread_ts) > 0),
			CHECK (length(renderer_version) > 0),
			CHECK (latest_sequence >= 0),
			CHECK (status IN ('prepared', 'message_created', 'updating', 'finalized', 'interrupted', 'unknown'))
		) WITHOUT ROWID`,
		`CREATE INDEX standard_incremental_recovery ON standard_incremental_operations (status, updated_at)`,
	})
}
