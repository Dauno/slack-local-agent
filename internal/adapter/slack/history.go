package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

type historyClient interface {
	ConversationReplies(context.Context, string, string, string, int) ([]slackapi.Message, error)
	ConversationHistory(context.Context, string, string, int) ([]slackapi.Message, error)
}

type sdkHistoryClient struct {
	client *slackapi.Client
}

func (c sdkHistoryClient) ConversationReplies(ctx context.Context, channelID, rootTS, latest string, limit int) ([]slackapi.Message, error) {
	messages, _, _, err := c.client.GetConversationRepliesContext(ctx, &slackapi.GetConversationRepliesParameters{
		ChannelID:          channelID,
		Timestamp:          rootTS,
		Latest:             latest,
		Inclusive:          true,
		Limit:              limit,
		IncludeAllMetadata: true,
	})
	return messages, err
}

func (c sdkHistoryClient) ConversationHistory(ctx context.Context, channelID, latest string, limit int) ([]slackapi.Message, error) {
	response, err := c.client.GetConversationHistoryContext(ctx, &slackapi.GetConversationHistoryParameters{
		ChannelID:          channelID,
		Latest:             latest,
		Inclusive:          true,
		Limit:              limit,
		IncludeAllMetadata: true,
	})
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("Slack conversations.history returned no response")
	}
	return response.Messages, nil
}

// HistoryReader recovers bounded Slack context without persisting it.
type HistoryReader struct {
	client     historyClient
	botUserID  string
	timeout    time.Duration
	logger     port.Logger
	partLabels bool
}

func NewHistoryReader(client *slackapi.Client, botUserID string, timeout time.Duration, logger port.Logger, partLabels bool) *HistoryReader {
	var history historyClient
	if client != nil {
		history = sdkHistoryClient{client: client}
	}
	return newHistoryReader(history, botUserID, timeout, logger, partLabels)
}

func newHistoryReader(client historyClient, botUserID string, timeout time.Duration, logger port.Logger, partLabels bool) *HistoryReader {
	return &HistoryReader{
		client: client, botUserID: botUserID, timeout: timeout,
		logger: loggerOrDiscard(logger), partLabels: partLabels,
	}
}

func (r *HistoryReader) RecentHistory(ctx context.Context, invocation domain.Invocation, limits domain.ContextLimits) (port.History, error) {
	if r == nil || r.client == nil {
		return port.History{}, errors.New("Slack history client is required")
	}
	if r.botUserID == "" {
		return port.History{}, errors.New("Slack bot user ID is required")
	}
	if limits.MaxMessages <= 0 || limits.MaxChars <= 0 {
		return port.History{}, errors.New("Slack history limits must be positive")
	}
	if err := invocation.Validate(); err != nil {
		return port.History{}, fmt.Errorf("invalid Slack history invocation: %w", err)
	}

	callCtx := ctx
	cancel := func() {}
	if r.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, r.timeout)
	}
	defer cancel()

	var (
		messages []slackapi.Message
		err      error
	)
	if invocation.ChannelKind == domain.ChannelDM && !invocation.ThreadedDM {
		messages, err = r.client.ConversationHistory(callCtx, invocation.ChannelID, invocation.EventTS, limits.MaxMessages)
		if len(messages) > limits.MaxMessages {
			messages = messages[:limits.MaxMessages]
		}
		slices.Reverse(messages) // conversations.history is newest first.
	} else {
		rootTS := invocation.ThreadTS
		if rootTS == "" {
			rootTS = invocation.EventTS
		}
		messages, err = r.client.ConversationReplies(callCtx, invocation.ChannelID, rootTS, invocation.EventTS, limits.MaxMessages)
		if len(messages) > limits.MaxMessages {
			messages = messages[len(messages)-limits.MaxMessages:]
		}
	}
	if err != nil {
		safeErr := secure.NewRedactor().Error(err)
		r.logger.Warn("Slack history read failed", "channel_id", invocation.ChannelID, "error", safeErr)
		return port.History{}, fmt.Errorf("read Slack conversation history: %w", safeErr)
	}

	history := mapHistory(messages, r.botUserID, limits.MaxChars)
	history.Messages = domain.LimitMessages(history.Messages, limits)
	return history, nil
}

// FindPublishedAssistantExchange provides fail-closed crash recovery. It
// validates render mode, part identity, and content digest without requiring
// exact returned message.text equality.
func (r *HistoryReader) FindPublishedAssistantExchange(ctx context.Context, intent port.AssistantExchangeIntent) (string, bool, error) {
	if r == nil || r.client == nil {
		return "", false, errors.New("Slack history client is required")
	}
	if r.botUserID == "" || intent.ChannelID == "" || intent.CorrelationID == "" {
		return "", false, errors.New("invalid assistant exchange finder input")
	}

	callCtx := ctx
	cancel := func() {}
	if r.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, r.timeout)
	}
	defer cancel()

	const recoveryHistoryLimit = 100
	var (
		messages []slackapi.Message
		err      error
	)
	if intent.ChannelKind == domain.ChannelDM && intent.RootTS == "" {
		messages, err = r.client.ConversationHistory(callCtx, intent.ChannelID, "", recoveryHistoryLimit)
	} else {
		if intent.RootTS == "" {
			return "", false, errors.New("threaded assistant exchange has no root timestamp")
		}
		messages, err = r.client.ConversationReplies(callCtx, intent.ChannelID, intent.RootTS, "", recoveryHistoryLimit)
	}
	if err != nil {
		safeErr := secure.NewRedactor().Error(err)
		return "", false, fmt.Errorf("read Slack conversation for assistant exchange recovery: %w", safeErr)
	}
	if intent.PresentationJSON != "" {
		return findPublishedStructuredExchange(messages, r.botUserID, intent)
	}

	expectedParts := renderMarkdownV1(intent.Content, r.partLabels)
	if len(expectedParts) == 0 {
		return "", false, errors.New("no expected parts from markdown splitter")
	}

	matched := make([]slackapi.Message, 0, len(expectedParts))
	for _, message := range messages {
		if message.Metadata.EventType != assistantMetadataEventType {
			continue
		}
		md := parseExchangeMetadata(message)
		if md.CorrelationID != intent.CorrelationID {
			continue
		}
		if message.User != r.botUserID || message.Hidden || message.Edited != nil || len(message.Files) != 0 {
			return "", false, nil
		}
		if md.RenderMode != markdownRenderMode || md.PartCount != len(expectedParts) ||
			md.PartIndex < 1 || md.PartIndex > len(expectedParts) || message.Timestamp == "" {
			return "", false, nil
		}
		expectedDigest := contentSHA256(expectedParts[md.PartIndex-1])
		if md.ContentSHA256 != expectedDigest {
			return "", false, nil
		}
		matched = append(matched, message)
	}

	if len(matched) != len(expectedParts) {
		return "", false, nil
	}

	seen := make(map[int]bool)
	byIndex := make([]slackapi.Message, len(expectedParts))
	for _, message := range matched {
		md := parseExchangeMetadata(message)
		if seen[md.PartIndex] {
			return "", false, nil // duplicate part index
		}
		seen[md.PartIndex] = true
		byIndex[md.PartIndex-1] = message
	}
	if len(seen) != len(expectedParts) {
		return "", false, nil
	}

	for index := 1; index < len(byIndex); index++ {
		previous := parseSlackTimestamp(byIndex[index-1].Timestamp)
		current := parseSlackTimestamp(byIndex[index].Timestamp)
		if previous.IsZero() || current.IsZero() || !previous.Before(current) {
			return "", false, nil
		}
	}

	return byIndex[len(byIndex)-1].Timestamp, true, nil
}

func findPublishedStructuredExchange(messages []slackapi.Message, botUserID string, intent port.AssistantExchangeIntent) (string, bool, error) {
	var presentation domain.Presentation
	if err := json.Unmarshal([]byte(intent.PresentationJSON), &presentation); err != nil {
		return "", false, fmt.Errorf("decode persisted presentation: %w", err)
	}
	if err := domain.ValidatePresentation(presentation); err != nil {
		return "", false, fmt.Errorf("validate persisted presentation: %w", err)
	}
	if presentation.FallbackMarkdown != intent.Content {
		return "", false, errors.New("persisted presentation fallback does not match canonical content")
	}
	canonical, err := json.Marshal(presentation)
	if err != nil {
		return "", false, fmt.Errorf("encode persisted presentation: %w", err)
	}
	if string(canonical) != intent.PresentationJSON {
		return "", false, errors.New("persisted presentation is not canonical JSON")
	}
	parts, err := renderPresentationParts(presentation, presentationBlockIdentity(intent.CorrelationID, canonical))
	if err != nil {
		return "", false, fmt.Errorf("reconstruct persisted presentation: %w", err)
	}

	matched := make([]slackapi.Message, 0, len(parts))
	for _, message := range messages {
		if message.Metadata.EventType != assistantMetadataEventType {
			continue
		}
		metadata := parseExchangeMetadata(message)
		if metadata.CorrelationID != intent.CorrelationID {
			continue
		}
		if message.User != botUserID || message.Hidden || message.Edited != nil || len(message.Files) != 0 {
			return "", false, nil
		}
		if metadata.RenderMode != blocksV1RenderMode || metadata.PartCount != len(parts) ||
			metadata.PartIndex < 1 || metadata.PartIndex > len(parts) || message.Timestamp == "" ||
			metadata.ContentSHA256 != structuredPartDigest(canonical, metadata.PartIndex) {
			return "", false, nil
		}
		matched = append(matched, message)
	}
	if len(matched) != len(parts) {
		return "", false, nil
	}

	byIndex := make([]slackapi.Message, len(parts))
	for _, message := range matched {
		index := parseExchangeMetadata(message).PartIndex - 1
		if byIndex[index].Timestamp != "" {
			return "", false, nil
		}
		byIndex[index] = message
	}
	for index := 1; index < len(byIndex); index++ {
		previous := parseSlackTimestamp(byIndex[index-1].Timestamp)
		current := parseSlackTimestamp(byIndex[index].Timestamp)
		if previous.IsZero() || current.IsZero() || !previous.Before(current) {
			return "", false, nil
		}
	}
	return byIndex[len(byIndex)-1].Timestamp, true, nil
}

type exchangeMetadata struct {
	CorrelationID string
	RenderMode    string
	PartIndex     int
	PartCount     int
	ContentSHA256 string
}

func parseExchangeMetadata(message slackapi.Message) exchangeMetadata {
	var md exchangeMetadata
	md.CorrelationID, _ = message.Metadata.EventPayload["correlation_id"].(string)
	md.RenderMode, _ = message.Metadata.EventPayload["render_mode"].(string)
	md.ContentSHA256, _ = message.Metadata.EventPayload["content_sha256"].(string)
	md.PartIndex, _ = metadataInt(message.Metadata.EventPayload["part_index"])
	md.PartCount, _ = metadataInt(message.Metadata.EventPayload["part_count"])
	return md
}

func metadataInt(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		if number != math.Trunc(number) || number < 1 || number > float64(math.MaxInt) {
			return 0, false
		}
		return int(number), true
	case json.Number:
		parsed, err := number.Int64()
		if err != nil || parsed < 1 || parsed > int64(math.MaxInt) {
			return 0, false
		}
		return int(parsed), true
	case int:
		return number, number > 0
	default:
		return 0, false
	}
}

func mapHistory(messages []slackapi.Message, botUserID string, maxChars int) port.History {
	history := port.History{Messages: make([]domain.Message, 0, len(messages))}
	for _, message := range messages {
		if message.User == "" || message.Hidden || message.Edited != nil || len(message.Files) != 0 {
			continue
		}
		switch message.Metadata.EventType {
		case progressMetadataEventType, promptMetadataEventType, confirmationMetadataEventType:
			continue
		}

		role := domain.RoleUser
		if message.User == botUserID {
			role = domain.RoleAssistant
			history.BotParticipated = true
		} else if message.SubType != "" {
			continue
		}

		content := message.Text
		if content == "" {
			content = extractPlainTextFromBlocks(message.Blocks.BlockSet, maxChars)
		}
		if strings.TrimSpace(content) == "" {
			if role == domain.RoleAssistant {
				// Bot participated in a translated message with no visible text.
				// Keep the message out of model context since we have no text to offer.
				continue
			}
			continue
		}

		history.Messages = append(history.Messages, domain.Message{
			Role:       role,
			Content:    content,
			UserID:     message.User,
			ExternalTS: message.Timestamp,
			CreatedAt:  parseSlackTimestamp(message.Timestamp),
		})
	}
	return history
}

func extractPlainTextFromBlocks(blocks []slackapi.Block, maxChars int) string {
	if len(blocks) == 0 {
		return ""
	}
	builder := newBoundedTextBuilder(maxChars)
	for _, block := range blocks {
		switch b := block.(type) {
		case *slackapi.MarkdownBlock:
			builder.WriteString(b.Text)
			builder.AppendByte('\n')
		case *slackapi.SectionBlock:
			if b.Text != nil {
				builder.WriteString(b.Text.Text)
				builder.AppendByte('\n')
			}
		case *slackapi.RichTextBlock:
			for _, element := range b.Elements {
				appendRichText(&builder, element)
			}
			builder.AppendByte('\n')
		case *slackapi.TableBlock:
			appendTable(&builder, b)
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

type boundedTextBuilder struct {
	builder   strings.Builder
	remaining int
}

func newBoundedTextBuilder(limit int) boundedTextBuilder {
	return boundedTextBuilder{remaining: max(limit, 0)}
}

func (b *boundedTextBuilder) WriteString(text string) {
	if b.remaining == 0 || text == "" {
		return
	}
	runes := []rune(text)
	if len(runes) > b.remaining {
		runes = runes[:b.remaining]
	}
	b.builder.WriteString(string(runes))
	b.remaining -= len(runes)
}

func (b *boundedTextBuilder) AppendByte(value byte) {
	if b.remaining > 0 {
		b.builder.WriteByte(value)
		b.remaining--
	}
}

func (b *boundedTextBuilder) String() string { return b.builder.String() }
func (b *boundedTextBuilder) Len() int       { return b.builder.Len() }

func appendRichText(builder *boundedTextBuilder, element slackapi.RichTextElement) {
	switch e := element.(type) {
	case *slackapi.RichTextSection:
		appendSectionElements(builder, e.Elements)
	case *slackapi.RichTextList:
		for _, item := range e.Elements {
			if text := builder.String(); len(text) > 0 && text[len(text)-1] != '\n' {
				builder.AppendByte('\n')
			}
			builder.WriteString("- ")
			if section, ok := item.(*slackapi.RichTextSection); ok {
				appendSectionElements(builder, section.Elements)
			}
		}
	case *slackapi.RichTextPreformatted:
		appendSectionElements(builder, e.Elements)
	case *slackapi.RichTextQuote:
		appendSectionElements(builder, e.Elements)
	}
}

func appendSectionElements(builder *boundedTextBuilder, elements []slackapi.RichTextSectionElement) {
	for _, el := range elements {
		switch sub := el.(type) {
		case *slackapi.RichTextSectionTextElement:
			builder.WriteString(sub.Text)
		case *slackapi.RichTextSectionLinkElement:
			if sub.Text != "" {
				builder.WriteString(sub.Text)
			} else {
				builder.WriteString(sub.URL)
			}
		case *slackapi.RichTextSectionEmojiElement:
			builder.WriteString(":" + sub.Name + ":")
		}
	}
}

func appendTable(builder *boundedTextBuilder, table *slackapi.TableBlock) {
	for _, row := range table.Rows {
		for index, cell := range row {
			if index > 0 {
				builder.WriteString(" | ")
			}
			switch value := cell.(type) {
			case *slackapi.TableRawTextCell:
				builder.WriteString(value.Text)
			case *slackapi.TableRawNumberCell:
				if value.Text != "" {
					builder.WriteString(value.Text)
				} else {
					builder.WriteString(strconv.FormatFloat(value.Value, 'f', -1, 64))
				}
			case *slackapi.TableRichTextCell:
				for _, element := range value.Elements {
					appendRichText(builder, element)
				}
			}
		}
		builder.AppendByte('\n')
	}
}

func parseSlackTimestamp(timestamp string) time.Time {
	secondsText, fractionText, found := strings.Cut(timestamp, ".")
	if !found {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(secondsText, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if len(fractionText) > 9 {
		fractionText = fractionText[:9]
	}
	fractionText += strings.Repeat("0", 9-len(fractionText))
	nanoseconds, err := strconv.ParseInt(fractionText, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(seconds, nanoseconds).UTC()
}

var _ port.HistoryReader = (*HistoryReader)(nil)
var _ port.AssistantExchangeFinder = (*HistoryReader)(nil)
