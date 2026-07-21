package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type GeneratedFileOperationStore struct{ db *sql.DB }

func NewGeneratedFileOperationStore(s *Store) *GeneratedFileOperationStore {
	if s == nil || s.db == nil {
		return nil
	}
	return &GeneratedFileOperationStore{db: s.db}
}

func (s *GeneratedFileOperationStore) CreateGeneratedFileOperation(ctx context.Context, op domain.GeneratedFileOperation) error {
	createdAt := op.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := op.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO generated_file_operations (id, conversation_key, actor, filename, media_type, content_sha256, size_bytes, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		op.ID, string(op.ConversationKey), op.Actor, op.Filename, op.MediaType, op.ContentSHA256, op.SizeBytes, string(op.Status), createdAt.UnixNano(), updatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("create generated file operation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("create generated file operation rows affected: %w", err)
	}
	if affected == 0 {
		return port.ErrGeneratedFileOperationExists
	}
	return nil
}

func (s *GeneratedFileOperationStore) UpdateGeneratedFileOperation(ctx context.Context, operationID string, status domain.GeneratedFileOperationStatus, slackFileID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE generated_file_operations SET status = ?, slack_file_id = ?, updated_at = ? WHERE id = ?`, string(status), slackFileID, time.Now().UTC().UnixNano(), operationID)
	if err != nil {
		return fmt.Errorf("update generated file operation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update generated file operation rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("update generated file operation: operation %q not found", operationID)
	}
	return nil
}

func (s *GeneratedFileOperationStore) GetGeneratedFileOperation(ctx context.Context, operationID string) (*domain.GeneratedFileOperation, error) {
	var op domain.GeneratedFileOperation
	var key string
	var createdAtNanos, updatedAtNanos int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_key, actor, filename, media_type, content_sha256, size_bytes, status, slack_file_id, created_at, updated_at
		FROM generated_file_operations WHERE id = ?`, operationID).Scan(&op.ID, &key, &op.Actor, &op.Filename, &op.MediaType, &op.ContentSHA256, &op.SizeBytes, &op.Status, &op.SlackFileID, &createdAtNanos, &updatedAtNanos)
	if err != nil {
		return nil, fmt.Errorf("get generated file operation: %w", err)
	}
	op.ConversationKey = domain.ConversationKey(key)
	op.CreatedAt = time.Unix(0, createdAtNanos).UTC()
	op.UpdatedAt = time.Unix(0, updatedAtNanos).UTC()
	return &op, nil
}

var _ port.GeneratedFileOperationStore = (*GeneratedFileOperationStore)(nil)
