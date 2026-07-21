package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestStandardProgressLifecycleAndWaitingLookup(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	operation := domain.ProgressOperation{
		ID: "progress-1", ConversationKey: "slack:T12345678:dm:D12345678:thread:1700000000.000001",
		ChannelID: "D12345678", ThreadTS: "1700000000.000001", State: domain.ProgressWorking,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateProgress(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProgressPublished(ctx, operation.ID, "1700000001.000001"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProgressState(ctx, operation.ID, domain.ProgressWaitingConfirmation, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	waiting, err := store.FindWaitingProgress(ctx, operation.ConversationKey)
	if err != nil || waiting == nil || waiting.MessageTS != "1700000001.000001" {
		t.Fatalf("waiting=%#v err=%v", waiting, err)
	}
	recoverable, err := store.ListRecoverableProgress(ctx)
	if err != nil || len(recoverable) != 1 {
		t.Fatalf("recoverable=%#v err=%v", recoverable, err)
	}
	if err := store.SetProgressState(ctx, operation.ID, domain.ProgressCleared, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	recoverable, err = store.ListRecoverableProgress(ctx)
	if err != nil || len(recoverable) != 0 {
		t.Fatalf("terminal recoverable=%#v err=%v", recoverable, err)
	}
}

func TestSuggestedPromptsAreClaimedOncePerWorkspaceUser(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000000.000001")
	id, claimed, err := store.ClaimSuggestedPrompts(ctx, "T12345678", "U12345678", key, now)
	if err != nil || !claimed || id == "" {
		t.Fatalf("id=%q claimed=%v err=%v", id, claimed, err)
	}
	if _, claimed, err := store.ClaimSuggestedPrompts(ctx, "T12345678", "U12345678", key, now); err != nil || claimed {
		t.Fatalf("second claim=%v err=%v", claimed, err)
	}
	if err := store.MarkSuggestedPromptsPublished(ctx, id, "1700000001.000001", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalOperationPersistsOnlyIdentitySequenceAndDigest(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	now := time.Unix(1700000000, 0).UTC()
	operation := domain.IncrementalOperation{
		ID: "incremental-1", ConversationKey: "slack:T12345678:dm:D12345678:thread:1700000000.000001",
		ChannelID: "D12345678", ThreadTS: "1700000000.000001", RendererVersion: "standard_incremental_v1",
		Status: domain.IncrementalPrepared, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.PrepareIncremental(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkIncrementalCreated(ctx, operation.ID, "1700000001.000001", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceIncremental(ctx, operation.ID, domain.IncrementalUpdating, 2, "digest-only", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	unfinished, err := store.ListUnfinishedIncremental(ctx)
	if err != nil || len(unfinished) != 1 || unfinished[0].MessageTS != "1700000001.000001" || unfinished[0].Sequence != 2 || unfinished[0].PrefixDigest != "digest-only" {
		t.Fatalf("unfinished=%#v err=%v", unfinished, err)
	}
	if err := store.AdvanceIncremental(ctx, operation.ID, domain.IncrementalFinalized, 3, "final-digest", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	unfinished, err = store.ListUnfinishedIncremental(ctx)
	if err != nil || len(unfinished) != 0 {
		t.Fatalf("finalized unfinished=%#v err=%v", unfinished, err)
	}
}
