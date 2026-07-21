package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestHistoryReaderUsesRepliesForChannelThreadAndMapsRoles(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "pregunta", Timestamp: "1720000000.000001"}},
		{Msg: slackapi.Msg{User: testBot, Text: "respuesta", Timestamp: "1720000001.000002", SubType: slackapi.MsgSubTypeBotMessage}},
		{Msg: slackapi.Msg{User: "U00000003", Text: "seguimiento", Timestamp: "1720000002.000003"}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	invocation := validThreadInvocation()

	got, err := reader.RecentHistory(context.Background(), invocation, domain.ContextLimits{MaxMessages: 3, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if !got.BotParticipated {
		t.Fatal("BotParticipated = false")
	}
	if len(got.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(got.Messages))
	}
	roles := []domain.Role{domain.RoleUser, domain.RoleAssistant, domain.RoleUser}
	for index, role := range roles {
		if got.Messages[index].Role != role {
			t.Fatalf("message %d role = %q, want %q", index, got.Messages[index].Role, role)
		}
	}
	if got.Messages[1].CreatedAt.IsZero() || got.Messages[1].CreatedAt.Nanosecond() != 2000 {
		t.Fatalf("assistant CreatedAt = %v", got.Messages[1].CreatedAt)
	}
	call := client.lastCall()
	if call.method != "replies" || call.channelID != testChannel || call.rootTS != testThread || call.latest != testTS || call.limit != 3 {
		t.Fatalf("history call = %#v", call)
	}
	if !call.hadDeadline {
		t.Fatal("history API call had no timeout deadline")
	}
}

func TestHistoryReaderUsesInvocationAsRootForNewChannelThread(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	invocation := validThreadInvocation()
	invocation.Trigger = domain.TriggerMention
	invocation.ThreadTS = ""

	if _, err := reader.RecentHistory(context.Background(), invocation, domain.ContextLimits{MaxMessages: 5, MaxChars: 100}); err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if call := client.lastCall(); call.method != "replies" || call.rootTS != testTS {
		t.Fatalf("history call = %#v", call)
	}
}

func TestHistoryReaderUsesDMHistoryAndRestoresChronologicalOrder(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "latest", Timestamp: "1720000003.000003"}},
		{Msg: slackapi.Msg{User: testBot, Text: "middle", Timestamp: "1720000002.000002"}},
		{Msg: slackapi.Msg{User: testUser, Text: "oldest", Timestamp: "1720000001.000001"}},
	}}
	reader := newHistoryReader(client, testBot, 0, nil, true)
	invocation := validDMInvocation()

	got, err := reader.RecentHistory(context.Background(), invocation, domain.ContextLimits{MaxMessages: 3, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	want := []string{"oldest", "middle", "latest"}
	for index := range want {
		if got.Messages[index].Content != want[index] {
			t.Fatalf("message %d = %q, want %q", index, got.Messages[index].Content, want[index])
		}
	}
	call := client.lastCall()
	if call.method != "history" || call.channelID != testDM || call.latest != testTS || call.limit != 3 || call.hadDeadline {
		t.Fatalf("history call = %#v", call)
	}
}

func TestHistoryReaderUsesRepliesForThreadedDM(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "root", Timestamp: testThread}},
		{Msg: slackapi.Msg{User: testBot, Text: "answer", Timestamp: testTS}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	invocation := validDMInvocation()
	invocation.ThreadedDM = true
	invocation.ThreadTS = testThread

	got, err := reader.RecentHistory(context.Background(), invocation, domain.ContextLimits{MaxMessages: 3, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "root" || got.Messages[1].Content != "answer" {
		t.Fatalf("thread history = %#v", got.Messages)
	}
	call := client.lastCall()
	if call.method != "replies" || call.channelID != testDM || call.rootTS != testThread || call.latest != testTS {
		t.Fatalf("history call = %#v", call)
	}
}

func TestHistoryReaderRecoversThreadedDMExchangeFromReplies(t *testing.T) {
	t.Parallel()
	const content = "threaded response"
	parts := renderMarkdownV1(content, true)
	client := &fakeHistoryClient{replies: []slackapi.Message{{Msg: slackapi.Msg{
		User: testBot, Timestamp: "1720000002.000002",
		Metadata: exchangeMetadataFor("threaded-correlation", markdownRenderMode, 1, 1, contentSHA256(parts[0])),
	}}}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)

	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, RootTS: testThread,
		Content: content, CorrelationID: "threaded-correlation",
	})
	if err != nil || !found || timestamp != "1720000002.000002" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
	if call := client.lastCall(); call.method != "replies" || call.rootTS != testThread {
		t.Fatalf("recovery call = %#v", call)
	}
}

func TestHistoryReaderEnforcesMessageAndCharacterLimitsDefensively(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "discard-old", Timestamp: "1.000001"}},
		{Msg: slackapi.Msg{User: testUser, Text: "discard-too", Timestamp: "2.000001"}},
		{Msg: slackapi.Msg{User: testBot, Text: "1234", Timestamp: "3.000001"}},
		{Msg: slackapi.Msg{User: testUser, Text: "abcdefgh", Timestamp: "4.000001"}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 2, MaxChars: 7})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "abcdefg" {
		t.Fatalf("limited messages = %#v", got.Messages)
	}
	if !got.BotParticipated {
		t.Fatal("bot participation from bounded API messages was lost after character limiting")
	}
}

func TestHistoryReaderFiltersUnsupportedHistoryMessages(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "", Timestamp: "1.1"}},
		{Msg: slackapi.Msg{User: "", Text: "system", Timestamp: "1.2"}},
		{Msg: slackapi.Msg{User: testUser, Text: "edited", Timestamp: "1.3", Edited: &slackapi.Edited{}}},
		{Msg: slackapi.Msg{User: testUser, Text: "file", Timestamp: "1.4", Files: []slackapi.File{{ID: "F"}}}},
		{Msg: slackapi.Msg{User: testUser, Text: "join", Timestamp: "1.5", SubType: slackapi.MsgSubTypeChannelJoin}},
		{Msg: slackapi.Msg{User: testBot, Text: "bot", Timestamp: "1.6", SubType: slackapi.MsgSubTypeBotMessage}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 10, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != domain.RoleAssistant || got.Messages[0].Content != "bot" || !got.BotParticipated {
		t.Fatalf("filtered history = %#v", got)
	}
}

func TestHistoryReaderReturnsRedactedWrappedAPIErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("request failed with xoxb-123456789-secret")
	client := &fakeHistoryClient{err: wantErr}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)

	_, err := reader.RecentHistory(context.Background(), validDMInvocation(), domain.ContextLimits{MaxMessages: 3, MaxChars: 100})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RecentHistory() error = %v, want wrapped API error", err)
	}
	if strings.Contains(err.Error(), "123456789-secret") {
		t.Fatalf("RecentHistory() leaked token: %v", err)
	}
}

func exchangeMetadataFor(correlationID, renderMode string, partIndex, partCount int, digest string) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{
		EventType: assistantMetadataEventType,
		EventPayload: map[string]any{
			"correlation_id": correlationID,
			"render_mode":    renderMode,
			"part_index":     float64(partIndex),
			"part_count":     float64(partCount),
			"content_sha256": digest,
		},
	}
}

func TestHistoryReaderFindsPublishedStructuredExchangeFromCanonicalPresentation(t *testing.T) {
	presentation := domain.Presentation{
		FallbackMarkdown: "Large table",
		Table:            &domain.Table{Headers: []string{"Item"}, Rows: make([][]string, 120)},
	}
	for index := range presentation.Table.Rows {
		presentation.Table.Rows[index] = []string{fmt.Sprintf("item-%d", index)}
	}
	encoded, err := json.Marshal(presentation)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := renderPresentationParts(presentation, contentSHA256(string(encoded))[:16])
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]slackapi.Message, len(parts))
	for index := range parts {
		messages[index] = slackapi.Message{Msg: slackapi.Msg{
			User: testBot, Timestamp: fmt.Sprintf("172000000%d.000001", index+1),
			Metadata: exchangeMetadataFor("intent-correlation", blocksV1RenderMode, index+1, len(parts), structuredPartDigest(encoded, index+1)),
		}}
	}
	reader := newHistoryReader(&fakeHistoryClient{history: messages}, testBot, time.Second, nil, true)

	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: presentation.FallbackMarkdown,
		CorrelationID: "intent-correlation", PresentationJSON: string(encoded),
	})
	if err != nil || !found || timestamp != "1720000002.000001" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRejectsStructuredExchangeWithMarkdownMetadata(t *testing.T) {
	presentation := domain.Presentation{FallbackMarkdown: "Source", Sources: []domain.Source{{Text: "Docs", URL: "https://example.com"}}}
	encoded, err := json.Marshal(presentation)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeHistoryClient{history: []slackapi.Message{{Msg: slackapi.Msg{
		User: testBot, Timestamp: "1720000001.000001",
		Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, structuredPartDigest(encoded, 1)),
	}}}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)

	_, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: presentation.FallbackMarkdown,
		CorrelationID: "intent-correlation", PresentationJSON: string(encoded),
	})
	if err != nil || found {
		t.Fatalf("FindPublishedAssistantExchange() found wrong renderer metadata: found=%t err=%v", found, err)
	}
}

func TestHistoryReaderFindsPublishedAssistantExchangeByMetadataDigest(t *testing.T) {
	t.Parallel()
	chunks := SplitMarkdown("published reply", SlackMarkdownChunkRunes, true)
	digest := contentSHA256(chunks[0])
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: "older reply", Timestamp: "1719999999.000001"}},
		{Msg: slackapi.Msg{User: testBot, Text: "translated by slack", Timestamp: "1720000001.000002", Metadata: slackapi.SlackMetadata{
			EventType: assistantMetadataEventType,
			EventPayload: map[string]any{
				"correlation_id": "intent-correlation",
				"render_mode":    "markdown_v1",
				"part_index":     float64(1),
				"part_count":     float64(1),
				"content_sha256": digest,
			},
		}}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	intent := port.AssistantExchangeIntent{
		ID: "intent", ChannelID: testDM, ChannelKind: domain.ChannelDM,
		Content: "published reply", CorrelationID: "intent-correlation",
	}
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), intent)
	if err != nil || !found || timestamp != "1720000001.000002" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
	if call := client.lastCall(); call.method != "history" || call.latest != "" || call.limit != 100 || !call.hadDeadline {
		t.Fatalf("recovery history call = %#v", call)
	}
}

func TestHistoryReaderRejectsRecoveryWithWrongDigest(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: "some text", Timestamp: "1720000001.000002", Metadata: slackapi.SlackMetadata{
			EventType: assistantMetadataEventType,
			EventPayload: map[string]any{
				"correlation_id": "intent-correlation",
				"render_mode":    "markdown_v1",
				"part_index":     float64(1),
				"part_count":     float64(1),
				"content_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
			},
		}}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRecoveryUsesSameSafetyRenderingAsPublisher(t *testing.T) {
	t.Parallel()
	content := "Do not notify <@U12345678> or <!channel>."
	parts := renderMarkdownV1(content, true)
	client := &fakeHistoryClient{history: []slackapi.Message{{Msg: slackapi.Msg{
		User: testBot, Timestamp: "1720000001.000002",
		Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(parts[0])),
	}}}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || !found || timestamp != "1720000001.000002" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRejectsConflictingCandidateMetadata(t *testing.T) {
	t.Parallel()
	part := renderMarkdownV1("published reply", true)[0]
	valid := exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(part))
	unknown := exchangeMetadataFor("intent-correlation", "markdown_v2", 1, 1, contentSHA256(part))
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: valid}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: unknown}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("conflicting recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRejectsReorderedMultipartDelivery(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("x", SlackMarkdownChunkRunes+100)
	parts := renderMarkdownV1(content, true)
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, contentSHA256(parts[0]))}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 2, 2, contentSHA256(parts[1]))}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("reordered recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderReturnsFinalMultipartTimestamp(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("x", SlackMarkdownChunkRunes+100)
	parts := renderMarkdownV1(content, true)
	client := &fakeHistoryClient{history: []slackapi.Message{
		// conversations.history is newest first; metadata indices remain authoritative.
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 2, 2, contentSHA256(parts[1]))}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, contentSHA256(parts[0]))}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || !found || timestamp != "1720000002.000002" {
		t.Fatalf("multipart recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRejectsDuplicatePart(t *testing.T) {
	t.Parallel()
	part := renderMarkdownV1("published reply", true)[0]
	metadata := exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(part))
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: metadata}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: metadata}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	_, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found {
		t.Fatalf("duplicate recovery found = %t, err = %v", found, err)
	}
}

func TestHistoryReaderRejectsEditedMatchingCandidate(t *testing.T) {
	t.Parallel()
	part := renderMarkdownV1("published reply", true)[0]
	client := &fakeHistoryClient{history: []slackapi.Message{{Msg: slackapi.Msg{
		User: testBot, Timestamp: "1720000001.000001", Edited: &slackapi.Edited{},
		Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(part)),
	}}}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	_, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found {
		t.Fatalf("edited recovery found = %t, err = %v", found, err)
	}
}

func TestHistoryReaderRejectsRecoveryWithoutMetadata(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: "published reply", Timestamp: "1720000001.000002"}},
	}}
	reader := newHistoryReader(client, testBot, 0, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("ambiguous FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestMapHistoryExcludesApplicationOwnedControlMessages(t *testing.T) {
	messages := []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: "Working", Timestamp: "1720000000.000001", Metadata: slackapi.SlackMetadata{EventType: progressMetadataEventType}}},
		{Msg: slackapi.Msg{User: testBot, Text: "Try this", Timestamp: "1720000001.000001", Metadata: slackapi.SlackMetadata{EventType: promptMetadataEventType}}},
		{Msg: slackapi.Msg{User: testUser, Text: "question", Timestamp: "1720000002.000001"}},
	}
	history := mapHistory(messages, testBot, 1000)
	if history.BotParticipated || len(history.Messages) != 1 || history.Messages[0].Content != "question" {
		t.Fatalf("control messages leaked into history: %#v", history)
	}
}

func TestHistoryReaderRequiresCorrelationOnEveryMultipartChunk(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("x", SlackMarkdownChunkRunes+100)
	chunks := SplitMarkdown(content, SlackMarkdownChunkRunes, true)
	if len(chunks) != 2 {
		t.Fatalf("SplitMarkdown returned %d chunks, want 2", len(chunks))
	}
	digest0 := contentSHA256(chunks[0])
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: chunks[0], Timestamp: "1720000001.000001", Metadata: slackapi.SlackMetadata{
			EventType: assistantMetadataEventType,
			EventPayload: map[string]any{
				"correlation_id": "intent-correlation",
				"render_mode":    "markdown_v1",
				"part_index":     float64(1),
				"part_count":     float64(2),
				"content_sha256": digest0,
			},
		}}},
		// Missing metadata on second chunk
		{Msg: slackapi.Msg{User: testBot, Text: chunks[1], Timestamp: "1720000002.000002"}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("partial correlation recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderDetectsBotParticipationWithTranslatedMessage(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "question", Timestamp: "1.1"}},
		{Msg: slackapi.Msg{User: testBot, Text: "", Timestamp: "1.2", SubType: slackapi.MsgSubTypeBotMessage,
			Blocks: slackapi.Blocks{BlockSet: []slackapi.Block{
				slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", "translated response", false, false), nil, nil),
			}},
		}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 5, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if !got.BotParticipated {
		t.Fatal("bot participation = false for translated message")
	}
	if len(got.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (user + bot with translated content)", len(got.Messages))
	}
	if got.Messages[1].Content != "translated response" {
		t.Fatalf("extracted translated content = %q", got.Messages[1].Content)
	}
}

func TestHistoryReaderSkipsEmptyTranslatedBotMessage(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "question", Timestamp: "1.1"}},
		{Msg: slackapi.Msg{User: testBot, Text: "", Timestamp: "1.2", SubType: slackapi.MsgSubTypeBotMessage}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, true)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 5, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if !got.BotParticipated {
		t.Fatal("bot participation should still be detected even with empty text")
	}
	if len(got.Messages) != 1 {
		t.Fatalf("message count = %d, want 1 (only user message)", len(got.Messages))
	}
}

func TestHistoryReaderExtractsObservedTranslatedBlocksWithinLimit(t *testing.T) {
	t.Parallel()
	table := slackapi.NewTableBlock("").AddRow(
		slackapi.NewTableRawTextCell("Name"),
		slackapi.NewTableRawTextCell("Value"),
	).AddRow(
		slackapi.NewTableRawTextCell("item"),
		slackapi.NewTableRawNumberCell(42),
	)
	blocks := []slackapi.Block{
		slackapi.NewMarkdownBlock("", "# Heading"),
		table,
	}
	got := extractPlainTextFromBlocks(blocks, 100)
	if got != "# Heading\nName | Value\nitem | 42" {
		t.Fatalf("translated block text = %q", got)
	}
	bounded := extractPlainTextFromBlocks([]slackapi.Block{
		slackapi.NewMarkdownBlock("", strings.Repeat("界", 50)),
	}, 12)
	if len([]rune(bounded)) != 12 {
		t.Fatalf("bounded translated text has %d runes", len([]rune(bounded)))
	}
}

func TestHistoryReaderValidatesDependenciesAndInput(t *testing.T) {
	t.Parallel()
	validClient := &fakeHistoryClient{}
	tests := []struct {
		name   string
		reader *HistoryReader
		inv    domain.Invocation
		limits domain.ContextLimits
	}{
		{name: "missing client", reader: newHistoryReader(nil, testBot, time.Second, nil, true), inv: validDMInvocation(), limits: domain.ContextLimits{MaxMessages: 1, MaxChars: 1}},
		{name: "missing bot ID", reader: newHistoryReader(validClient, "", time.Second, nil, true), inv: validDMInvocation(), limits: domain.ContextLimits{MaxMessages: 1, MaxChars: 1}},
		{name: "invalid limits", reader: newHistoryReader(validClient, testBot, time.Second, nil, true), inv: validDMInvocation()},
		{name: "invalid invocation", reader: newHistoryReader(validClient, testBot, time.Second, nil, true), inv: domain.Invocation{}, limits: domain.ContextLimits{MaxMessages: 1, MaxChars: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.reader.RecentHistory(context.Background(), tt.inv, tt.limits); err == nil {
				t.Fatal("RecentHistory() error = nil")
			}
		})
	}
}

func TestHistoryReaderRecoveryWithPartLabelsDisabled(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("x", SlackMarkdownChunkRunes+100)
	parts := renderMarkdownV1(content, false)
	if len(parts) != 2 {
		t.Fatalf("renderMarkdownV1(false) returned %d parts, want 2", len(parts))
	}
	for i, part := range parts {
		if strings.HasPrefix(part, "Part ") {
			t.Fatalf("part %d unexpectedly has label prefix: %q", i+1, part)
		}
	}
	digests := make([]string, len(parts))
	for i, part := range parts {
		digests[i] = contentSHA256(part)
	}
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, digests[0])}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 2, 2, digests[1])}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil, false)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || !found || timestamp != "1720000002.000002" {
		t.Fatalf("FindPublishedAssistantExchange(false) = %q, %t, %v", timestamp, found, err)
	}

	// Missing metadata still fails.
	clientMissing := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, digests[0])}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002"}},
	}}
	readerMissing := newHistoryReader(clientMissing, testBot, time.Second, nil, false)
	_, foundMissing, errMissing := readerMissing.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if errMissing != nil || foundMissing {
		t.Fatalf("missing metadata recovery = %t, %v", foundMissing, errMissing)
	}

	// Wrong digest still fails.
	clientBadDigest := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, "0000000000000000000000000000000000000000000000000000000000000000")}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 2, 2, digests[1])}},
	}}
	readerBadDigest := newHistoryReader(clientBadDigest, testBot, time.Second, nil, false)
	_, foundBad, errBad := readerBadDigest.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if errBad != nil || foundBad {
		t.Fatalf("bad digest recovery = %t, %v", foundBad, errBad)
	}

	// Duplicate part still fails.
	clientDup := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, digests[0])}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, digests[0])}},
	}}
	readerDup := newHistoryReader(clientDup, testBot, time.Second, nil, false)
	_, foundDup, errDup := readerDup.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if errDup != nil || foundDup {
		t.Fatalf("duplicate recovery = %t, %v", foundDup, errDup)
	}

	// Reordered parts still fail (part 1 has later timestamp than part 2).
	clientReorder := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 2, 2, digests[1])}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, digests[0])}},
	}}
	readerReorder := newHistoryReader(clientReorder, testBot, time.Second, nil, false)
	_, foundReorder, errReorder := readerReorder.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if errReorder != nil || foundReorder {
		t.Fatalf("reordered recovery = %t, %v", foundReorder, errReorder)
	}
}

type historyCall struct {
	method      string
	channelID   string
	rootTS      string
	latest      string
	limit       int
	hadDeadline bool
}

type fakeHistoryClient struct {
	mu      sync.Mutex
	calls   []historyCall
	replies []slackapi.Message
	history []slackapi.Message
	err     error
}

func (c *fakeHistoryClient) ConversationReplies(ctx context.Context, channelID, rootTS, latest string, limit int) ([]slackapi.Message, error) {
	c.record(ctx, historyCall{method: "replies", channelID: channelID, rootTS: rootTS, latest: latest, limit: limit})
	return append([]slackapi.Message(nil), c.replies...), c.err
}

func (c *fakeHistoryClient) ConversationHistory(ctx context.Context, channelID, latest string, limit int) ([]slackapi.Message, error) {
	c.record(ctx, historyCall{method: "history", channelID: channelID, latest: latest, limit: limit})
	return append([]slackapi.Message(nil), c.history...), c.err
}

func (c *fakeHistoryClient) record(ctx context.Context, call historyCall) {
	_, call.hadDeadline = ctx.Deadline()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *fakeHistoryClient) lastCall() historyCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return historyCall{}
	}
	return c.calls[len(c.calls)-1]
}

func validThreadInvocation() domain.Invocation {
	return domain.Invocation{
		EventID: testEventID, EventType: "message", TeamID: testTeam,
		ChannelID: testChannel, ChannelKind: domain.ChannelPublic, UserID: testUser,
		EventTS: testTS, ThreadTS: testThread, Text: "follow up", Trigger: domain.TriggerThreadReply,
	}
}

func validDMInvocation() domain.Invocation {
	return domain.Invocation{
		EventID: testEventID, EventType: "message", TeamID: testTeam,
		ChannelID: testDM, ChannelKind: domain.ChannelDM, UserID: testUser,
		EventTS: testTS, Text: "hello", Trigger: domain.TriggerDirectMessage,
	}
}
