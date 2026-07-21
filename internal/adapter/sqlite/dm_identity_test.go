package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestEnsureDMIdentityModeStampsAndRejectsChanges(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	if err := store.EnsureDMIdentityMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureDMIdentityMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureDMIdentityMode(ctx, false); !errors.Is(err, ErrDMIdentityModeMismatch) {
		t.Fatalf("EnsureDMIdentityMode() error = %v, want mode mismatch", err)
	}
}

func TestEnsureThreadedDMIdentityRejectsUnstampedLegacyConversations(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	metadata := domain.ConversationMetadata{
		Key: "slack:T12345678:dm:D12345678", TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM,
		LastTS: "1700000000.000001",
	}
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "old", UserID: "U12345678", ExternalTS: "1700000000.000001", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureDMIdentityMode(ctx, true); !errors.Is(err, ErrDMIdentityModeMismatch) {
		t.Fatalf("EnsureDMIdentityMode() error = %v, want mode mismatch", err)
	}
}
