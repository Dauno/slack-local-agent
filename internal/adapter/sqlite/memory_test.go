package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestMemoryStore_ApplyPatchIsTransactionalIdempotentAndAddsEvidence(t *testing.T) {
	store, _ := newTestStore(t)
	patch := domain.MemoryPatch{
		ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "1700000000.000001", SourceAuthorID: "U12345678",
		Operations: []domain.MemoryOp{
			{Type: domain.MemoryOpCreateTopic, TopicSlug: "atomic", TopicTitle: "Atomic", Content: "first"},
			{Type: domain.MemoryOpCreateTopic, TopicSlug: "atomic", TopicTitle: "Duplicate", Content: "second"},
		},
	}
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1000}
	if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err == nil {
		t.Fatal("ApplyMemoryPatch() accepted a patch with a duplicate topic")
	}
	if topics, err := store.ListTopics(t.Context()); err != nil || len(topics) != 0 {
		t.Fatalf("failed patch left topics = %#v, err = %v", topics, err)
	}

	patch.Operations = patch.Operations[:1]
	applied, err := store.ApplyMemoryPatch(t.Context(), patch, limits)
	if err != nil || !applied {
		t.Fatalf("ApplyMemoryPatch() = %v, %v", applied, err)
	}
	applied, err = store.ApplyMemoryPatch(t.Context(), patch, limits)
	if err != nil || applied {
		t.Fatalf("replayed ApplyMemoryPatch() = %v, %v", applied, err)
	}
	topic, err := store.GetTopic(t.Context(), "atomic")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.GetEvidence(t.Context(), topic.ID)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("GetEvidence() = %#v, %v", evidence, err)
	}
	if evidence[0].SourceTS != patch.ExchangeTS || evidence[0].AuthorID != patch.SourceAuthorID {
		t.Fatalf("evidence provenance = %#v", evidence[0])
	}
}

func TestMemoryStore_LoadOutboxMessagesUsesExactExchangeAndRecoversLease(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "3"}
	now := time.Now().UTC()
	for _, message := range []domain.Message{
		{Role: domain.RoleUser, Content: "source user", UserID: "U12345678", ExternalTS: "1", CreatedAt: now},
		{Role: domain.RoleAssistant, Content: "source reply", ExternalTS: "2", CreatedAt: now.Add(time.Second)},
		{Role: domain.RoleUser, Content: "later unrelated", UserID: "U99999999", ExternalTS: "3", CreatedAt: now.Add(2 * time.Second)},
	} {
		if err := store.AppendMessage(ctx, metadata, message, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EnqueueOutboxItem(ctx, key, "2"); err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNextOutboxItem(ctx)
	if err != nil || item == nil {
		t.Fatalf("ClaimNextOutboxItem() = %#v, %v", item, err)
	}
	messages, err := store.LoadOutboxMessages(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "source user" || messages[1].Content != "source reply" {
		t.Fatalf("LoadOutboxMessages() = %#v", messages)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE memory_outbox SET lease_until = 0 WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimNextOutboxItem(ctx)
	if err != nil || reclaimed == nil || reclaimed.ID != item.ID || reclaimed.Attempts != 2 {
		t.Fatalf("expired lease was not reclaimed: %#v, %v", reclaimed, err)
	}
}

func TestMemoryStore_AssistantExchangeIsAtomicAndSurvivesRetention(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "2"}
	now := time.Now().UTC()
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "source user", UserID: "U12345678", ExternalTS: "1", CreatedAt: now}, 1); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "source reply", ExternalTS: "2", CreatedAt: now.Add(time.Second)}
	prepared, err := store.PrepareAssistantExchange(ctx, metadata, assistant, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.CorrelationID == "" {
		t.Fatal("prepared exchange has no durable correlation ID")
	}
	var persistedCorrelationID string
	if err := store.db.QueryRowContext(ctx, `SELECT correlation_id FROM memory_exchange_intents WHERE id = ?`, prepared.ID).Scan(&persistedCorrelationID); err != nil {
		t.Fatal(err)
	}
	if persistedCorrelationID != prepared.CorrelationID {
		t.Fatalf("persisted correlation = %q, want %q", persistedCorrelationID, prepared.CorrelationID)
	}
	if err := store.MarkAssistantExchangePublished(ctx, prepared.ID, assistant.ExternalTS); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(ctx, prepared.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(retained) != 1 || retained[0].Content != "source reply" {
		t.Fatalf("retained messages = %#v, %v", retained, err)
	}
	item, err := store.ClaimNextOutboxItem(ctx)
	if err != nil || item == nil {
		t.Fatalf("ClaimNextOutboxItem() = %#v, %v", item, err)
	}
	source, err := store.LoadOutboxMessages(ctx, item)
	if err != nil || len(source) != 2 || source[0].Content != "source user" || source[1].Content != "source reply" {
		t.Fatalf("outbox source = %#v, %v", source, err)
	}
}

func TestThreadedDMAssistantExchangePersistsRecoveryRoot(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	const rootTS = "1700000000.000001"
	metadata := domain.ConversationMetadata{
		Key:    "slack:T12345678:dm:D12345678:thread:" + rootTS,
		TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM,
		RootTS: rootTS, LastTS: rootTS,
	}
	now := time.Now().UTC()
	if err := store.AppendMessage(ctx, metadata, domain.Message{
		Role: domain.RoleUser, Content: "source", UserID: "U12345678",
		ExternalTS: rootTS, CreatedAt: now,
	}, 10); err != nil {
		t.Fatal(err)
	}
	prepared, err := store.PrepareAssistantExchange(ctx, metadata, domain.Message{
		Role: domain.RoleAssistant, Content: "response", CreatedAt: now.Add(time.Second),
	}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	var intentRoot, conversationRoot string
	if err := store.db.QueryRowContext(ctx, `SELECT root_ts FROM memory_exchange_intents WHERE id = ?`, prepared.ID).Scan(&intentRoot); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT root_ts FROM conversations WHERE conversation_key = ?`, metadata.Key).Scan(&conversationRoot); err != nil {
		t.Fatal(err)
	}
	if intentRoot != rootTS || conversationRoot != "" {
		t.Fatalf("roots = intent %q, conversation %q", intentRoot, conversationRoot)
	}
}

func TestMemoryStore_IneligibleAssistantExchangePersistsWithoutOutbox(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "2"}
	now := time.Now().UTC()
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "attachment text", UserID: "U12345678", ExternalTS: "1", CreatedAt: now}, 10); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "attachment answer", CreatedAt: now.Add(time.Second)}
	prepared, err := store.PrepareAssistantExchange(ctx, metadata, assistant, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAssistantExchangePublished(ctx, prepared.ID, "2"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(ctx, prepared.ID); err != nil {
		t.Fatal(err)
	}

	messages, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(messages) != 2 || messages[1].Content != "attachment answer" {
		t.Fatalf("persisted messages = %#v, %v", messages, err)
	}
	var outboxCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_outbox`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 0 {
		t.Fatalf("ineligible exchange created %d outbox rows", outboxCount)
	}
}

func TestMemoryStore_AssistantExchangeRollsBackWhenEnqueueTransactionFails(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "source", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	conflicting := metadata
	conflicting.ChannelID = "D99999999" // Existing canonical key makes the transaction fail.
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "reply", ExternalTS: "2", CreatedAt: time.Now().UTC()}
	prepared, err := store.PrepareAssistantExchange(ctx, conflicting, assistant, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAssistantExchangePublished(ctx, prepared.ID, assistant.ExternalTS); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(ctx, prepared.ID); err == nil {
		t.Fatal("FinalizeAssistantExchange() accepted conflicting conversation metadata")
	}
	messages, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(messages) != 1 || messages[0].Role != domain.RoleUser {
		t.Fatalf("failed atomic exchange persisted messages = %#v, %v", messages, err)
	}
	var outboxCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_outbox`).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("failed atomic exchange enqueued %d items, err = %v", outboxCount, err)
	}
}

func TestMemoryStore_DoesNotFinalizePreparedExchangeBeforePublish(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "remember this", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "published reply", CreatedAt: time.Now().UTC()}
	prepared, err := store.PrepareAssistantExchange(ctx, metadata, assistant, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileAssistantExchanges(ctx, nil); err != nil {
		t.Fatal(err)
	}
	messages, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(messages) != 1 || messages[0].Content != "remember this" {
		t.Fatalf("prepared exchange persisted messages = %#v, %v", messages, err)
	}
	if item, err := store.ClaimNextOutboxItem(ctx); err != nil || item != nil {
		t.Fatalf("prepared exchange enqueued outbox item = %#v, %v", item, err)
	}
	if err := store.FinalizeAssistantExchange(ctx, prepared.ID); err == nil {
		t.Fatal("FinalizeAssistantExchange() accepted a prepared intent")
	}
}

func TestMemoryStore_ReconcilesPublishedExchangeAndPreservesSlackTimestamp(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "remember this", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	prepared, err := store.PrepareAssistantExchange(ctx, metadata, domain.Message{Role: domain.RoleAssistant, Content: "published reply", CreatedAt: time.Now().UTC()}, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	const slackTS = "1700000002.000003"
	if err := store.MarkAssistantExchangePublished(ctx, prepared.ID, slackTS); err != nil {
		t.Fatal(err)
	}
	// Model a crash after remote publication was recorded but before finalization.
	if err := store.ReconcileAssistantExchanges(ctx, nil); err != nil {
		t.Fatal(err)
	}
	messages, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(messages) != 2 || messages[1].ExternalTS != slackTS {
		t.Fatalf("reconciled messages = %#v, %v", messages, err)
	}
	item, err := store.ClaimNextOutboxItem(ctx)
	if err != nil || item == nil || item.ExchangeTS != slackTS {
		t.Fatalf("reconciled outbox item = %#v, %v", item, err)
	}
}

func TestMemoryStore_RecoversRemotePublishBeforeLocalFinalization(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "remember this", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	prepared, err := store.PrepareAssistantExchange(ctx, metadata, domain.Message{Role: domain.RoleAssistant, Content: "published reply", CreatedAt: time.Now().UTC()}, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	const slackTS = "1700000002.000003"
	// Model a process crash after Slack accepted the reply but before it could be marked locally.
	finder := exchangeFinder{intentID: prepared.ID, correlationID: prepared.CorrelationID, content: "published reply", timestamp: slackTS}
	if err := store.ReconcileAssistantExchanges(ctx, finder); err != nil {
		t.Fatal(err)
	}
	messages, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(messages) != 2 || messages[1].ExternalTS != slackTS {
		t.Fatalf("recovered messages = %#v, %v", messages, err)
	}
}

func TestMemoryStore_LeavesPreparedExchangesUnresolvedWhenTheyShareRemoteTimestamp(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "remember this", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.PrepareAssistantExchange(ctx, metadata, domain.Message{Role: domain.RoleAssistant, Content: "same reply", CreatedAt: time.Now().UTC()}, 10, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReconcileAssistantExchanges(ctx, exchangeFinder{content: "same reply", timestamp: "1700000002.000003"}); err != nil {
		t.Fatal(err)
	}
	messages, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("ambiguous recovery persisted messages = %#v, %v", messages, err)
	}
	var prepared int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_exchange_intents WHERE publish_status = 'prepared'`).Scan(&prepared); err != nil || prepared != 2 {
		t.Fatalf("unresolved prepared intents = %d, %v", prepared, err)
	}
}

type exchangeFinder struct {
	intentID         string
	correlationID    string
	content          string
	presentationJSON string
	timestamp        string
}

func (f exchangeFinder) FindPublishedAssistantExchange(_ context.Context, intent port.AssistantExchangeIntent) (string, bool, error) {
	if (f.intentID != "" && intent.ID != f.intentID) || intent.Content != f.content || intent.CorrelationID != f.correlationID ||
		(f.presentationJSON != "" && intent.PresentationJSON != f.presentationJSON) {
		return "", false, nil
	}
	return f.timestamp, true, nil
}

func TestMemoryStore_RecoversStructuredExchangeWithPersistedPresentation(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(ctx, metadata, domain.Message{Role: domain.RoleUser, Content: "show data", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	const presentationJSON = `{"FallbackMarkdown":"table fallback","Sources":null,"Table":{"Caption":"","Headers":["A"],"Rows":[["1"]],"RowHeader":0}}`
	prepared, err := store.PrepareStructuredAssistantExchange(ctx, metadata, domain.Message{Role: domain.RoleAssistant, Content: "table fallback", CreatedAt: time.Now().UTC()}, presentationJSON, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	const slackTS = "1700000002.000003"
	finder := exchangeFinder{
		intentID: prepared.ID, correlationID: prepared.CorrelationID, content: "table fallback",
		presentationJSON: presentationJSON, timestamp: slackTS,
	}
	if err := store.ReconcileAssistantExchanges(ctx, finder); err != nil {
		t.Fatal(err)
	}
	messages, err := store.RecentMessages(ctx, key, 10)
	if err != nil || len(messages) != 2 || messages[1].ExternalTS != slackTS || messages[1].Content != "table fallback" {
		t.Fatalf("recovered structured messages = %#v, %v", messages, err)
	}
}

func TestMemoryStore_LinkUsesCurrentRevisionProvenance(t *testing.T) {
	store, _ := newTestStore(t)
	a, _ := store.CreateTopic(t.Context(), "alpha-link", "Alpha", "", nil, "alpha", "")
	_, _ = store.CreateTopic(t.Context(), "beta-link", "Beta", "", nil, "beta", "")
	patch := domain.MemoryPatch{ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "4", Operations: []domain.MemoryOp{{Type: domain.MemoryOpLinkAdd, TopicSlug: "alpha-link", TargetTopicSlug: "beta-link", LinkRelation: "related", ExpectedRev: 1}}}
	if _, err := store.ApplyMemoryPatch(t.Context(), patch, domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 100}); err != nil {
		t.Fatal(err)
	}
	_, revision, err := store.GetTopicByID(t.Context(), a.ID)
	if err != nil || revision == nil {
		t.Fatalf("GetTopicByID() = %#v, %v", revision, err)
	}
	links, err := store.GetTopicLinks(t.Context(), a.ID)
	if err != nil || len(links) != 1 || links[0].RevisionID != revision.ID {
		t.Fatalf("link provenance = %#v, %v", links, err)
	}
}

func TestMemoryStore_CreateAndGetTopic(t *testing.T) {
	store, _ := newTestStore(t)

	topic, err := store.CreateTopic(t.Context(), "project-alpha", "Project Alpha",
		"A test project", []string{"alpha", "test"},
		"# Project Alpha\n\nThis is curated knowledge.", "initial creation")
	if err != nil {
		t.Fatalf("CreateTopic() error = %v", err)
	}
	if topic.ID == "" {
		t.Fatal("CreateTopic() returned empty ID")
	}
	if topic.Slug != "project-alpha" {
		t.Fatalf("CreateTopic() slug = %q", topic.Slug)
	}
	if topic.CurrentRev != 1 {
		t.Fatalf("CreateTopic() current_rev = %d", topic.CurrentRev)
	}

	got, err := store.GetTopic(t.Context(), "project-alpha")
	if err != nil {
		t.Fatalf("GetTopic() error = %v", err)
	}
	if got.ID != topic.ID {
		t.Fatalf("GetTopic() ID = %q, want %q", got.ID, topic.ID)
	}
	if got.Title != "Project Alpha" {
		t.Fatalf("GetTopic() title = %q", got.Title)
	}

	gotByID, rev, err := store.GetTopicByID(t.Context(), topic.ID)
	if err != nil {
		t.Fatalf("GetTopicByID() error = %v", err)
	}
	if gotByID.Slug != "project-alpha" {
		t.Fatalf("GetTopicByID() slug = %q", gotByID.Slug)
	}
	if rev == nil {
		t.Fatal("GetTopicByID() returned nil revision")
	}
	if rev.Content != "# Project Alpha\n\nThis is curated knowledge." {
		t.Fatalf("GetTopicByID() content = %q", rev.Content)
	}
}

func TestMemoryStore_DuplicateSlug(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.CreateTopic(t.Context(), "test", "Test", "desc", nil, "content", "init")
	if err != nil {
		t.Fatalf("first CreateTopic() error = %v", err)
	}
	_, err = store.CreateTopic(t.Context(), "test", "Test 2", "desc 2", nil, "content", "init")
	if err == nil {
		t.Fatal("CreateTopic() expected duplicate error")
	}
}

func TestMemoryStore_AddRevisionAndFTS(t *testing.T) {
	store, _ := newTestStore(t)

	topic, err := store.CreateTopic(t.Context(), "tools", "Tools", "Dev tools", nil, "We use Go.", "init")
	if err != nil {
		t.Fatalf("CreateTopic() error = %v", err)
	}

	rev, err := store.AddRevision(t.Context(), topic.ID, 1, "We use Go and Rust.", "added Rust")
	if err != nil {
		t.Fatalf("AddRevision() error = %v", err)
	}
	if rev.RevisionNumber != 2 {
		t.Fatalf("AddRevision() rev = %d", rev.RevisionNumber)
	}

	// Search should find the updated content
	snippets, err := store.SearchTopics(t.Context(), "Rust", 5, 5000)
	if err != nil {
		t.Fatalf("SearchTopics() error = %v", err)
	}
	if len(snippets) != 1 {
		t.Fatalf("SearchTopics() got %d results", len(snippets))
	}
	if snippets[0].Slug != "tools" {
		t.Fatalf("SearchTopics() slug = %q", snippets[0].Slug)
	}

	// Search should NOT find old content
	snippets, err = store.SearchTopics(t.Context(), "We use Go", 5, 5000)
	if err != nil {
		t.Fatalf("SearchTopics() error = %v", err)
	}
	if len(snippets) != 1 {
		t.Fatalf("SearchTopics() got %d results, expected 1 (old content also indexed via revision)", len(snippets))
	}
}

func TestMemoryStore_StaleRevision(t *testing.T) {
	store, _ := newTestStore(t)

	topic, err := store.CreateTopic(t.Context(), "stale", "Stale", "desc", nil, "content", "init")
	if err != nil {
		t.Fatalf("CreateTopic() error = %v", err)
	}

	_, err = store.AddRevision(t.Context(), topic.ID, 2, "new content", "update")
	if err == nil {
		t.Fatal("AddRevision() expected stale revision error")
	}
}

func TestMemoryStore_ListTopics(t *testing.T) {
	store, _ := newTestStore(t)

	_, _ = store.CreateTopic(t.Context(), "a", "A", "desc", nil, "content a", "init")
	_, _ = store.CreateTopic(t.Context(), "b", "B", "desc", nil, "content b", "init")

	topics, err := store.ListTopics(t.Context())
	if err != nil {
		t.Fatalf("ListTopics() error = %v", err)
	}
	if len(topics) != 2 {
		t.Fatalf("ListTopics() got %d topics", len(topics))
	}
}

func TestMemoryStore_DeleteTopic(t *testing.T) {
	store, _ := newTestStore(t)

	topic, err := store.CreateTopic(t.Context(), "del", "Delete", "desc", nil, "content", "init")
	if err != nil {
		t.Fatalf("CreateTopic() error = %v", err)
	}

	if err := store.DeleteTopic(t.Context(), topic.ID); err != nil {
		t.Fatalf("DeleteTopic() error = %v", err)
	}

	_, err = store.GetTopic(t.Context(), "del")
	if err == nil {
		t.Fatal("GetTopic() expected not found after delete")
	}
}

func TestMemoryStore_TopicLinks(t *testing.T) {
	store, _ := newTestStore(t)

	a, _ := store.CreateTopic(t.Context(), "alpha", "Alpha", "desc", nil, "content", "init")
	b, _ := store.CreateTopic(t.Context(), "beta", "Beta", "desc", nil, "content", "init")

	if err := store.AddTopicLink(t.Context(), a.ID, b.ID, "related_to", 1); err != nil {
		t.Fatalf("AddTopicLink() error = %v", err)
	}

	links, err := store.GetTopicLinks(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("GetTopicLinks() error = %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("GetTopicLinks() got %d links", len(links))
	}
	if links[0].Relation != "related_to" {
		t.Fatalf("GetTopicLinks() relation = %q", links[0].Relation)
	}

	if err := store.RemoveTopicLink(t.Context(), a.ID, b.ID); err != nil {
		t.Fatalf("RemoveTopicLink() error = %v", err)
	}

	links, err = store.GetTopicLinks(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("GetTopicLinks() after remove error = %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("GetTopicLinks() expected empty after remove")
	}
}

func TestMemoryStore_OutboxLifecycle(t *testing.T) {
	store, _ := newTestStore(t)

	if err := store.EnqueueOutboxItem(t.Context(), "slack:T12345678:dm:D12345678", "1700000000.000001"); err != nil {
		t.Fatalf("EnqueueOutboxItem() error = %v", err)
	}
	if err := store.EnqueueOutboxItem(t.Context(), "slack:T12345678:dm:D87654321", "1700000001.000002"); err != nil {
		t.Fatalf("EnqueueOutboxItem() error = %v", err)
	}

	item, err := store.ClaimNextOutboxItem(t.Context())
	if err != nil {
		t.Fatalf("ClaimNextOutboxItem() error = %v", err)
	}
	if item == nil {
		t.Fatal("ClaimNextOutboxItem() returned nil")
	}
	if item.Status != domain.OutboxStatusProcessing {
		t.Fatalf("ClaimNextOutboxItem() status = %q", item.Status)
	}

	if err := store.CompleteOutboxItem(t.Context(), item.ID, item.LeaseUntil); err != nil {
		t.Fatalf("CompleteOutboxItem() error = %v", err)
	}

	// Second item should still be claimable
	item2, err := store.ClaimNextOutboxItem(t.Context())
	if err != nil {
		t.Fatalf("ClaimNextOutboxItem() error = %v", err)
	}
	if item2 == nil {
		t.Fatal("ClaimNextOutboxItem() returned nil for second item")
	}
	if item2.ID == item.ID {
		t.Fatal("ClaimNextOutboxItem() returned same completed item")
	}

	if err := store.CompleteOutboxItem(t.Context(), item2.ID, item2.LeaseUntil); err != nil {
		t.Fatalf("CompleteOutboxItem() error = %v", err)
	}

	// No more items
	item3, err := store.ClaimNextOutboxItem(t.Context())
	if err != nil {
		t.Fatalf("ClaimNextOutboxItem() error = %v", err)
	}
	if item3 != nil {
		t.Fatal("ClaimNextOutboxItem() expected nil for empty outbox")
	}
}

func TestMemoryStore_OutboxRetry(t *testing.T) {
	store, _ := newTestStore(t)

	if err := store.EnqueueOutboxItem(t.Context(), "slack:T12345678:dm:D12345678", "1700000000.000001"); err != nil {
		t.Fatalf("EnqueueOutboxItem() error = %v", err)
	}

	item, _ := store.ClaimNextOutboxItem(t.Context())
	if err := store.FailOutboxItem(t.Context(), item.ID, item.LeaseUntil, "test failure"); err != nil {
		t.Fatalf("FailOutboxItem() error = %v", err)
	}

	// Failed item should not be claimable (next_attempt is in the future)
	next, err := store.ClaimNextOutboxItem(t.Context())
	if err != nil {
		t.Fatalf("ClaimNextOutboxItem() error = %v", err)
	}
	if next != nil {
		t.Fatal("ClaimNextOutboxItem() expected nil for failed item past retry window")
	}
}

func TestMemoryStore_Evidence(t *testing.T) {
	store, _ := newTestStore(t)

	topic, _ := store.CreateTopic(t.Context(), "evidence", "Evidence", "desc", nil, "content", "init")

	_, rev, _ := store.GetTopicByID(t.Context(), topic.ID)
	revID := rev.ID

	id, err := store.AddEvidence(t.Context(), revID, "slack:T12345678:dm:D12345678", "1700000000.000001", "U12345678", domain.EvidenceSource)
	if err != nil {
		t.Fatalf("AddEvidence() error = %v", err)
	}
	if id == 0 {
		t.Fatal("AddEvidence() returned 0")
	}

	evidence, err := store.GetEvidence(t.Context(), topic.ID)
	if err != nil {
		t.Fatalf("GetEvidence() error = %v", err)
	}
	if len(evidence) != 1 {
		t.Fatalf("GetEvidence() got %d records", len(evidence))
	}
	if evidence[0].Type != domain.EvidenceSource {
		t.Fatalf("GetEvidence() type = %q", evidence[0].Type)
	}
}

func TestMemoryStore_FindSimilarTopic(t *testing.T) {
	store, _ := newTestStore(t)

	_, _ = store.CreateTopic(t.Context(), "project", "Project Alpha", "desc", nil, "content", "init")

	found, err := store.FindSimilarTopic(t.Context(), "Project Alpha")
	if err != nil {
		t.Fatalf("FindSimilarTopic() error = %v", err)
	}
	if found == nil {
		t.Fatal("FindSimilarTopic() returned nil")
	}
	if found.Slug != "project" {
		t.Fatalf("FindSimilarTopic() slug = %q", found.Slug)
	}

	notFound, err := store.FindSimilarTopic(t.Context(), "Non-existent")
	if err != nil {
		t.Fatalf("FindSimilarTopic() error = %v", err)
	}
	if notFound != nil {
		t.Fatal("FindSimilarTopic() expected nil for unknown title")
	}
}

func TestMemoryStore_TopicExistsBySlug(t *testing.T) {
	store, _ := newTestStore(t)

	_, _ = store.CreateTopic(t.Context(), "exists", "Exists", "desc", nil, "content", "init")

	exists, err := store.TopicExistsBySlug(t.Context(), "exists")
	if err != nil {
		t.Fatalf("TopicExistsBySlug() error = %v", err)
	}
	if !exists {
		t.Fatal("TopicExistsBySlug() returned false for existing topic")
	}

	exists, err = store.TopicExistsBySlug(t.Context(), "not-exists")
	if err != nil {
		t.Fatalf("TopicExistsBySlug() error = %v", err)
	}
	if exists {
		t.Fatal("TopicExistsBySlug() returned true for non-existing topic")
	}
}

func TestMemoryStore_SearchTopicsUnicode(t *testing.T) {
	store, _ := newTestStore(t)

	_, _ = store.CreateTopic(t.Context(), "unicode", "Unicode", "desc",
		nil, "Contenido con ñoños y emojis 🚀", "init")

	snippets, err := store.SearchTopics(t.Context(), "ñoños", 5, 5000)
	if err != nil {
		t.Fatalf("SearchTopics() error = %v", err)
	}
	if len(snippets) != 1 {
		t.Fatalf("SearchTopics() got %d results for unicode query", len(snippets))
	}
}

func TestMemoryStore_SearchFTSHandlesSlackPunctuation(t *testing.T) {
	store, _ := newTestStore(t)
	_, err := store.CreateTopic(t.Context(), "creator", "Creator", "", nil,
		"Quien es tu creador? El operador OR se trata como texto.", "init")
	if err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{
		"quien es tu creador ?",
		"¿quien es tu creador?",
		`"quien es tu creador"`,
		"quien OR es tu creador",
	} {
		snippets, err := store.SearchTopics(t.Context(), query, 5, 5000)
		if err != nil {
			t.Fatalf("SearchTopics(%q) error = %v", query, err)
		}
		if len(snippets) != 1 || snippets[0].Slug != "creator" {
			t.Fatalf("SearchTopics(%q) = %#v", query, snippets)
		}
	}

	references, err := store.SearchTopicReferences(t.Context(), "quien es tu creador ?", 5)
	if err != nil {
		t.Fatalf("SearchTopicReferences() error = %v", err)
	}
	if len(references) != 1 || references[0].Slug != "creator" {
		t.Fatalf("SearchTopicReferences() = %#v", references)
	}
}

func TestMemoryStore_SearchFTSMatchesNaturalLanguageEntityQuestions(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.CreateTopic(t.Context(), "person-dauno", "Dauno", "", nil, "Dauno se identifica como creador de local-agent.", "init"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"¿Quién es Dauno?", "Who is Dauno?", `Dauno OR "*"`} {
		snippets, err := store.SearchTopics(t.Context(), query, 1, 500)
		if err != nil {
			t.Fatalf("SearchTopics(%q) error = %v", query, err)
		}
		if len(snippets) != 1 || snippets[0].Slug != "person-dauno" {
			t.Fatalf("SearchTopics(%q) = %#v", query, snippets)
		}
	}
}

func TestMemoryStore_SearchPersonTopicsByOwnerScopesOwnership(t *testing.T) {
	store, _ := newTestStore(t)
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1_000}
	patches := []domain.MemoryPatch{
		{
			ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "1", SourceAuthorID: "U00000001",
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "dauno", TopicTitle: "Dauno", BundlePath: "people", Content: "Dauno is the creator."}},
		},
		{
			ConversationKey: "slack:T12345678:dm:D87654321", ExchangeTS: "2", SourceAuthorID: "U00000002",
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "other", TopicTitle: "Other", BundlePath: "people", Content: "Other is a user."}},
		},
		{
			ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "3", SourceAuthorID: "U00000001",
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "project", TopicTitle: "Project", BundlePath: "projects", Content: "Dauno maintains the project."}},
		},
	}
	for _, patch := range patches {
		if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err != nil {
			t.Fatal(err)
		}
	}

	ownerKey := domain.SlackOwnerKey("slack:T12345678:dm:D12345678", "U00000001")
	snippets, err := store.SearchPersonTopicsByOwner(t.Context(), ownerKey, 5, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(snippets) != 1 || snippets[0].Slug != domain.ScopedPersonTopicSlug("dauno", ownerKey) {
		t.Fatalf("SearchPersonTopicsByOwner() = %#v", snippets)
	}
}

func TestMemoryStore_ScopesSameNamePeopleByOwnerAndRejectsCrossOwnerRevision(t *testing.T) {
	store, _ := newTestStore(t)
	const key = domain.ConversationKey("slack:T12345678:dm:D12345678")
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1_000}
	owners := []struct {
		userID  string
		content string
	}{
		{userID: "U00000001", content: "Dauno owns the first identity."},
		{userID: "U00000002", content: "Dauno owns the second identity."},
	}
	for index, owner := range owners {
		patch := domain.MemoryPatch{
			ConversationKey: key, ExchangeTS: fmt.Sprintf("%d", index+1), SourceAuthorID: owner.userID,
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "person-dauno", TopicTitle: "Dauno", BundlePath: "people", Content: owner.content}},
		}
		if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err != nil {
			t.Fatal(err)
		}
	}

	firstOwner := domain.SlackOwnerKey(key, owners[0].userID)
	secondOwner := domain.SlackOwnerKey(key, owners[1].userID)
	firstSlug := domain.ScopedPersonTopicSlug("person-dauno", firstOwner)
	secondSlug := domain.ScopedPersonTopicSlug("person-dauno", secondOwner)
	if firstSlug == secondSlug {
		t.Fatal("same-name owners produced the same scoped slug")
	}
	first, err := store.GetTopic(t.Context(), firstSlug)
	if err != nil || first.OwnerKey != firstOwner {
		t.Fatalf("first owned topic = %#v, %v", first, err)
	}
	second, err := store.GetTopic(t.Context(), secondSlug)
	if err != nil || second.OwnerKey != secondOwner {
		t.Fatalf("second owned topic = %#v, %v", second, err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE memory_topics SET owner_key = ? WHERE id = ?`, secondOwner, first.ID); err == nil || !strings.Contains(err.Error(), "owner is immutable") {
		t.Fatalf("owner mutation error = %v", err)
	}

	_, err = store.ApplyMemoryPatch(t.Context(), domain.MemoryPatch{
		ConversationKey: key, ExchangeTS: "3", SourceAuthorID: owners[1].userID,
		Operations: []domain.MemoryOp{{Type: domain.MemoryOpRevise, TopicSlug: firstSlug, ExpectedRev: 1, Content: "cross-owner overwrite"}},
	}, limits)
	if err == nil || !strings.Contains(err.Error(), "owned by another Slack user") {
		t.Fatalf("cross-owner revision error = %v", err)
	}
	first, err = store.GetTopic(t.Context(), firstSlug)
	if err != nil || first.Content != owners[0].content || first.CurrentRev != 1 {
		t.Fatalf("cross-owner revision changed first topic = %#v, %v", first, err)
	}

	for _, test := range []struct {
		owner string
		slug  string
	}{{firstOwner, firstSlug}, {secondOwner, secondSlug}} {
		snippets, err := store.SearchPersonTopicsByOwner(t.Context(), test.owner, 1, 1_000)
		if err != nil || len(snippets) != 1 || snippets[0].Slug != test.slug {
			t.Fatalf("SearchPersonTopicsByOwner(%q) = %#v, %v", test.owner, snippets, err)
		}
	}
	for _, test := range []struct {
		owner string
		slug  string
	}{{firstOwner, firstSlug}, {secondOwner, secondSlug}} {
		snippets, err := store.SearchTopicsForOwner(t.Context(), "Dauno", test.owner, 1, 1_000)
		if err != nil || len(snippets) != 1 || snippets[0].Slug != test.slug {
			t.Fatalf("SearchTopicsForOwner(%q) = %#v, %v", test.owner, snippets, err)
		}
	}
}

func TestMemoryStore_SearchPersonTopicsByOwnerSkipsOversizedCandidate(t *testing.T) {
	store, _ := newTestStore(t)
	const key = domain.ConversationKey("slack:T12345678:dm:D12345678")
	const userID = "U00000001"
	ownerKey := domain.SlackOwnerKey(key, userID)
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1_000}
	for _, patch := range []domain.MemoryPatch{
		{
			ConversationKey: key, ExchangeTS: "1", SourceAuthorID: userID,
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "large", TopicTitle: "Large", BundlePath: "people", Content: strings.Repeat("needle ", 100)}},
		},
		{
			ConversationKey: key, ExchangeTS: "2", SourceAuthorID: userID,
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "small", TopicTitle: "Small", BundlePath: "people", Content: "needle fits"}},
		},
	} {
		if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE memory_topics SET updated_at = CASE slug WHEN ? THEN 2 ELSE 1 END`, domain.ScopedPersonTopicSlug("large", ownerKey)); err != nil {
		t.Fatal(err)
	}

	snippets, err := store.SearchPersonTopicsByOwner(t.Context(), ownerKey, 1, len([]rune("needle fits")))
	if err != nil {
		t.Fatal(err)
	}
	if len(snippets) != 1 || snippets[0].Slug != domain.ScopedPersonTopicSlug("small", ownerKey) || snippets[0].Content != "needle fits" {
		t.Fatalf("SearchPersonTopicsByOwner() = %#v", snippets)
	}
}

func TestMemoryStore_SearchTopicsCharLimit(t *testing.T) {
	store, _ := newTestStore(t)

	longContent := ""
	for i := 0; i < 100; i++ {
		longContent += "Some repeating content with the keyword searchable. "
	}
	_, _ = store.CreateTopic(t.Context(), "big", "Big Topic", "desc", nil, longContent, "init")

	// Limit to very few chars
	snippets, err := store.SearchTopics(t.Context(), "searchable", 5, 50)
	if err != nil {
		t.Fatalf("SearchTopics() error = %v", err)
	}
	if len(snippets) != 1 || len([]rune(snippets[0].Content)) != 50 {
		t.Fatalf("SearchTopics() = %#v; want truncated result", snippets)
	}
}

func TestMemoryStore_SearchTopicsSkipsOversizedTopHitForLaterFit(t *testing.T) {
	store, _ := newTestStore(t)
	_, _ = store.CreateTopic(t.Context(), "oversized", "Oversized", "", nil, strings.Repeat("needle ", 100), "init")
	_, _ = store.CreateTopic(t.Context(), "small", "Small", "", nil, "needle fits", "init")

	snippets, err := store.SearchTopics(t.Context(), "needle", 1, len([]rune("needle fits")))
	if err != nil {
		t.Fatal(err)
	}
	if len(snippets) != 1 || snippets[0].Slug != "small" || snippets[0].Content != "needle fits" {
		t.Fatalf("SearchTopics() = %#v; want later fitting result", snippets)
	}
}

func TestMemoryStore_ApplyPatchStoresBundlePath(t *testing.T) {
	store, _ := newTestStore(t)
	patch := domain.MemoryPatch{
		ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "1", SourceAuthorID: "U12345678",
		Operations: []domain.MemoryOp{
			{Type: domain.MemoryOpCreateTopic, TopicSlug: "alpha", TopicTitle: "Alpha", Content: "content", BundlePath: "projects"},
		},
	}
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1000}
	if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err != nil {
		t.Fatal(err)
	}
	topic, err := store.GetTopic(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if topic.BundlePath != "projects" {
		t.Fatalf("BundlePath = %q, want projects", topic.BundlePath)
	}
}

func TestMemoryStore_ApplyPatchDefaultsInvalidBundlePathToTopics(t *testing.T) {
	store, _ := newTestStore(t)
	patch := domain.MemoryPatch{
		ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "1", SourceAuthorID: "U12345678",
		Operations: []domain.MemoryOp{
			{Type: domain.MemoryOpCreateTopic, TopicSlug: "alpha", TopicTitle: "Alpha", Content: "content", BundlePath: "/invalid//path"},
		},
	}
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1000}
	if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err != nil {
		t.Fatal(err)
	}
	topic, err := store.GetTopic(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if topic.BundlePath != "topics" {
		t.Fatalf("BundlePath = %q, want topics", topic.BundlePath)
	}
}

func TestMemoryStore_ReadProjectionSnapshotIncludesAllData(t *testing.T) {
	store, _ := newTestStore(t)
	patch := domain.MemoryPatch{
		ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "1", SourceAuthorID: "U12345678",
		Operations: []domain.MemoryOp{
			{Type: domain.MemoryOpCreateTopic, TopicSlug: "a", TopicTitle: "A", Content: "content a", BundlePath: "facts"},
			{Type: domain.MemoryOpCreateTopic, TopicSlug: "b", TopicTitle: "B", Content: "content b", BundlePath: "people"},
		},
	}
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1000}
	if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadProjectionSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Topics) != 2 {
		t.Fatalf("topics = %d", len(snapshot.Topics))
	}
	for _, topic := range snapshot.Topics {
		if len(snapshot.Revisions[topic.ID]) == 0 {
			t.Fatalf("no revisions for topic %s", topic.Slug)
		}
		if len(snapshot.Evidence[topic.ID]) == 0 {
			t.Fatalf("no evidence for topic %s", topic.Slug)
		}
	}
}
