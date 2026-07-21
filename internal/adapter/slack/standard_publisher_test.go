package slack

import (
	"context"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

type fakeStandardMessageClient struct {
	postedText     string
	postedMetadata slackapi.SlackMetadata
	updatedText    string
	updatedMeta    slackapi.SlackMetadata
	messages       []slackapi.Message
	hasMore        bool
}

func (c *fakeStandardMessageClient) PostStandard(_ context.Context, _, _, markdown string, metadata slackapi.SlackMetadata) (string, error) {
	c.postedText, c.postedMetadata = markdown, metadata
	return "1700000001.000001", nil
}

func (c *fakeStandardMessageClient) UpdateStandard(_ context.Context, _, _, markdown string, metadata slackapi.SlackMetadata) error {
	c.updatedText, c.updatedMeta = markdown, metadata
	return nil
}

func (c *fakeStandardMessageClient) StandardMessages(context.Context, string, string, int) ([]slackapi.Message, bool, error) {
	return c.messages, c.hasMore, nil
}

func TestStandardPublisherUsesApplicationOwnedProgressLabels(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}

	published, err := publisher.PublishProgress(t.Context(), target, operation)
	if err != nil {
		t.Fatal(err)
	}
	if published.LastMessageTS == "" || client.postedText != "Working" || client.postedMetadata.EventType != progressMetadataEventType {
		t.Fatalf("published=%#v text=%q metadata=%#v", published, client.postedText, client.postedMetadata)
	}
	operation.MessageTS = published.LastMessageTS
	operation.State = domain.ProgressFinalizing
	if err := publisher.UpdateProgress(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	if client.updatedText != "Finalizing" || client.updatedMeta.EventPayload["state"] != string(domain.ProgressFinalizing) {
		t.Fatalf("updated text=%q metadata=%#v", client.updatedText, client.updatedMeta)
	}
}

func TestStandardPublisherRecoversProgressByExactMetadata(t *testing.T) {
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	client := &fakeStandardMessageClient{messages: []slackapi.Message{{Msg: slackapi.Msg{
		User: "U00000001", Timestamp: "1700000001.000001", Metadata: progressMetadata(operation),
	}}}}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}

	published, found, err := publisher.RecoverProgress(t.Context(), operation)
	if err != nil || !found || published.LastMessageTS != "1700000001.000001" {
		t.Fatalf("published=%#v found=%v err=%v", published, found, err)
	}
	client.messages = append(client.messages, client.messages[0])
	if _, _, err := publisher.RecoverProgress(t.Context(), operation); err == nil {
		t.Fatal("duplicate progress metadata was accepted")
	}
}

func TestSuggestedPromptsNeutralizeSlackControls(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}
	_, err := publisher.PublishSuggestedPrompts(t.Context(), domain.ReplyTarget{
		ChannelID: "D00000001", ThreadTS: "1700000000.000001",
	}, "prompts-1", []string{"Ask <@U99999999> for help"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(client.postedText, "<@U99999999>") || !strings.Contains(client.postedText, "&lt;@U99999999>") {
		t.Fatalf("unsafe prompt rendering: %q", client.postedText)
	}
	if client.postedMetadata.EventType != promptMetadataEventType {
		t.Fatalf("metadata=%#v", client.postedMetadata)
	}
}

func TestIncrementalPublisherEnforcesObservedLimitAndCanonicalFinalMetadata(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}
	operation := domain.IncrementalOperation{
		ID: "incremental-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001",
		MessageTS: "1700000001.000001", RendererVersion: standardIncrementalRenderer, Sequence: 2, PrefixDigest: "digest",
	}
	if err := publisher.UpdateIncremental(t.Context(), operation, strings.Repeat("界", SlackMarkdownChunkRunes+1)); err == nil {
		t.Fatal("oversized incremental update was accepted")
	}
	if err := publisher.FinalizeIncremental(t.Context(), operation, "final answer", "assistant-correlation"); err != nil {
		t.Fatal(err)
	}
	if client.updatedMeta.EventType != assistantMetadataEventType || client.updatedMeta.EventPayload["correlation_id"] != "assistant-correlation" || client.updatedMeta.EventPayload["part_count"] != 1 {
		t.Fatalf("final metadata=%#v", client.updatedMeta)
	}
}
