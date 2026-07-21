package slack

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type socketClient interface {
	Run(context.Context) error
	Events() <-chan socketmode.Event
	Ack(context.Context, socketmode.Request) error
}

type sdkSocketClient struct {
	client *socketmode.Client
}

func (c sdkSocketClient) Run(ctx context.Context) error {
	return c.client.RunContext(ctx)
}

func (c sdkSocketClient) Events() <-chan socketmode.Event {
	return c.client.Events
}

func (c sdkSocketClient) Ack(ctx context.Context, request socketmode.Request) error {
	return c.client.AckCtx(ctx, request.EnvelopeID, nil)
}

// Listener owns the Socket Mode lifecycle and its acknowledge-before-dispatch
// boundary. Handler work is launched asynchronously with the listener context.
type Listener struct {
	client             socketClient
	router             Router
	logger             port.Logger
	interactiveHandler func(context.Context, domain.ConfirmationInteractiveAction) error
}

func NewListener(client *socketmode.Client, router Router, logger port.Logger) *Listener {
	var socket socketClient
	if client != nil {
		socket = sdkSocketClient{client: client}
	}
	return newListener(socket, router, logger)
}

func newListener(client socketClient, router Router, logger port.Logger) *Listener {
	return &Listener{client: client, router: router, logger: loggerOrDiscard(logger)}
}

func (l *Listener) SetInteractiveHandler(handler func(context.Context, domain.ConfirmationInteractiveAction) error) {
	if l == nil {
		return
	}
	l.interactiveHandler = handler
}

// Run blocks until the context is canceled or the Socket Mode client stops.
// Context cancellation is a normal shutdown and returns nil.
func (l *Listener) Run(ctx context.Context, handler func(context.Context, domain.Invocation)) error {
	if l == nil || l.client == nil {
		return errors.New("Socket Mode client is required")
	}
	if handler == nil {
		return errors.New("Slack invocation handler is required")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runResult := make(chan error, 1)
	go func() {
		runResult <- l.client.Run(runCtx)
	}()

	var handlers sync.WaitGroup
	waitHandlers := func() { handlers.Wait() }

	for {
		select {
		case <-ctx.Done():
			cancel()
			waitHandlers()
			err := <-runResult
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err()) {
				return nil
			}
			return fmt.Errorf("run Slack Socket Mode client: %w", err)

		case err := <-runResult:
			cancel()
			waitHandlers()
			if err == nil || (ctx.Err() != nil && errors.Is(err, context.Canceled)) {
				return nil
			}
			return fmt.Errorf("run Slack Socket Mode client: %w", err)

		case event, open := <-l.client.Events():
			if !open {
				cancel()
				waitHandlers()
				err := <-runResult
				if err == nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("run Slack Socket Mode client: %w", err)
			}

			switch event.Type {
			case socketmode.EventTypeInteractive:
				l.handleInteractive(runCtx, event, &handlers)
			case socketmode.EventTypeEventsAPI:
				l.handleEventsAPI(runCtx, event, &handlers, handler)
			}
		}
	}
}

func (l *Listener) handleInteractive(ctx context.Context, event socketmode.Event, handlers *sync.WaitGroup) {
	if event.Request == nil {
		l.logger.Warn("Slack interactive event ignored because its Socket Mode request is missing")
		return
	}

	if err := l.client.Ack(ctx, *event.Request); err != nil {
		l.logger.Error("Slack Socket Mode interactive acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
		if ctx.Err() != nil {
			return
		}
	}

	callback, ok := event.Data.(slack.InteractionCallback)
	if !ok {
		l.logger.Debug("unsupported Slack interactive payload ignored")
		return
	}

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		l.logger.Debug("non-confirmation interactive action ignored", "action_id", callback.ActionID)
		return
	}

	if l.interactiveHandler == nil {
		l.logger.Warn("interactive handler not configured, ignoring confirmation action")
		return
	}

	handlers.Add(1)
	go func() {
		defer handlers.Done()
		if err := l.interactiveHandler(ctx, action); err != nil {
			l.logger.Warn("interactive handler returned error", "error", err)
		}
	}()
}

func (l *Listener) handleEventsAPI(ctx context.Context, event socketmode.Event, handlers *sync.WaitGroup, handler func(context.Context, domain.Invocation)) {
	if event.Request == nil {
		l.logger.Warn("Slack event ignored because its Socket Mode request is missing")
		return
	}

	if err := l.client.Ack(ctx, *event.Request); err != nil {
		l.logger.Error("Slack Socket Mode acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
		if ctx.Err() != nil {
			return
		}
	}

	apiEvent, ok := event.Data.(slackevents.EventsAPIEvent)
	if !ok {
		l.logger.Debug("unsupported Slack Events API payload ignored")
		return
	}
	invocation, ok := l.router.Route(apiEvent)
	if !ok {
		l.logger.Debug("unsupported Slack event ignored", "event_type", apiEvent.InnerEvent.Type)
		return
	}

	handlers.Add(1)
	go func() {
		defer handlers.Done()
		handler(ctx, invocation)
	}()
}
