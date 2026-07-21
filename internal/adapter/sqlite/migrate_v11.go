package sqlite

import (
	"context"
	"database/sql"

	_ "modernc.org/sqlite"
)

func migrateV11(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 11, []string{
		`ALTER TABLE tool_confirmation_deliveries ADD COLUMN slack_message_ts TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tool_confirmation_deliveries ADD COLUMN renderer_mode TEXT NOT NULL DEFAULT ''`,
	})
}
