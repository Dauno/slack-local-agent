package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	defaultPace                = time.Second
	assistantMetadataEventType = "local_agent_assistant_exchange"
)

type postRequest struct {
	channelID     string
	markdown      string
	threadTS      string
	correlationID string
	renderMode    string
	partIndex     int
	partCount     int
	contentSHA256 string
}

type postClient interface {
	PostMessage(ctx context.Context, req postRequest) (string, error)
}

type sdkPostClient struct {
	client *slackapi.Client
}

func (c sdkPostClient) PostMessage(ctx context.Context, req postRequest) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionMarkdownText(req.markdown),
		slackapi.MsgOptionDisableLinkUnfurl(),
		slackapi.MsgOptionDisableMediaUnfurl(),
	}
	if req.threadTS != "" {
		options = append(options, slackapi.MsgOptionTS(req.threadTS))
	}
	if req.correlationID != "" {
		payload := map[string]any{
			"correlation_id": req.correlationID,
			"render_mode":    req.renderMode,
			"part_index":     req.partIndex,
			"part_count":     req.partCount,
			"content_sha256": req.contentSHA256,
		}
		options = append(options, slackapi.MsgOptionMetadata(slackapi.SlackMetadata{
			EventType:    assistantMetadataEventType,
			EventPayload: payload,
		}))
	}
	_, timestamp, err := c.client.PostMessageContext(ctx, req.channelID, options...)
	return timestamp, err
}

type sleepFunc func(context.Context, time.Duration) error

// Publisher implements port.ResponsePublisher using Slack chat.postMessage.
type Publisher struct {
	client     postClient
	timeout    time.Duration
	pace       time.Duration
	sleep      sleepFunc
	now        func() time.Time
	logger     port.Logger
	partLabels bool
	channels   sync.Map
}

type channelPace struct {
	mu          sync.Mutex
	lastAttempt time.Time
}

func NewPublisher(client *slackapi.Client, timeout time.Duration, logger port.Logger, partLabels bool) *Publisher {
	var poster postClient
	if client != nil {
		poster = sdkPostClient{client: client}
	}
	return newPublisher(poster, timeout, logger, partLabels)
}

func newPublisher(client postClient, timeout time.Duration, logger port.Logger, partLabels bool) *Publisher {
	return &Publisher{
		client: client, timeout: timeout, pace: defaultPace,
		sleep: sleepContext, now: time.Now, logger: loggerOrDiscard(logger),
		partLabels: partLabels,
	}
}

func (p *Publisher) Publish(ctx context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	if p == nil || p.client == nil {
		return port.PublishedResponse{}, errors.New("Slack posting client is required")
	}
	if target.ChannelID == "" {
		return port.PublishedResponse{}, errors.New("Slack response channel is required")
	}
	if strings.TrimSpace(text) == "" {
		return port.PublishedResponse{}, errors.New("Slack response text is required")
	}

	chunks := renderMarkdownV1(text, p.partLabels)
	if len(chunks) == 0 {
		return port.PublishedResponse{}, errors.New("markdown splitting produced no parts")
	}

	result := port.PublishedResponse{}
	channel := p.channelPace(target.ChannelID)
	channel.mu.Lock()
	defer channel.mu.Unlock()
	for index, chunk := range chunks {
		if err := p.waitForChannel(ctx, channel); err != nil {
			return result, fmt.Errorf("pace Slack channel %s: %w", target.ChannelID, err)
		}
		req := postRequest{
			channelID:     target.ChannelID,
			markdown:      chunk,
			threadTS:      target.ThreadTS,
			correlationID: target.CorrelationID,
			renderMode:    markdownRenderMode,
			partIndex:     index + 1,
			partCount:     len(chunks),
			contentSHA256: contentSHA256(chunk),
		}
		timestamp, err := p.postWithRetry(ctx, req)
		channel.lastAttempt = p.now()
		if err != nil {
			safeErr := secure.NewRedactor().Error(err)
			p.logger.Error("Slack response posting failed", "channel_id", target.ChannelID, "chunk", index+1, "chunks", len(chunks), "error", safeErr)
			return result, fmt.Errorf("post Slack response chunk %d of %d: %w", index+1, len(chunks), safeErr)
		}
		result.LastMessageTS = timestamp
	}
	return result, nil
}

func (p *Publisher) channelPace(channelID string) *channelPace {
	value, _ := p.channels.LoadOrStore(channelID, &channelPace{})
	return value.(*channelPace)
}

func (p *Publisher) waitForChannel(ctx context.Context, channel *channelPace) error {
	if p.pace <= 0 || channel.lastAttempt.IsZero() {
		return nil
	}
	wait := p.pace - p.now().Sub(channel.lastAttempt)
	if wait <= 0 {
		return nil
	}
	return p.sleep(ctx, wait)
}

func (p *Publisher) postWithRetry(ctx context.Context, req postRequest) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		callCtx := ctx
		cancel := func() {}
		if p.timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, p.timeout)
		}
		timestamp, err := p.client.PostMessage(callCtx, req)
		cancel()
		if err == nil {
			return timestamp, nil
		}

		var rateLimited *slackapi.RateLimitedError
		if attempt != 0 || !errors.As(err, &rateLimited) {
			return "", err
		}
		p.logger.Warn("Slack response rate limited; retrying once", "channel_id", req.channelID, "retry_after", rateLimited.RetryAfter)
		if err := p.sleep(ctx, max(rateLimited.RetryAfter, 0)); err != nil {
			return "", err
		}
	}
	return "", errors.New("Slack response retry exhausted")
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ port.ResponsePublisher = (*Publisher)(nil)
