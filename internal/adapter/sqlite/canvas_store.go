package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type CanvasOperationStore struct {
	db *sql.DB
}

func NewCanvasOperationStore(s *Store) *CanvasOperationStore {
	if s == nil || s.db == nil {
		return nil
	}
	return &CanvasOperationStore{db: s.db}
}

func (s *CanvasOperationStore) CreateOperation(ctx context.Context, op domain.CanvasOperation) error {
	createdAt := op.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := op.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO canvas_operations (id, conversation_key, actor, title, content_sha256, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		op.ID, string(op.ConversationKey), op.Actor, op.Title, op.ContentSHA256, string(op.Status), createdAt.UnixNano(), updatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("create canvas operation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("create canvas operation rows affected: %w", err)
	}
	if affected == 0 {
		return port.ErrCanvasOperationExists
	}
	return nil
}

func (s *CanvasOperationStore) UpdateOperationStatus(ctx context.Context, operationID string, status domain.CanvasOperationStatus, canvasID string) error {
	nowNanos := time.Now().UTC().UnixNano()
	result, err := s.db.ExecContext(ctx, `
		UPDATE canvas_operations SET status = ?, canvas_id = ?, updated_at = ? WHERE id = ?`,
		string(status), canvasID, nowNanos, operationID,
	)
	if err != nil {
		return fmt.Errorf("update canvas operation status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update canvas operation rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("update canvas operation: operation %q not found", operationID)
	}
	return nil
}

func (s *CanvasOperationStore) GetOperation(ctx context.Context, operationID string) (*domain.CanvasOperation, error) {
	var op domain.CanvasOperation
	var key string
	var createdAtNanos, updatedAtNanos int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_key, actor, title, content_sha256, status, canvas_id, created_at, updated_at
		FROM canvas_operations WHERE id = ?`, operationID).Scan(
		&op.ID, &key, &op.Actor, &op.Title, &op.ContentSHA256, &op.Status, &op.CanvasID, &createdAtNanos, &updatedAtNanos,
	)
	if err != nil {
		return nil, fmt.Errorf("get canvas operation: %w", err)
	}
	op.ConversationKey = domain.ConversationKey(key)
	op.CreatedAt = time.Unix(0, createdAtNanos).UTC()
	op.UpdatedAt = time.Unix(0, updatedAtNanos).UTC()
	return &op, nil
}

var _ port.CanvasOperationStore = (*CanvasOperationStore)(nil)
