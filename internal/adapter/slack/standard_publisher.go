package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	progressMetadataEventType    = "local_agent_progress"
	promptMetadataEventType      = "local_agent_suggested_prompts"
	incrementalMetadataEventType = "local_agent_incremental"
	progressRecoveryLimit        = 100
	standardIncrementalRenderer  = "standard_incremental_v1"
)

type standardMessageClient interface {
	PostStandard(context.Context, string, string, string, slackapi.SlackMetadata) (string, error)
	UpdateStandard(context.Context, string, string, string, slackapi.SlackMetadata) error
	StandardMessages(context.Context, string, string, int) ([]slackapi.Message, bool, error)
}

type sdkStandardMessageClient struct {
	client *slackapi.Client
}

func (c sdkStandardMessageClient) PostStandard(ctx context.Context, channelID, threadTS, markdown string, metadata slackapi.SlackMetadata) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionMarkdownText(markdown),
		slackapi.MsgOptionDisableLinkUnfurl(),
		slackapi.MsgOptionDisableMediaUnfurl(),
		slackapi.MsgOptionMetadata(metadata),
	}
	if threadTS != "" {
		options = append(options, slackapi.MsgOptionTS(threadTS))
	}
	_, timestamp, err := c.client.PostMessageContext(ctx, channelID, options...)
	return timestamp, err
}

func (c sdkStandardMessageClient) UpdateStandard(ctx context.Context, channelID, messageTS, markdown string, metadata slackapi.SlackMetadata) error {
	_, _, _, err := c.client.UpdateMessageContext(ctx, channelID, messageTS,
		slackapi.MsgOptionMarkdownText(markdown), slackapi.MsgOptionMetadata(metadata))
	return err
}

func (c sdkStandardMessageClient) StandardMessages(ctx context.Context, channelID, threadTS string, limit int) ([]slackapi.Message, bool, error) {
	if threadTS != "" {
		messages, hasMore, _, err := c.client.GetConversationRepliesContext(ctx, &slackapi.GetConversationRepliesParameters{
			ChannelID: channelID, Timestamp: threadTS, Limit: limit, IncludeAllMetadata: true,
		})
		return messages, hasMore, err
	}
	response, err := c.client.GetConversationHistoryContext(ctx, &slackapi.GetConversationHistoryParameters{
		ChannelID: channelID, Limit: limit, IncludeAllMetadata: true,
	})
	if err != nil {
		return nil, false, err
	}
	return response.Messages, response.HasMore, nil
}

type StandardPublisher struct {
	client    standardMessageClient
	botUserID string
	timeout   time.Duration
}

func NewStandardPublisher(client *slackapi.Client, botUserID string, timeout time.Duration) *StandardPublisher {
	var standard standardMessageClient
	if client != nil {
		standard = sdkStandardMessageClient{client: client}
	}
	return &StandardPublisher{client: standard, botUserID: botUserID, timeout: timeout}
}

func (p *StandardPublisher) PublishProgress(ctx context.Context, target domain.ReplyTarget, operation domain.ProgressOperation) (port.PublishedResponse, error) {
	if err := p.validateProgress(operation); err != nil {
		return port.PublishedResponse{}, err
	}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	timestamp, err := p.client.PostStandard(callCtx, target.ChannelID, target.ThreadTS, progressLabel(operation.State), progressMetadata(operation))
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("publish Slack progress: %w", err)
	}
	return port.PublishedResponse{LastMessageTS: timestamp}, nil
}

func (p *StandardPublisher) UpdateProgress(ctx context.Context, operation domain.ProgressOperation) error {
	if err := p.validateProgress(operation); err != nil {
		return err
	}
	if operation.MessageTS == "" {
		return errors.New("Slack progress message timestamp is required")
	}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	if err := p.client.UpdateStandard(callCtx, operation.ChannelID, operation.MessageTS, progressLabel(operation.State), progressMetadata(operation)); err != nil {
		return fmt.Errorf("update Slack progress: %w", err)
	}
	return nil
}

func (p *StandardPublisher) RecoverProgress(ctx context.Context, operation domain.ProgressOperation) (port.PublishedResponse, bool, error) {
	if err := p.validateProgress(operation); err != nil {
		return port.PublishedResponse{}, false, err
	}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	messages, hasMore, err := p.client.StandardMessages(callCtx, operation.ChannelID, operation.ThreadTS, progressRecoveryLimit)
	if err != nil {
		return port.PublishedResponse{}, false, fmt.Errorf("recover Slack progress: %w", err)
	}
	if hasMore {
		return port.PublishedResponse{}, false, errors.New("recover Slack progress: bounded history is incomplete")
	}
	var match string
	for _, message := range messages {
		if message.User != p.botUserID || message.Metadata.EventType != progressMetadataEventType {
			continue
		}
		operationID, _ := message.Metadata.EventPayload["operation_id"].(string)
		if operationID != operation.ID {
			continue
		}
		if match != "" {
			return port.PublishedResponse{}, false, errors.New("recover Slack progress: duplicate operation metadata")
		}
		match = message.Timestamp
	}
	return port.PublishedResponse{LastMessageTS: match}, match != "", nil
}

func (p *StandardPublisher) PublishSuggestedPrompts(ctx context.Context, target domain.ReplyTarget, deliveryID string, prompts []string) (port.PublishedResponse, error) {
	if p == nil || p.client == nil {
		return port.PublishedResponse{}, errors.New("Slack standard publisher is required")
	}
	if target.ChannelID == "" || target.ThreadTS == "" || deliveryID == "" || len(prompts) == 0 {
		return port.PublishedResponse{}, errors.New("Slack suggested prompt identity and content are required")
	}
	var text strings.Builder
	text.WriteString("**Prueba con una de estas solicitudes:**")
	for _, prompt := range prompts {
		text.WriteString("\n- ")
		text.WriteString(prompt)
	}
	markdown := neutralizeUnsafeControls(text.String())
	if len([]rune(markdown)) > SlackMarkdownChunkRunes {
		return port.PublishedResponse{}, errors.New("Slack suggested prompts exceed one message")
	}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	metadata := slackapi.SlackMetadata{EventType: promptMetadataEventType, EventPayload: map[string]any{"delivery_id": deliveryID}}
	timestamp, err := p.client.PostStandard(callCtx, target.ChannelID, target.ThreadTS, markdown, metadata)
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("publish Slack suggested prompts: %w", err)
	}
	return port.PublishedResponse{LastMessageTS: timestamp}, nil
}

func (p *StandardPublisher) validateProgress(operation domain.ProgressOperation) error {
	if p == nil || p.client == nil {
		return errors.New("Slack standard publisher is required")
	}
	if p.botUserID == "" || operation.ID == "" || operation.ChannelID == "" || operation.ThreadTS == "" {
		return errors.New("Slack progress identity is required")
	}
	if progressLabel(operation.State) == "" {
		return fmt.Errorf("unsupported Slack progress state %q", operation.State)
	}
	return nil
}

func progressMetadata(operation domain.ProgressOperation) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{EventType: progressMetadataEventType, EventPayload: map[string]any{
		"operation_id": operation.ID,
		"state":        string(operation.State),
	}}
}

func progressLabel(state domain.ProgressState) string {
	switch state {
	case domain.ProgressWorking:
		return "Working"
	case domain.ProgressWaitingConfirmation:
		return "Waiting for approval"
	case domain.ProgressFinalizing:
		return "Finalizing"
	case domain.ProgressCleared:
		return "Completed"
	case domain.ProgressFailed, domain.ProgressInterrupted:
		return "Interrupted"
	default:
		return ""
	}
}

func standardTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

var _ port.ProgressPublisher = (*StandardPublisher)(nil)
var _ port.SuggestedPromptPublisher = (*StandardPublisher)(nil)
var _ port.IncrementalPublisher = (*StandardPublisher)(nil)

func (p *StandardPublisher) CreateIncremental(ctx context.Context, target domain.ReplyTarget, operation domain.IncrementalOperation, text string) (port.PublishedResponse, error) {
	markdown, err := incrementalMarkdown(text)
	if err != nil {
		return port.PublishedResponse{}, err
	}
	if err := p.validateIncremental(operation, false); err != nil {
		return port.PublishedResponse{}, err
	}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	timestamp, err := p.client.PostStandard(callCtx, target.ChannelID, target.ThreadTS, markdown, incrementalMetadata(operation))
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("create Slack incremental message: %w", err)
	}
	return port.PublishedResponse{LastMessageTS: timestamp}, nil
}

func (p *StandardPublisher) UpdateIncremental(ctx context.Context, operation domain.IncrementalOperation, text string) error {
	markdown, err := incrementalMarkdown(text)
	if err != nil {
		return err
	}
	if err := p.validateIncremental(operation, true); err != nil {
		return err
	}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	if err := p.client.UpdateStandard(callCtx, operation.ChannelID, operation.MessageTS, markdown, incrementalMetadata(operation)); err != nil {
		return fmt.Errorf("update Slack incremental message: %w", err)
	}
	return nil
}

func (p *StandardPublisher) FinalizeIncremental(ctx context.Context, operation domain.IncrementalOperation, text, assistantCorrelationID string) error {
	markdown, err := incrementalMarkdown(text)
	if err != nil {
		return err
	}
	if err := p.validateIncremental(operation, true); err != nil {
		return err
	}
	if assistantCorrelationID == "" {
		return errors.New("assistant correlation ID is required to finalize Slack incremental delivery")
	}
	metadata := slackapi.SlackMetadata{EventType: assistantMetadataEventType, EventPayload: map[string]any{
		"correlation_id": assistantCorrelationID, "render_mode": markdownRenderMode,
		"part_index": 1, "part_count": 1, "content_sha256": contentSHA256(markdown),
	}}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	if err := p.client.UpdateStandard(callCtx, operation.ChannelID, operation.MessageTS, markdown, metadata); err != nil {
		return fmt.Errorf("finalize Slack incremental message: %w", err)
	}
	return nil
}

func (p *StandardPublisher) InterruptIncremental(ctx context.Context, operation domain.IncrementalOperation, text string) error {
	if strings.TrimSpace(text) == "" {
		text = "Interrupted"
	}
	return p.UpdateIncremental(ctx, operation, text)
}

func (p *StandardPublisher) RecoverIncremental(ctx context.Context, operation domain.IncrementalOperation) (port.PublishedResponse, bool, error) {
	if err := p.validateIncremental(operation, false); err != nil {
		return port.PublishedResponse{}, false, err
	}
	callCtx, cancel := standardTimeout(ctx, p.timeout)
	defer cancel()
	messages, hasMore, err := p.client.StandardMessages(callCtx, operation.ChannelID, operation.ThreadTS, progressRecoveryLimit)
	if err != nil {
		return port.PublishedResponse{}, false, fmt.Errorf("recover Slack incremental message: %w", err)
	}
	if hasMore {
		return port.PublishedResponse{}, false, errors.New("recover Slack incremental message: bounded history is incomplete")
	}
	var match string
	for _, message := range messages {
		if message.User != p.botUserID || message.Metadata.EventType != incrementalMetadataEventType {
			continue
		}
		operationID, _ := message.Metadata.EventPayload["operation_id"].(string)
		if operationID != operation.ID {
			continue
		}
		if match != "" {
			return port.PublishedResponse{}, false, errors.New("recover Slack incremental message: duplicate operation metadata")
		}
		match = message.Timestamp
	}
	return port.PublishedResponse{LastMessageTS: match}, match != "", nil
}

func (p *StandardPublisher) validateIncremental(operation domain.IncrementalOperation, requireMessage bool) error {
	if p == nil || p.client == nil || p.botUserID == "" {
		return errors.New("Slack standard publisher is required")
	}
	if operation.ID == "" || operation.ChannelID == "" || operation.ThreadTS == "" || operation.RendererVersion != standardIncrementalRenderer {
		return errors.New("Slack incremental delivery identity is invalid")
	}
	if requireMessage && operation.MessageTS == "" {
		return errors.New("Slack incremental message timestamp is required")
	}
	return nil
}

func incrementalMetadata(operation domain.IncrementalOperation) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{EventType: incrementalMetadataEventType, EventPayload: map[string]any{
		"operation_id": operation.ID, "renderer_version": operation.RendererVersion,
		"sequence": operation.Sequence, "prefix_digest": operation.PrefixDigest,
	}}
}

func incrementalMarkdown(text string) (string, error) {
	markdown := neutralizeUnsafeControls(text)
	if strings.TrimSpace(markdown) == "" {
		return "", errors.New("Slack incremental text is required")
	}
	if len([]rune(markdown)) > SlackMarkdownChunkRunes {
		return "", fmt.Errorf("Slack incremental text exceeds %d Unicode code points", SlackMarkdownChunkRunes)
	}
	return markdown, nil
}
