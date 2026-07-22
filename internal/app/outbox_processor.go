package app

import (
	"context"
	"errors"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	memoryusecase "github.com/Dauno/slack-local-agent/internal/usecase/memory"
)

func processOutbox(
	ctx context.Context,
	store *adaptersqlite.Store,
	curator *memorycurator.Curator,
	memoryService *memoryusecase.Service,
	projector *memoryprojector.Projector,
	memoryDir string,
	maxRetries int,
	logger port.Logger,
) {
	for {
		item, err := store.ClaimNextOutboxItem(ctx)
		if err != nil {
			logger.Error("memory outbox claim failed", "error", err)
			return
		}
		if item == nil {
			return
		}

		messages, err := store.LoadOutboxMessages(ctx, item)
		if err != nil {
			logger.Error("memory outbox load messages failed", "item_id", item.ID, "error", err)
			retryOutbox(ctx, store, item, maxRetries, err)
			return
		}
		if len(messages) == 0 {
			err := errors.New("source exchange is no longer available")
			logger.Warn("memory outbox source exchange unavailable", "item_id", item.ID)
			retryOutbox(ctx, store, item, maxRetries, err)
			return
		}

		trusted, err := memoryService.TrustedEntityOperations(ctx, item.ConversationKey, messages)
		if err != nil {
			logger.Warn("trusted entity topic lookup failed", "item_id", item.ID, "error", err)
			retryOutbox(ctx, store, item, maxRetries, err)
			return
		}

		topics, err := memoryService.RelevantTopics(ctx, messages)
		if err != nil {
			logger.Warn("memory curator topic lookup failed", "item_id", item.ID, "error", err)
			retryOutbox(ctx, store, item, maxRetries, err)
			return
		}
		patch, err := curator.ProposePatch(ctx, item.ConversationKey, item.ExchangeTS, messages, topics)
		if err != nil {
			if errors.Is(err, port.ErrModelCallLimitReached) {
				if len(trusted) > 0 {
					logger.Debug("memory curator skipped by shared model-call limit; applying trusted entity operations", "item_id", item.ID)
				} else {
					logger.Debug("memory curator deferred by shared model-call limit", "item_id", item.ID)
					if err := rescheduleOutbox(ctx, store, item); err != nil {
						logger.Warn("memory curator reschedule failed", "item_id", item.ID, "error", err)
					}
					return
				}
			}
			if errors.Is(err, errCuratorResponseIncomplete) && len(trusted) == 0 {
				logger.Warn("memory curator response incomplete; discarding optional patch", "item_id", item.ID, "error", err)
				patch = domain.MemoryPatch{ConversationKey: item.ConversationKey, ExchangeTS: item.ExchangeTS}
			} else if len(trusted) == 0 {
				logger.Warn("memory curator proposal failed", "item_id", item.ID, "error", err)
				retryOutbox(ctx, store, item, maxRetries, err)
				return
			}
			if len(trusted) > 0 {
				logger.Warn("memory curator proposal failed; applying trusted entity operations", "item_id", item.ID, "error", err)
				patch = domain.MemoryPatch{ConversationKey: item.ConversationKey, ExchangeTS: item.ExchangeTS}
			}
		}
		patch.Operations = mergeTrustedEntityOperations(trusted, patch.Operations)
		for _, message := range messages {
			if message.Role == domain.RoleUser && message.UserID != "" {
				patch.SourceAuthorID = message.UserID
				break
			}
		}
		if err := memoryService.Validate(patch); err != nil {
			if len(trusted) > 0 {
				logger.Warn("optional curator patch rejected; applying trusted entity operations", "item_id", item.ID, "error", err)
				patch.Operations = trusted
			} else {
				logger.Warn("optional curator patch rejected; discarding", "item_id", item.ID, "error", err)
				patch.Operations = nil
			}
		}

		if _, applyErr := memoryService.ValidateAndApply(ctx, patch); applyErr != nil {
			logger.Warn("memory patch validation failed", "item_id", item.ID, "error", applyErr)
			retryOutbox(ctx, store, item, maxRetries, applyErr)
			return
		}

		if err := projector.Project(ctx, store, memoryDir); err != nil {
			logger.Error("memory projection failed", "error", err)
			retryOutbox(ctx, store, item, maxRetries, err)
			return
		}

		if err := store.CompleteOutboxItem(ctx, item.ID, item.LeaseUntil); err != nil {
			logger.Warn("memory outbox completion failed", "item_id", item.ID, "error", err)
			return
		}
		logger.Debug("memory curator processed exchange",
			"item_id", item.ID,
			"operations", len(patch.Operations))
	}
}

func mergeTrustedEntityOperations(trusted, proposed []domain.MemoryOp) []domain.MemoryOp {
	if len(trusted) == 0 {
		return proposed
	}
	trustedSlugs := make(map[string]struct{}, len(trusted))
	for _, op := range trusted {
		trustedSlugs[op.TopicSlug] = struct{}{}
	}
	result := append([]domain.MemoryOp(nil), trusted...)
	for _, op := range proposed {
		if _, superseded := trustedSlugs[op.TopicSlug]; !superseded {
			result = append(result, op)
		}
	}
	return result
}

func retryOutbox(ctx context.Context, store *adaptersqlite.Store, item *domain.OutboxItem, maxRetries int, cause error) {
	if item.Attempts >= maxRetries {
		_ = store.FailOutboxItem(ctx, item.ID, item.LeaseUntil, cause.Error())
		return
	}
	delay := time.Minute * time.Duration(1<<min(item.Attempts-1, 5))
	_ = store.RetryOutboxItem(ctx, item.ID, item.LeaseUntil, time.Now().UTC().Add(delay))
}

func rescheduleOutbox(ctx context.Context, store *adaptersqlite.Store, item *domain.OutboxItem) error {
	return store.RescheduleOutboxItem(ctx, item.ID, item.LeaseUntil, time.Now().UTC())
}
