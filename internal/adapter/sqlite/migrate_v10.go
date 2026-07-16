package sqlite

import (
	"context"
	"database/sql"

	_ "modernc.org/sqlite"
)

// migrateV10 adds the ADK session, event, and application/user state
// tables required for the durable session.Service implementation.
func migrateV10(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 10, []string{
		`CREATE TABLE adk_sessions (
			app_name TEXT NOT NULL,
			user_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			state JSON NOT NULL DEFAULT '{}',
			revision INTEGER NOT NULL DEFAULT 0,
			create_time INTEGER NOT NULL,
			update_time INTEGER NOT NULL,
			PRIMARY KEY (app_name, user_id, session_id),
			CHECK (length(app_name) > 0),
			CHECK (length(user_id) > 0),
			CHECK (length(session_id) > 0)
		) WITHOUT ROWID`,

		`CREATE TABLE adk_events (
			id TEXT NOT NULL,
			app_name TEXT NOT NULL,
			user_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			invocation_id TEXT NOT NULL DEFAULT '',
			author TEXT NOT NULL DEFAULT '',
			actions JSON NOT NULL DEFAULT '{}',
			long_running_tool_ids JSON,
			routes JSON,
			output JSON,
			node_info JSON,
			requested_input JSON,
			branch TEXT,
			isolation_scope TEXT,
			timestamp INTEGER NOT NULL,
			content JSON,
			grounding_metadata JSON,
			custom_metadata JSON,
			usage_metadata JSON,
			citation_metadata JSON,
			error_code TEXT,
			error_message TEXT,
			partial INTEGER NOT NULL DEFAULT 0,
			turn_complete INTEGER NOT NULL DEFAULT 0,
			interrupted INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (app_name, user_id, session_id, id),
			UNIQUE (app_name, user_id, session_id, ordinal),
			FOREIGN KEY (app_name, user_id, session_id) REFERENCES adk_sessions(app_name, user_id, session_id) ON DELETE CASCADE,
			CHECK (length(id) > 0)
		) WITHOUT ROWID`,

		`CREATE INDEX adk_events_by_session_time
			ON adk_events (app_name, user_id, session_id, ordinal DESC)`,

		`CREATE TABLE adk_app_state (
			app_name TEXT PRIMARY KEY,
			state JSON NOT NULL DEFAULT '{}',
			update_time INTEGER NOT NULL
		) WITHOUT ROWID`,

		`CREATE TABLE adk_user_state (
			app_name TEXT NOT NULL,
			user_id TEXT NOT NULL,
			state JSON NOT NULL DEFAULT '{}',
			update_time INTEGER NOT NULL,
			PRIMARY KEY (app_name, user_id)
		) WITHOUT ROWID`,

		`CREATE TABLE tool_confirmation_deliveries (
			wrapper_call_id TEXT PRIMARY KEY,
			original_call_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			actor TEXT NOT NULL,
			slack_team_id TEXT NOT NULL,
			slack_channel_id TEXT NOT NULL,
			slack_thread_ts TEXT NOT NULL DEFAULT '',
			presentation JSON NOT NULL DEFAULT '{}',
			expiry INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			correlation_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(wrapper_call_id) > 0),
			CHECK (length(original_call_id) > 0),
			CHECK (status IN ('pending', 'published', 'approved', 'rejected', 'expired', 'consumed', 'failed'))
		) WITHOUT ROWID`,

		`CREATE INDEX tool_confirmation_deliveries_by_status
			ON tool_confirmation_deliveries (status, expiry)`,

		`CREATE TABLE tool_execution_audit (
			original_call_id TEXT PRIMARY KEY,
			capability TEXT NOT NULL,
			actor TEXT NOT NULL,
			authorization_result TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL DEFAULT '',
			lifecycle_state TEXT NOT NULL DEFAULT 'requested',
			created_at INTEGER NOT NULL,
			completed_at INTEGER NOT NULL DEFAULT 0,
			CHECK (length(original_call_id) > 0),
			CHECK (lifecycle_state IN ('requested', 'authorized', 'running', 'completed', 'failed', 'rejected'))
		) WITHOUT ROWID`,

		`CREATE INDEX tool_execution_audit_by_state
			ON tool_execution_audit (lifecycle_state, created_at DESC)`,
	})
}
