package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

type migrationFunc func(context.Context, *sql.Tx) error

var migrations = map[int]migrationFunc{
	1:  migrateV1,
	2:  migrateV2,
	3:  migrateV3,
	4:  migrateV4,
	5:  migrateV5,
	6:  migrateV6,
	7:  migrateV7,
	8:  migrateV8,
	9:  migrateV9,
	10: migrateV10,
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > SchemaVersion {
		return &FutureSchemaError{Found: current, Supported: SchemaVersion}
	}
	if current > 0 && current < SchemaVersion {
		return &StateResetNeededError{Found: current, Supported: SchemaVersion}
	}

	for version := current + 1; version <= SchemaVersion; version++ {
		fn, ok := migrations[version]
		if !ok {
			return fmt.Errorf("no SQLite migration registered for version %d", version)
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}

// migrateV9 adds immutable ownership for person topics. Legacy topics are
// claimed only when their evidence identifies one Slack workspace and user.
// Ambiguous or evidence-free topics remain unowned.
func migrateV9(ctx context.Context, tx *sql.Tx) error {
	if err := execMigration(ctx, tx, 9, []string{
		`ALTER TABLE memory_topics ADD COLUMN owner_key TEXT NOT NULL DEFAULT ''`,
		`WITH inferred_owners AS (
			SELECT r.topic_id,
				'slack:' || substr(e.source_key, 7, instr(substr(e.source_key, 7), ':') - 1) || ':user:' || e.author_id AS owner_key
			FROM memory_topic_revisions r
			JOIN memory_evidence e ON e.topic_revision = r.id
			WHERE e.source_key GLOB 'slack:*:*' AND length(e.author_id) > 0
		), unambiguous_owners AS (
			SELECT topic_id, MIN(owner_key) AS owner_key
			FROM inferred_owners
			GROUP BY topic_id
			HAVING COUNT(DISTINCT owner_key) = 1
		)
		UPDATE memory_topics
		SET owner_key = (
			SELECT owner_key FROM unambiguous_owners WHERE topic_id = memory_topics.id
		)
		WHERE bundle_path = 'people'
			AND id IN (SELECT topic_id FROM unambiguous_owners)`,
		`CREATE INDEX memory_topics_by_owner ON memory_topics (owner_key, bundle_path, updated_at DESC)`,
		`CREATE TRIGGER memory_topics_require_person_owner
			BEFORE INSERT ON memory_topics
			WHEN NEW.bundle_path = 'people' AND length(NEW.owner_key) = 0
			BEGIN SELECT RAISE(ABORT, 'person topic requires an owner'); END`,
		`CREATE TRIGGER memory_topics_owner_immutable
			BEFORE UPDATE OF owner_key ON memory_topics
			WHEN OLD.owner_key != NEW.owner_key
			BEGIN SELECT RAISE(ABORT, 'person topic owner is immutable'); END`,
	}); err != nil {
		return err
	}
	return nil
}

func execMigration(ctx context.Context, tx *sql.Tx, version int, statements []string) error {
	for i, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply SQLite schema v%d statement %d: %w", version, i+1, err)
		}
	}
	return nil
}

// migrateV8 adds bundle_path to memory_topics so the projector can render
// topics into an OKF directory tree. Existing topics default to 'topics'.
func migrateV8(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 8, []string{
		`ALTER TABLE memory_topics ADD COLUMN bundle_path TEXT NOT NULL DEFAULT 'topics'`,
	})
}

// migrateV7 adds the Slack metadata correlation used to prove a prepared
// exchange was published. Existing prepared rows intentionally receive no ID:
// their prior content/time matching is unsafe, so they remain unresolved.
func migrateV7(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 7, []string{
		`ALTER TABLE memory_exchange_intents ADD COLUMN correlation_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX memory_exchange_intents_by_correlation
			ON memory_exchange_intents (correlation_id)
			WHERE length(correlation_id) > 0`,
	})
}

// migrateV6 separates durable pre-publish intents from replies confirmed by
// Slack. Existing v5 rows are conservatively treated as prepared and require
// Slack correlation before they can be finalized locally.
func migrateV6(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 6, []string{
		`ALTER TABLE memory_exchange_intents ADD COLUMN publish_status TEXT NOT NULL DEFAULT 'prepared'`,
		`DROP INDEX memory_exchange_intents_by_conversation_and_source`,
		`CREATE UNIQUE INDEX memory_exchange_intents_by_published_message
			ON memory_exchange_intents (conversation_key, assistant_external_ts)
			WHERE publish_status = 'published'`,
	})
}

func migrateV5(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 5, []string{
		`CREATE TABLE memory_exchange_intents (
			id TEXT PRIMARY KEY,
			conversation_key TEXT NOT NULL,
			team_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			channel_kind TEXT NOT NULL,
			root_ts TEXT NOT NULL,
			last_ts TEXT NOT NULL,
			assistant_content TEXT NOT NULL,
			assistant_external_ts TEXT NOT NULL,
			assistant_created_at INTEGER NOT NULL,
			retain INTEGER NOT NULL,
			source_messages TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			CHECK (length(id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (channel_kind IN ('dm', 'channel', 'group')),
			CHECK (retain > 0)
		)`,
		`CREATE UNIQUE INDEX memory_exchange_intents_by_conversation_and_source
			ON memory_exchange_intents (conversation_key, assistant_external_ts)`,
	})
}

func migrateV4(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 4, []string{
		`ALTER TABLE memory_outbox ADD COLUMN source_messages TEXT NOT NULL DEFAULT '[]'`,
	})
}

func migrateV3(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 3, []string{
		`ALTER TABLE memory_outbox ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX memory_outbox_by_processing_lease ON memory_outbox (status, lease_until)`,
		`CREATE TABLE memory_patch_receipts (
			conversation_key TEXT NOT NULL,
			exchange_ts TEXT NOT NULL,
			applied_at INTEGER NOT NULL,
			PRIMARY KEY (conversation_key, exchange_ts),
			CHECK (length(conversation_key) > 0),
			CHECK (length(exchange_ts) > 0)
		) WITHOUT ROWID`,
	})
}

func migrateV2(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 2, []string{
		`CREATE TABLE memory_topics (
			id TEXT PRIMARY KEY,
			slug TEXT NOT NULL UNIQUE,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			tags TEXT NOT NULL DEFAULT '[]',
			content TEXT NOT NULL DEFAULT '',
			current_rev INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(id) > 0),
			CHECK (length(slug) > 0),
			CHECK (length(title) > 0),
			CHECK (status IN ('active', 'archived')),
			CHECK (current_rev >= 0)
		)`,
		`CREATE INDEX memory_topics_by_slug ON memory_topics (slug)`,
		`CREATE INDEX memory_topics_by_status ON memory_topics (status, updated_at DESC)`,

		`CREATE TABLE memory_topic_revisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			topic_id TEXT NOT NULL,
			revision_number INTEGER NOT NULL,
			content TEXT NOT NULL,
			change_reason TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			FOREIGN KEY (topic_id) REFERENCES memory_topics(id) ON DELETE CASCADE,
			CHECK (revision_number > 0),
			CHECK (length(content) > 0)
		)`,
		`CREATE UNIQUE INDEX memory_topic_revisions_by_topic_and_rev
			ON memory_topic_revisions (topic_id, revision_number)`,

		`CREATE TABLE memory_evidence (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			topic_revision INTEGER NOT NULL,
			source_key TEXT NOT NULL,
			source_ts TEXT NOT NULL,
			author_id TEXT NOT NULL,
			type TEXT NOT NULL,
			FOREIGN KEY (topic_revision) REFERENCES memory_topic_revisions(id) ON DELETE CASCADE,
			CHECK (length(source_key) > 0),
			CHECK (type IN ('source', 'decision'))
		)`,

		`CREATE TABLE memory_topic_links (
			source_topic_id TEXT NOT NULL,
			target_topic_id TEXT NOT NULL,
			relation TEXT NOT NULL,
			revision_id INTEGER NOT NULL,
			PRIMARY KEY (source_topic_id, target_topic_id),
			FOREIGN KEY (source_topic_id) REFERENCES memory_topics(id) ON DELETE CASCADE,
			FOREIGN KEY (target_topic_id) REFERENCES memory_topics(id) ON DELETE CASCADE,
			FOREIGN KEY (revision_id) REFERENCES memory_topic_revisions(id) ON DELETE CASCADE,
			CHECK (length(source_topic_id) > 0),
			CHECK (length(target_topic_id) > 0),
			CHECK (source_topic_id != target_topic_id)
		)`,

		`CREATE TABLE memory_outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_key TEXT NOT NULL DEFAULT '',
			exchange_ts TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt INTEGER NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (status IN ('pending', 'processing', 'done', 'failed')),
			CHECK (attempts >= 0)
		)`,
		`CREATE INDEX memory_outbox_by_status_and_next
			ON memory_outbox (status, next_attempt)`,

		`CREATE VIRTUAL TABLE memory_topics_fts USING fts5(
			title,
			description,
			tags,
			content,
			tokenize='unicode61'
		)`,
	})
}

func migrateV1(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 1, []string{
		`CREATE TABLE dedupe_records (
			dedupe_key TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			CHECK (length(dedupe_key) > 0),
			CHECK (expires_at > created_at)
		) WITHOUT ROWID`,
		`CREATE INDEX dedupe_records_by_expiry
			ON dedupe_records (expires_at)`,
		`CREATE TABLE conversations (
			conversation_key TEXT PRIMARY KEY,
			team_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			channel_kind TEXT NOT NULL,
			root_ts TEXT NOT NULL,
			last_ts TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(conversation_key) > 0),
			CHECK (length(team_id) > 0),
			CHECK (length(channel_id) > 0),
			CHECK (length(last_ts) > 0),
			CHECK (channel_kind IN ('dm', 'channel', 'group')),
			CHECK (
				(channel_kind = 'dm' AND root_ts = '') OR
				(channel_kind IN ('channel', 'group') AND length(root_ts) > 0)
			)
		) WITHOUT ROWID`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_key TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			user_id TEXT NOT NULL,
			external_ts TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (conversation_key) REFERENCES conversations(conversation_key) ON DELETE CASCADE,
			CHECK (role IN ('user', 'assistant'))
		)`,
		`CREATE INDEX messages_by_conversation_and_time
			ON messages (conversation_key, created_at DESC, id DESC)`,
	})
}
