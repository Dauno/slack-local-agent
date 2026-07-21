package sqlite

import (
	"context"
	"database/sql"
)

// migrateV15 stores global durable runtime choices that cannot be changed by a
// feature toggle without invalidating conversation identity.
func migrateV15(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 15, []string{
		`CREATE TABLE runtime_state (
			state_key TEXT PRIMARY KEY,
			state_value TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(state_key) > 0),
			CHECK (length(state_value) > 0)
		) WITHOUT ROWID`,
	})
}
