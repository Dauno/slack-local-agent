package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ConversationStore = (*Store)(nil)
var _ port.AssistantExchangeWriter = (*Store)(nil)

func (s *Store) ClaimDedupe(
	ctx context.Context,
	keys []string,
	createdAt time.Time,
	expiresAt time.Time,
) (bool, error) {
	uniqueKeys, err := validateDedupeClaim(keys, createdAt, expiresAt)
	if err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("begin dedupe claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowNanos := createdAt.UnixNano()
	for _, key := range uniqueKeys {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM dedupe_records WHERE dedupe_key = ? AND expires_at <= ?`,
			key, nowNanos,
		); err != nil {
			return false, fmt.Errorf("remove expired dedupe key: %w", err)
		}
	}

	for _, key := range uniqueKeys {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO dedupe_records (dedupe_key, created_at, expires_at)
			VALUES (?, ?, ?)
			ON CONFLICT (dedupe_key) DO NOTHING`,
			key, nowNanos, expiresAt.UnixNano(),
		)
		if err != nil {
			return false, fmt.Errorf("claim dedupe key: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("inspect dedupe claim: %w", err)
		}
		if inserted == 0 {
			return false, nil
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit dedupe claim: %w", err)
	}
	return true, nil
}

func validateDedupeClaim(keys []string, createdAt, expiresAt time.Time) ([]string, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one dedupe key is required")
	}
	if !expiresAt.After(createdAt) {
		return nil, errors.New("dedupe expiry must be after creation time")
	}
	unique := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return nil, errors.New("dedupe keys must not be empty")
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	return unique, nil
}

func (s *Store) CleanupDedupe(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM dedupe_records WHERE expires_at <= ?`, now.UnixNano(),
	); err != nil {
		return fmt.Errorf("clean expired dedupe records: %w", err)
	}
	return nil
}

func (s *Store) HasAssistantMessage(ctx context.Context, key domain.ConversationKey) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE conversation_key = ? AND role = 'assistant'
		)`, string(key),
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check assistant participation: %w", err)
	}
	return exists, nil
}

func (s *Store) RecentMessages(
	ctx context.Context,
	key domain.ConversationKey,
	limit int,
) ([]domain.Message, error) {
	if limit <= 0 {
		return []domain.Message{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, user_id, external_ts, created_at
		FROM (
			SELECT id, role, content, user_id, external_ts, created_at
			FROM messages
			WHERE conversation_key = ?
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		)
		ORDER BY created_at ASC, id ASC`, string(key), limit)
	if err != nil {
		return nil, fmt.Errorf("read recent conversation messages: %w", err)
	}
	defer rows.Close()

	messages := make([]domain.Message, 0, limit)
	for rows.Next() {
		var (
			message      domain.Message
			role         string
			createdNanos int64
		)
		if err := rows.Scan(
			&role,
			&message.Content,
			&message.UserID,
			&message.ExternalTS,
			&createdNanos,
		); err != nil {
			return nil, fmt.Errorf("scan conversation message: %w", err)
		}
		message.Role = domain.Role(role)
		message.CreatedAt = time.Unix(0, createdNanos).UTC()
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation messages: %w", err)
	}
	return messages, nil
}

func (s *Store) AppendMessage(
	ctx context.Context,
	metadata domain.ConversationMetadata,
	message domain.Message,
	retain int,
) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin append conversation message: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := appendMessageTx(ctx, tx, metadata, message, retain); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation message: %w", err)
	}
	return nil
}

// PrepareAssistantExchange records the complete exchange before publishing so a
// later database failure cannot lose a reply Slack has already accepted.
type sourceMessagesWrapper struct {
	MemoryEligible bool              `json:"memory_eligible"`
	Messages       []json.RawMessage `json:"messages"`
}

func (s *Store) prepareAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, presentationJSON string, retain int, memoryEligible bool) (port.PreparedAssistantExchange, error) {
	if message.Role != domain.RoleAssistant {
		return port.PreparedAssistantExchange{}, fmt.Errorf("assistant exchange requires assistant role, got %q", message.Role)
	}
	if retain <= 0 {
		return port.PreparedAssistantExchange{}, errors.New("message retention must be positive")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("begin prepare assistant exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	source, err := sourceExchangeTx(ctx, tx, metadata.Key)
	if err != nil {
		return port.PreparedAssistantExchange{}, err
	}
	source = append(source, message)
	payload, err := json.Marshal(sourceMessagesWrapper{
		MemoryEligible: memoryEligible,
		Messages:       marshalMessages(source),
	})
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("encode assistant exchange source: %w", err)
	}
	nowNanos := time.Now().UTC().UnixNano()
	intentID := generateTopicID()
	correlationID := "assistant_exchange_" + generateTopicID()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_exchange_intents (
			id, conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts,
			assistant_content, assistant_external_ts, assistant_created_at, retain, source_messages, created_at, publish_status, correlation_id, presentation_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'prepared', ?, ?)`,
		intentID, string(metadata.Key), metadata.TeamID, metadata.ChannelID, string(metadata.ChannelKind), metadata.RootTS, metadata.LastTS,
		message.Content, message.ExternalTS, message.CreatedAt.UnixNano(), retain, string(payload), nowNanos, correlationID, presentationJSON,
	); err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("prepare assistant exchange: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("commit prepared assistant exchange: %w", err)
	}
	return port.PreparedAssistantExchange{ID: intentID, CorrelationID: correlationID}, nil
}

func (s *Store) PrepareAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, retain int, memoryEligible bool) (port.PreparedAssistantExchange, error) {
	return s.prepareAssistantExchange(ctx, metadata, message, "", retain, memoryEligible)
}

func (s *Store) PrepareStructuredAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, presentationJSON string, retain int, memoryEligible bool) (port.PreparedAssistantExchange, error) {
	return s.prepareAssistantExchange(ctx, metadata, message, presentationJSON, retain, memoryEligible)
}

// MarkAssistantExchangePublished records Slack's actual response timestamp
// after Publish succeeds. A prepared intent cannot be finalized locally.
func (s *Store) MarkAssistantExchangePublished(ctx context.Context, intentID, assistantTS string) error {
	if strings.TrimSpace(assistantTS) == "" {
		return errors.New("published assistant timestamp is required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE memory_exchange_intents
		SET assistant_external_ts = ?, publish_status = 'published'
		WHERE id = ? AND publish_status = 'prepared'`, assistantTS, intentID)
	if err != nil {
		return fmt.Errorf("mark assistant exchange published: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect assistant exchange publication: %w", err)
	}
	if changed == 0 {
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT publish_status FROM memory_exchange_intents WHERE id = ?`, intentID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) || status == "published" {
			return nil // A prior mark/finalization committed before its caller observed success.
		}
		if err != nil {
			return fmt.Errorf("read assistant exchange publication: %w", err)
		}
		return fmt.Errorf("mark assistant exchange published: unexpected status %q", status)
	}
	return nil
}

// FinalizeAssistantExchange atomically persists a Slack-confirmed reply and
// its outbox work item, consuming its durable published intent.
func (s *Store) FinalizeAssistantExchange(ctx context.Context, intentID string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin finalize assistant exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	intent, err := loadAssistantExchangeIntent(ctx, tx, intentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // A prior finalization committed before its caller observed success.
	}
	if err != nil {
		return err
	}
	if intent.PublishStatus != "published" || strings.TrimSpace(intent.AssistantExternalTS) == "" {
		return errors.New("cannot finalize an assistant exchange that is not published")
	}
	metadata := intent.metadata()
	message := intent.assistantMessage()
	if err := appendMessageTx(ctx, tx, metadata, message, intent.Retain); err != nil {
		return err
	}
	source, eligible, err := decodeSourceMessages(intent.SourceMessages, message)
	if err != nil {
		return err
	}
	if eligible {
		payload, err := json.Marshal(source)
		if err != nil {
			return fmt.Errorf("encode finalized assistant exchange: %w", err)
		}
		nowNanos := time.Now().UTC().UnixNano()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memory_outbox (conversation_key, exchange_ts, source_messages, status, attempts, next_attempt, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', 0, ?, ?, ?)`,
			string(metadata.Key), message.ExternalTS, string(payload), nowNanos, nowNanos, nowNanos,
		); err != nil {
			return fmt.Errorf("enqueue assistant exchange: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_exchange_intents WHERE id = ?`, intentID); err != nil {
		return fmt.Errorf("delete finalized assistant exchange intent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finalized assistant exchange: %w", err)
	}
	return nil
}

func (s *Store) DiscardAssistantExchange(ctx context.Context, intentID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM memory_exchange_intents WHERE id = ?`, intentID); err != nil {
		return fmt.Errorf("discard assistant exchange intent: %w", err)
	}
	return nil
}

// ReconcileAssistantExchanges finalizes only published intents. Prepared
// intents are finalized only after Slack exposes their durable correlation ID;
// legacy rows without one intentionally remain unresolved.
func (s *Store) ReconcileAssistantExchanges(ctx context.Context, finder port.AssistantExchangeFinder) error {
	for {
		var intentID string
		err := s.db.QueryRowContext(ctx, `
			SELECT id FROM memory_exchange_intents
			WHERE publish_status = 'published'
			ORDER BY created_at ASC LIMIT 1`).Scan(&intentID)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return fmt.Errorf("select assistant exchange intent: %w", err)
		}
		intent, err := s.getAssistantExchangeIntent(ctx, intentID)
		if err != nil {
			return err
		}
		if err := s.FinalizeAssistantExchange(ctx, intent.ID); err != nil {
			return fmt.Errorf("reconcile assistant exchange %q: %w", intent.ID, err)
		}
	}
	if finder == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM memory_exchange_intents
		WHERE publish_status = 'prepared' AND length(correlation_id) > 0
		ORDER BY created_at ASC`)
	if err != nil {
		return fmt.Errorf("select prepared assistant exchange intents: %w", err)
	}
	defer rows.Close()
	var intentIDs []string
	for rows.Next() {
		var intentID string
		if err := rows.Scan(&intentID); err != nil {
			return fmt.Errorf("scan prepared assistant exchange intent: %w", err)
		}
		intentIDs = append(intentIDs, intentID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate prepared assistant exchange intents: %w", err)
	}
	type recoveryMatch struct {
		intent      assistantExchangeIntent
		assistantTS string
	}
	matchesByTimestamp := make(map[string][]recoveryMatch)
	for _, intentID := range intentIDs {
		intent, err := s.getAssistantExchangeIntent(ctx, intentID)
		if err != nil {
			return err
		}
		assistantTS, found, findErr := finder.FindPublishedAssistantExchange(ctx, intent.finderIntent())
		if findErr != nil {
			return fmt.Errorf("find published assistant exchange %q: %w", intent.ID, findErr)
		}
		if !found {
			continue
		}
		matchesByTimestamp[assistantTS] = append(matchesByTimestamp[assistantTS], recoveryMatch{intent: intent, assistantTS: assistantTS})
	}
	for _, matches := range matchesByTimestamp {
		if len(matches) != 1 {
			// A remote message cannot prove which same-content intent it fulfills.
			continue
		}
		match := matches[0]
		if err := s.MarkAssistantExchangePublished(ctx, match.intent.ID, match.assistantTS); err != nil {
			return fmt.Errorf("mark reconciled assistant exchange %q published: %w", match.intent.ID, err)
		}
		if err := s.FinalizeAssistantExchange(ctx, match.intent.ID); err != nil {
			return fmt.Errorf("reconcile assistant exchange %q: %w", match.intent.ID, err)
		}
	}
	return nil
}

type assistantExchangeIntent struct {
	ID                  string
	ConversationKey     domain.ConversationKey
	TeamID              string
	ChannelID           string
	ChannelKind         domain.ChannelKind
	RootTS              string
	LastTS              string
	AssistantContent    string
	AssistantExternalTS string
	AssistantCreatedAt  time.Time
	CorrelationID       string
	Retain              int
	SourceMessages      string
	PublishStatus       string
	PresentationJSON    string
}

func (s *Store) getAssistantExchangeIntent(ctx context.Context, intentID string) (assistantExchangeIntent, error) {
	return loadAssistantExchangeIntent(ctx, s.db, intentID)
}

func loadAssistantExchangeIntent(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, intentID string) (assistantExchangeIntent, error) {
	var intent assistantExchangeIntent
	var kind string
	var createdNanos int64
	err := queryer.QueryRowContext(ctx, `
		SELECT id, conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts,
			assistant_content, assistant_external_ts, assistant_created_at, retain, source_messages, publish_status, correlation_id, presentation_json
		FROM memory_exchange_intents WHERE id = ?`, intentID,
	).Scan(&intent.ID, &intent.ConversationKey, &intent.TeamID, &intent.ChannelID, &kind, &intent.RootTS, &intent.LastTS,
		&intent.AssistantContent, &intent.AssistantExternalTS, &createdNanos, &intent.Retain, &intent.SourceMessages, &intent.PublishStatus, &intent.CorrelationID, &intent.PresentationJSON)
	if err != nil {
		return assistantExchangeIntent{}, err
	}
	intent.ChannelKind = domain.ChannelKind(kind)
	intent.AssistantCreatedAt = time.Unix(0, createdNanos).UTC()
	return intent, nil
}

func (i assistantExchangeIntent) assistantMessage() domain.Message {
	return domain.Message{Role: domain.RoleAssistant, Content: i.AssistantContent, ExternalTS: i.AssistantExternalTS, CreatedAt: i.AssistantCreatedAt}
}

func (i assistantExchangeIntent) metadata() domain.ConversationMetadata {
	return domain.ConversationMetadata{
		Key: i.ConversationKey, TeamID: i.TeamID, ChannelID: i.ChannelID,
		ChannelKind: i.ChannelKind, RootTS: i.RootTS, LastTS: i.AssistantExternalTS,
	}
}

func (i assistantExchangeIntent) finderIntent() port.AssistantExchangeIntent {
	return port.AssistantExchangeIntent{
		ID: i.ID, ChannelID: i.ChannelID, ChannelKind: i.ChannelKind,
		RootTS: i.RootTS, Content: i.AssistantContent, CorrelationID: i.CorrelationID,
		PresentationJSON: i.PresentationJSON,
	}
}

func marshalMessages(messages []domain.Message) []json.RawMessage {
	result := make([]json.RawMessage, 0, len(messages))
	for _, msg := range messages {
		encoded, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		result = append(result, json.RawMessage(encoded))
	}
	return result
}

func decodeSourceMessages(sourceMessagesJSON string, assistant domain.Message) ([]domain.Message, bool, error) {
	var wrapper sourceMessagesWrapper
	if err := json.Unmarshal([]byte(sourceMessagesJSON), &wrapper); err == nil && len(wrapper.Messages) > 0 {
		eligible := wrapper.MemoryEligible
		source := make([]domain.Message, 0, len(wrapper.Messages))
		for _, raw := range wrapper.Messages {
			var msg domain.Message
			if err := json.Unmarshal(raw, &msg); err != nil {
				return nil, false, fmt.Errorf("decode assistant exchange source message: %w", err)
			}
			source = append(source, msg)
		}
		if len(source) == 0 || source[len(source)-1].Role != domain.RoleAssistant {
			return nil, false, errors.New("prepared assistant exchange source is invalid")
		}
		source[len(source)-1] = assistant
		return source, eligible, nil
	}

	// Legacy format: plain message array. Treated as memory-eligible.
	var source []domain.Message
	if err := json.Unmarshal([]byte(sourceMessagesJSON), &source); err != nil {
		return nil, false, fmt.Errorf("decode assistant exchange source: %w", err)
	}
	if len(source) == 0 || source[len(source)-1].Role != domain.RoleAssistant {
		return nil, false, errors.New("prepared assistant exchange source is invalid")
	}
	source[len(source)-1] = assistant
	return source, true, nil
}

func (i assistantExchangeIntent) sourceWithAssistant(assistant domain.Message) ([]domain.Message, error) {
	source, _, err := decodeSourceMessages(i.SourceMessages, assistant)
	return source, err
}

func appendMessageTx(ctx context.Context, tx *sql.Tx, metadata domain.ConversationMetadata, message domain.Message, retain int) error {
	if retain <= 0 {
		return errors.New("message retention must be positive")
	}
	if message.Role != domain.RoleUser && message.Role != domain.RoleAssistant {
		return fmt.Errorf("unsupported conversation role %q", message.Role)
	}

	createdNanos := message.CreatedAt.UnixNano()
	conversationRootTS := metadata.RootTS
	if metadata.ChannelKind == domain.ChannelDM {
		// The v1 schema requires an empty root for DM rows; the canonical
		// threaded root remains encoded in the conversation key.
		conversationRootTS = ""
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (
			conversation_key, team_id, channel_id, channel_kind,
			root_ts, last_ts, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (conversation_key) DO UPDATE SET
			last_ts = CASE
				WHEN excluded.last_ts > conversations.last_ts THEN excluded.last_ts
				ELSE conversations.last_ts
			END,
			updated_at = max(conversations.updated_at, excluded.updated_at)
		WHERE conversations.team_id = excluded.team_id
			AND conversations.channel_id = excluded.channel_id
			AND conversations.channel_kind = excluded.channel_kind
			AND conversations.root_ts = excluded.root_ts`,
		string(metadata.Key), metadata.TeamID, metadata.ChannelID, string(metadata.ChannelKind),
		conversationRootTS, metadata.LastTS, createdNanos, createdNanos,
	)
	if err != nil {
		return fmt.Errorf("upsert conversation metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect conversation metadata update: %w", err)
	}
	if affected == 0 {
		return ErrMetadataConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			conversation_key, role, content, user_id, external_ts, created_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		string(metadata.Key), string(message.Role), message.Content,
		message.UserID, message.ExternalTS, createdNanos,
	); err != nil {
		return fmt.Errorf("insert conversation message: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE id IN (
			SELECT id
			FROM messages
			WHERE conversation_key = ?
			ORDER BY created_at DESC, id DESC
			LIMIT -1 OFFSET ?
		)`, string(metadata.Key), retain); err != nil {
		return fmt.Errorf("prune conversation messages: %w", err)
	}

	return nil
}

func sourceExchangeTx(ctx context.Context, tx *sql.Tx, key domain.ConversationKey) ([]domain.Message, error) {
	var user domain.Message
	var role string
	var createdNanos int64
	err := tx.QueryRowContext(ctx, `
		SELECT role, content, user_id, external_ts, created_at
		FROM messages WHERE conversation_key = ? AND role = 'user'
		ORDER BY created_at DESC, id DESC LIMIT 1`, string(key),
	).Scan(&role, &user.Content, &user.UserID, &user.ExternalTS, &createdNanos)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("assistant exchange has no persisted user source")
	}
	if err != nil {
		return nil, fmt.Errorf("load assistant exchange source: %w", err)
	}
	user.Role = domain.Role(role)
	user.CreatedAt = time.Unix(0, createdNanos).UTC()
	return []domain.Message{user}, nil
}

// ProbeReadWrite verifies access to the migrated main database without leaving
// application data behind.
func (s *Store) ProbeReadWrite(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin SQLite read/write probe: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("probe SQLite read access: %w", err)
	}
	if version != SchemaVersion {
		return fmt.Errorf("probe SQLite schema version: got %d, want %d", version, SchemaVersion)
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dedupe_records (dedupe_key, created_at, expires_at)
		VALUES ('__local_agent_read_write_probe__', ?, ?)
		ON CONFLICT (dedupe_key) DO UPDATE SET
			created_at = excluded.created_at,
			expires_at = excluded.expires_at`,
		now.UnixNano(), now.Add(time.Minute).UnixNano(),
	); err != nil {
		return fmt.Errorf("probe SQLite write access: %w", err)
	}

	if err := tx.Rollback(); err != nil {
		return fmt.Errorf("rollback SQLite read/write probe: %w", err)
	}
	return nil
}
