package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	blocksV1RenderMode   = "blocks_v1"
	maxFallbackText      = 3000
	maxContextElements   = 10
	maxContextText       = 3000
	maxTableRowsPerBlock = 100
	maxTableChars        = 10000
	maxBlocksPerMessage  = 50
)

type blockPostClient interface {
	PostBlocks(ctx context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error)
}

type sdkBlockPostClient struct {
	client *slackapi.Client
}

func (c sdkBlockPostClient) PostBlocks(ctx context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionText(fallbackText, false),
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionDisableLinkUnfurl(),
		slackapi.MsgOptionDisableMediaUnfurl(),
	}
	if threadTS != "" {
		options = append(options, slackapi.MsgOptionTS(threadTS))
	}
	if metadata.EventType != "" {
		options = append(options, slackapi.MsgOptionMetadata(metadata))
	}
	_, timestamp, err := c.client.PostMessageContext(ctx, channelID, options...)
	return timestamp, err
}

type BlockPublisher struct {
	client   blockPostClient
	timeout  time.Duration
	pace     time.Duration
	sleep    sleepFunc
	now      func() time.Time
	logger   port.Logger
	channels sync.Map
}

func NewBlockPublisher(client *slackapi.Client, timeout time.Duration, logger port.Logger) *BlockPublisher {
	var poster blockPostClient
	if client != nil {
		poster = sdkBlockPostClient{client: client}
	}
	return newBlockPublisher(poster, timeout, logger)
}

func newBlockPublisher(client blockPostClient, timeout time.Duration, logger port.Logger) *BlockPublisher {
	return &BlockPublisher{
		client: client, timeout: timeout, pace: defaultPace,
		sleep: sleepContext, now: time.Now, logger: loggerOrDiscard(logger),
	}
}

func (p *BlockPublisher) ValidateStructured(presentation domain.Presentation) error {
	if err := domain.ValidatePresentation(presentation); err != nil {
		return fmt.Errorf("invalid presentation: %w", err)
	}
	encoded, err := json.Marshal(presentation)
	if err != nil {
		return fmt.Errorf("encode presentation: %w", err)
	}
	if _, err := renderPresentationParts(presentation, presentationBlockIdentity("", encoded)); err != nil {
		return fmt.Errorf("render presentation blocks: %w", err)
	}
	return nil
}

func (p *BlockPublisher) PublishStructured(ctx context.Context, target domain.ReplyTarget, presentation domain.Presentation) (port.PublishedResponse, error) {
	if p == nil || p.client == nil {
		return port.PublishedResponse{}, errors.New("block posting client is required")
	}
	if target.ChannelID == "" {
		return port.PublishedResponse{}, errors.New("channel is required")
	}
	if err := p.ValidateStructured(presentation); err != nil {
		return port.PublishedResponse{}, err
	}

	encoded, err := json.Marshal(presentation)
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("encode presentation: %w", err)
	}
	parts, err := renderPresentationParts(presentation, presentationBlockIdentity(target.CorrelationID, encoded))
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("render presentation blocks: %w", err)
	}
	fallbackText := neutralizeUnsafeControls(presentation.FallbackMarkdown)
	if utf8.RuneCountInString(fallbackText) > maxFallbackText {
		return port.PublishedResponse{}, fmt.Errorf("fallback text exceeds %d character limit", maxFallbackText)
	}

	result := port.PublishedResponse{}
	channel := p.channelPace(target.ChannelID)
	channel.mu.Lock()
	defer channel.mu.Unlock()
	for index, blocks := range parts {
		metadata := slackapi.SlackMetadata{}
		if target.CorrelationID != "" {
			metadata.EventType = assistantMetadataEventType
			metadata.EventPayload = map[string]any{
				"correlation_id": target.CorrelationID,
				"render_mode":    blocksV1RenderMode,
				"part_index":     index + 1,
				"part_count":     len(parts),
				"content_sha256": structuredPartDigest(encoded, index+1),
			}
		}

		if err := p.waitForChannel(ctx, channel); err != nil {
			return result, fmt.Errorf("pace Slack channel %s: %w", target.ChannelID, err)
		}
		timestamp, postErr := p.postWithRetry(ctx, target.ChannelID, fallbackText, blocks, metadata, target.ThreadTS)
		channel.lastAttempt = p.now()
		if postErr != nil {
			return result, fmt.Errorf("post Slack blocks part %d of %d: %w", index+1, len(parts), secure.NewRedactor().Error(postErr))
		}
		if timestamp == "" {
			return result, fmt.Errorf("post Slack blocks part %d of %d returned no timestamp", index+1, len(parts))
		}
		result.LastMessageTS = timestamp
	}
	return result, nil
}

func (p *BlockPublisher) channelPace(channelID string) *channelPace {
	value, _ := p.channels.LoadOrStore(channelID, &channelPace{})
	return value.(*channelPace)
}

func (p *BlockPublisher) waitForChannel(ctx context.Context, channel *channelPace) error {
	if p.pace <= 0 || channel.lastAttempt.IsZero() {
		return nil
	}
	wait := p.pace - p.now().Sub(channel.lastAttempt)
	if wait <= 0 {
		return nil
	}
	return p.sleep(ctx, wait)
}

func (p *BlockPublisher) postWithRetry(ctx context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		callCtx := ctx
		cancel := func() {}
		if p.timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, p.timeout)
		}
		timestamp, err := p.client.PostBlocks(callCtx, channelID, fallbackText, blocks, metadata, threadTS)
		cancel()
		if err == nil {
			return timestamp, nil
		}
		var rateLimited *slackapi.RateLimitedError
		if attempt != 0 || !errors.As(err, &rateLimited) {
			return "", err
		}
		p.logger.Warn("Slack block response rate limited; retrying once", "channel_id", channelID, "retry_after", rateLimited.RetryAfter)
		if err := p.sleep(ctx, max(rateLimited.RetryAfter, 0)); err != nil {
			return "", err
		}
	}
	return "", errors.New("Slack block response retry exhausted")
}

func structuredPartDigest(presentationJSON []byte, partIndex int) string {
	return contentSHA256(string(presentationJSON) + "\npart:" + strconv.Itoa(partIndex))
}

func presentationBlockIdentity(correlationID string, presentationJSON []byte) string {
	if correlationID != "" {
		return contentSHA256(correlationID)[:16]
	}
	return contentSHA256(string(presentationJSON))[:16]
}

func renderPresentationBlocks(presentation domain.Presentation) ([]slackapi.Block, error) {
	encoded, err := json.Marshal(presentation)
	if err != nil {
		return nil, err
	}
	parts, err := renderPresentationParts(presentation, contentSHA256(string(encoded))[:16])
	if err != nil {
		return nil, err
	}
	var blocks []slackapi.Block
	for _, part := range parts {
		blocks = append(blocks, part...)
	}
	return blocks, nil
}

func renderPresentationParts(presentation domain.Presentation, identity string) ([][]slackapi.Block, error) {
	if utf8.RuneCountInString(neutralizeUnsafeControls(presentation.FallbackMarkdown)) > maxFallbackText {
		return nil, fmt.Errorf("fallback text exceeds %d character limit", maxFallbackText)
	}

	var sourceBlock slackapi.Block
	if len(presentation.Sources) > 0 {
		if len(presentation.Sources) > maxContextElements {
			return nil, fmt.Errorf("sources exceed %d element limit", maxContextElements)
		}
		elements := make([]slackapi.MixedElement, 0, len(presentation.Sources))
		for _, source := range presentation.Sources {
			text := sourceLinkText(source)
			if utf8.RuneCountInString(text) > maxContextText {
				return nil, fmt.Errorf("source link exceeds %d character limit", maxContextText)
			}
			elements = append(elements, slackapi.NewTextBlockObject(slackapi.MarkdownType, text, false, false))
		}
		sourceBlock = slackapi.NewContextBlock("sources_v1_"+identity, elements...)
	}

	if presentation.Table == nil {
		var blocks []slackapi.Block
		if sourceBlock != nil {
			blocks = append(blocks, sourceBlock)
		} else {
			text := slackapi.NewTextBlockObject(slackapi.MarkdownType, neutralizeUnsafeControls(presentation.FallbackMarkdown), false, false)
			section := slackapi.NewSectionBlock(text, nil, nil)
			section.BlockID = "fallback_v1_" + identity
			blocks = append(blocks, section)
		}
		return [][]slackapi.Block{blocks}, nil
	}

	tableParts, err := renderTableParts(presentation.Table, identity)
	if err != nil {
		return nil, err
	}
	if sourceBlock != nil {
		tableParts[0] = append([]slackapi.Block{sourceBlock}, tableParts[0]...)
	}
	for _, blocks := range tableParts {
		if len(blocks) > maxBlocksPerMessage {
			return nil, fmt.Errorf("presentation exceeds %d block limit", maxBlocksPerMessage)
		}
	}
	return tableParts, nil
}

func renderTableParts(table *domain.Table, identity string) ([][]slackapi.Block, error) {
	if len(table.Headers) == 0 {
		return nil, errors.New("table has no columns")
	}
	if len(table.Headers) > 20 {
		return nil, errors.New("table exceeds 20 column limit")
	}
	caption := escapeSlackControl(table.Caption)
	if utf8.RuneCountInString(caption) > maxContextText {
		return nil, fmt.Errorf("table caption exceeds %d character limit", maxContextText)
	}

	header := make([]slackapi.TableCell, len(table.Headers))
	headerChars := 0
	for i, value := range table.Headers {
		escaped := escapeSlackControl(value)
		header[i] = newTableTextCell(escaped, true)
		headerChars += utf8.RuneCountInString(escaped)
	}
	if headerChars > maxTableChars {
		return nil, fmt.Errorf("table header exceeds %d aggregate character limit", maxTableChars)
	}

	columnSettings := make([]slackapi.ColumnSetting, len(table.Headers))
	for i := range columnSettings {
		columnSettings[i] = slackapi.ColumnSetting{Align: slackapi.ColumnAlignmentLeft}
	}

	var parts [][]slackapi.Block
	for start, partIndex := 0, 0; start < len(table.Rows) || (len(table.Rows) == 0 && partIndex == 0); partIndex++ {
		rows := [][]slackapi.TableCell{header}
		chars := headerChars
		end := start
		for end < len(table.Rows) && len(rows) < maxTableRowsPerBlock {
			row := make([]slackapi.TableCell, len(table.Rows[end]))
			rowChars := 0
			for i, value := range table.Rows[end] {
				escaped := escapeSlackControl(value)
				row[i] = newTableTextCell(escaped, i == table.RowHeader)
				rowChars += utf8.RuneCountInString(escaped)
			}
			if chars+rowChars > maxTableChars {
				if len(rows) == 1 {
					return nil, fmt.Errorf("table row %d exceeds %d aggregate character limit with repeated header", end, maxTableChars)
				}
				break
			}
			rows = append(rows, row)
			chars += rowChars
			end++
		}

		blocks := make([]slackapi.Block, 0, 2)
		if caption != "" {
			text := slackapi.NewTextBlockObject(slackapi.MarkdownType, caption, false, false)
			section := slackapi.NewSectionBlock(text, nil, nil)
			section.BlockID = fmt.Sprintf("caption_v1_%s_%d", identity, partIndex)
			blocks = append(blocks, section)
		}
		blocks = append(blocks, &slackapi.TableBlock{
			Type:           slackapi.MBTTable,
			BlockID:        fmt.Sprintf("table_v1_%s_%d", identity, partIndex),
			Rows:           rows,
			ColumnSettings: columnSettings,
		})
		parts = append(parts, blocks)
		start = end
	}
	return parts, nil
}

func newTableTextCell(text string, bold bool) slackapi.TableCell {
	if !bold {
		return slackapi.NewTableRawTextCell(text)
	}
	return slackapi.NewTableRichTextCell(slackapi.NewRichTextSection(
		slackapi.NewRichTextSectionTextElement(text, &slackapi.RichTextSectionTextStyle{Bold: true}),
	))
}

func sourceLinkText(source domain.Source) string {
	return "<" + escapeSlackControl(source.URL) + "|" + escapeSlackControl(source.Text) + ">"
}

func escapeSlackControl(text string) string {
	var builder strings.Builder
	for _, r := range text {
		switch r {
		case '<':
			builder.WriteString("&lt;")
		case '>':
			builder.WriteString("&gt;")
		case '&':
			builder.WriteString("&amp;")
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

var _ port.StructuredPublisher = (*BlockPublisher)(nil)
