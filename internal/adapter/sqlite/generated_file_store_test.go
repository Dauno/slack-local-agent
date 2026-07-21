package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestGeneratedFileOperationStorePersistsMetadataWithoutContent(t *testing.T) {
	store, _ := newTestStore(t)
	operations := NewGeneratedFileOperationStore(store)
	createdAt := time.Date(2026, 7, 21, 12, 0, 0, 123, time.UTC)
	op := domain.GeneratedFileOperation{ID: "file:call-1", ConversationKey: "slack:T:dm:D", Actor: "U1", Filename: "report.csv", MediaType: "text/csv", ContentSHA256: "abc", SizeBytes: 12, Status: domain.GeneratedFileOpPendingConfirmation, CreatedAt: createdAt, UpdatedAt: createdAt}
	if err := operations.CreateGeneratedFileOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if err := operations.CreateGeneratedFileOperation(context.Background(), op); !errors.Is(err, port.ErrGeneratedFileOperationExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := operations.UpdateGeneratedFileOperation(context.Background(), op.ID, domain.GeneratedFileOpCompleted, "F123"); err != nil {
		t.Fatal(err)
	}
	got, err := operations.GetGeneratedFileOperation(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.GeneratedFileOpCompleted || got.SlackFileID != "F123" || got.ContentSHA256 != "abc" || got.SizeBytes != 12 || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("persisted operation = %#v", got)
	}
}
