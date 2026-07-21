package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.StandardExperienceStore = (*Store)(nil)

func (s *Store) CreateProgress(ctx context.Context, operation domain.ProgressOperation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO standard_progress_operations
			(id, conversation_key, channel_id, thread_ts, message_ts, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, string(operation.ConversationKey), operation.ChannelID,
		operation.ThreadTS, operation.MessageTS, string(operation.State), operation.CreatedAt.UnixNano(), operation.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("create standard progress operation: %w", err)
	}
	return nil
}

func (s *Store) MarkProgressPublished(ctx context.Context, operationID, messageTS string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE standard_progress_operations SET message_ts = ?, updated_at = ? WHERE id = ? AND message_ts = ''`, messageTS, time.Now().UTC().UnixNano(), operationID)
	if err != nil {
		return fmt.Errorf("mark standard progress published: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect standard progress publication: %w", err)
	}
	if changed == 0 {
		var existing string
		if err := s.db.QueryRowContext(ctx, `SELECT message_ts FROM standard_progress_operations WHERE id = ?`, operationID).Scan(&existing); err != nil {
			return fmt.Errorf("read standard progress publication: %w", err)
		}
		if existing != messageTS {
			return errors.New("standard progress publication conflicts with persisted message")
		}
	}
	return nil
}

func (s *Store) SetProgressState(ctx context.Context, operationID string, state domain.ProgressState, updatedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE standard_progress_operations SET state = ?, updated_at = ? WHERE id = ?`, string(state), updatedAt.UnixNano(), operationID); err != nil {
		return fmt.Errorf("update standard progress state: %w", err)
	}
	return nil
}

func (s *Store) ListRecoverableProgress(ctx context.Context) ([]domain.ProgressOperation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_key, channel_id, thread_ts, message_ts, state, created_at, updated_at
		FROM standard_progress_operations
		WHERE state NOT IN ('cleared', 'failed', 'interrupted')
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list recoverable standard progress: %w", err)
	}
	defer rows.Close()
	var operations []domain.ProgressOperation
	for rows.Next() {
		operation, err := scanProgress(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable standard progress: %w", err)
	}
	return operations, nil
}

func (s *Store) FindWaitingProgress(ctx context.Context, key domain.ConversationKey) (*domain.ProgressOperation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_key, channel_id, thread_ts, message_ts, state, created_at, updated_at
		FROM standard_progress_operations
		WHERE conversation_key = ? AND state = 'waiting_confirmation'
		ORDER BY updated_at DESC LIMIT 1`, string(key))
	operation, err := scanProgress(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

type progressScanner interface {
	Scan(...any) error
}

func scanProgress(scanner progressScanner) (domain.ProgressOperation, error) {
	var operation domain.ProgressOperation
	var state string
	var createdAt, updatedAt int64
	if err := scanner.Scan(&operation.ID, &operation.ConversationKey, &operation.ChannelID, &operation.ThreadTS, &operation.MessageTS, &state, &createdAt, &updatedAt); err != nil {
		return domain.ProgressOperation{}, err
	}
	operation.State = domain.ProgressState(state)
	operation.CreatedAt = time.Unix(0, createdAt).UTC()
	operation.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return operation, nil
}

func (s *Store) ClaimSuggestedPrompts(ctx context.Context, teamID, userID string, key domain.ConversationKey, createdAt time.Time) (string, bool, error) {
	deliveryID := "standard_prompts:" + teamID + ":" + userID
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO standard_prompt_deliveries
			(id, team_id, user_id, conversation_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id, user_id) DO NOTHING`, deliveryID, teamID, userID, string(key), createdAt.UnixNano(), createdAt.UnixNano())
	if err != nil {
		return "", false, fmt.Errorf("claim standard suggested prompts: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("inspect standard suggested prompt claim: %w", err)
	}
	return deliveryID, changed == 1, nil
}

func (s *Store) MarkSuggestedPromptsPublished(ctx context.Context, deliveryID, messageTS string, updatedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE standard_prompt_deliveries SET status = 'published', message_ts = ?, updated_at = ? WHERE id = ? AND status = 'prepared'`, messageTS, updatedAt.UnixNano(), deliveryID); err != nil {
		return fmt.Errorf("mark standard suggested prompts published: %w", err)
	}
	return nil
}

func (s *Store) PrepareIncremental(ctx context.Context, operation domain.IncrementalOperation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO standard_incremental_operations
			(id, conversation_key, channel_id, thread_ts, message_ts, renderer_version, latest_sequence, prefix_digest, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, string(operation.ConversationKey), operation.ChannelID,
		operation.ThreadTS, operation.MessageTS, operation.RendererVersion, operation.Sequence, operation.PrefixDigest,
		string(operation.Status), operation.CreatedAt.UnixNano(), operation.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("prepare standard incremental operation: %w", err)
	}
	return nil
}

func (s *Store) MarkIncrementalCreated(ctx context.Context, operationID, messageTS string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE standard_incremental_operations
		SET message_ts = ?, status = 'message_created', updated_at = ?
		WHERE id = ? AND status = 'prepared' AND message_ts = ''`, messageTS, updatedAt.UnixNano(), operationID)
	if err != nil {
		return fmt.Errorf("mark standard incremental message created: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect standard incremental creation: %w", err)
	}
	if changed == 0 {
		var existing string
		if err := s.db.QueryRowContext(ctx, `SELECT message_ts FROM standard_incremental_operations WHERE id = ?`, operationID).Scan(&existing); err != nil {
			return fmt.Errorf("read standard incremental creation: %w", err)
		}
		if existing != messageTS {
			return errors.New("standard incremental message conflicts with persisted identity")
		}
	}
	return nil
}

func (s *Store) AdvanceIncremental(ctx context.Context, operationID string, status domain.IncrementalStatus, sequence int, prefixDigest string, updatedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE standard_incremental_operations
		SET status = ?, latest_sequence = ?, prefix_digest = ?, updated_at = ?
		WHERE id = ?`, string(status), sequence, prefixDigest, updatedAt.UnixNano(), operationID); err != nil {
		return fmt.Errorf("advance standard incremental operation: %w", err)
	}
	return nil
}

func (s *Store) ListUnfinishedIncremental(ctx context.Context) ([]domain.IncrementalOperation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_key, channel_id, thread_ts, message_ts, renderer_version,
			latest_sequence, prefix_digest, status, created_at, updated_at
		FROM standard_incremental_operations
		WHERE status NOT IN ('finalized', 'interrupted')
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list unfinished standard incremental operations: %w", err)
	}
	defer rows.Close()
	var operations []domain.IncrementalOperation
	for rows.Next() {
		var operation domain.IncrementalOperation
		var status string
		var createdAt, updatedAt int64
		if err := rows.Scan(&operation.ID, &operation.ConversationKey, &operation.ChannelID, &operation.ThreadTS, &operation.MessageTS,
			&operation.RendererVersion, &operation.Sequence, &operation.PrefixDigest, &status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan unfinished standard incremental operation: %w", err)
		}
		operation.Status = domain.IncrementalStatus(status)
		operation.CreatedAt = time.Unix(0, createdAt).UTC()
		operation.UpdatedAt = time.Unix(0, updatedAt).UTC()
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unfinished standard incremental operations: %w", err)
	}
	return operations, nil
}
