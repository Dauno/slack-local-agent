package sqlite

import (
	"context"
	"database/sql"
)

func migrateV16(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 16, []string{
		`CREATE TABLE standard_progress_operations (
			id TEXT PRIMARY KEY,
			conversation_key TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			thread_ts TEXT NOT NULL,
			message_ts TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(channel_id) > 0),
			CHECK (length(thread_ts) > 0),
			CHECK (state IN ('working', 'waiting_confirmation', 'finalizing', 'cleared', 'failed', 'interrupted'))
		) WITHOUT ROWID`,
		`CREATE INDEX standard_progress_recovery ON standard_progress_operations (state, updated_at)`,
		`CREATE TABLE standard_prompt_deliveries (
			id TEXT PRIMARY KEY,
			team_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			message_ts TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'prepared',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE (team_id, user_id),
			CHECK (length(id) > 0),
			CHECK (length(team_id) > 0),
			CHECK (length(user_id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (status IN ('prepared', 'published'))
		) WITHOUT ROWID`,
	})
}
