package slack

import (
	"context"
	"errors"
	"fmt"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/port"
)

type CanvasCreator struct {
	client  *slackapi.Client
	timeout time.Duration
}

func NewCanvasCreator(client *slackapi.Client, timeout time.Duration) *CanvasCreator {
	return &CanvasCreator{client: client, timeout: timeout}
}

func (c *CanvasCreator) CreateCanvas(ctx context.Context, title string, documentContent string) (port.CanvasCreateResult, error) {
	if c == nil || c.client == nil {
		return port.CanvasCreateResult{}, fmt.Errorf("canvas client is not configured")
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	canvasID, err := c.client.CreateCanvasContext(ctx, title, slackapi.DocumentContent{
		Type:     "markdown",
		Markdown: documentContent,
	})
	if err != nil {
		ambiguous := true
		var slackErr slackapi.SlackErrorResponse
		var rateLimitErr *slackapi.RateLimitedError
		var statusErr slackapi.StatusCodeError
		switch {
		case errors.As(err, &slackErr):
			ambiguous = slackErr.Err == "fatal_error" || slackErr.Err == "internal_error" || slackErr.Err == "service_unavailable"
		case errors.As(err, &rateLimitErr):
			ambiguous = false
		case errors.As(err, &statusErr):
			ambiguous = statusErr.Code >= 500
		}
		return port.CanvasCreateResult{}, &port.CanvasCreateError{
			Err: fmt.Errorf("create Slack canvas: %w", err), Ambiguous: ambiguous,
		}
	}
	return port.CanvasCreateResult{CanvasID: canvasID}, nil
}

var _ port.CanvasCreator = (*CanvasCreator)(nil)
