package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	memoryusecase "github.com/Dauno/slack-local-agent/internal/usecase/memory"
)

func (a *Application) Run(ctx context.Context) error {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("Configuration not found. Run: local-agent init")
		}
		return fmt.Errorf("load runtime configuration: %w", err)
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(paths.StateDir)
	if errors.Is(statErr, os.ErrNotExist) {
		return errors.New("Local state not found. Run: local-agent init")
	}
	if statErr != nil {
		return fmt.Errorf("inspect configured state directory: %w. Run: local-agent doctor", statErr)
	}
	if !info.IsDir() {
		return errors.New("Configured state.dir is not a directory. Run: local-agent doctor")
	}

	values, err := envfile.NewResolver(paths.EnvFile).Resolve(
		cfg.Model.APIKeyEnv, bootstrap.SlackBotTokenEnv, bootstrap.SlackAppTokenEnv,
	)
	if err != nil {
		return fmt.Errorf("load runtime secrets: %w", err)
	}
	apiKey, botToken, appToken, err := requiredSecrets(cfg, values)
	if err != nil {
		return err
	}
	redactor := secure.NewRedactor(apiKey, botToken, appToken)
	logger := logging.New(a.logOutput, cfg.Runtime.LogLevel, redactor)

	store, err := adaptersqlite.OpenExisting(ctx, paths.DatabaseFile)
	if err != nil {
		if errors.Is(err, adaptersqlite.ErrDatabaseNotFound) {
			return errors.New("Local state not found. Run: local-agent init")
		}
		if errors.Is(err, adaptersqlite.ErrFutureSchema) {
			return redactor.Error(fmt.Errorf("%w. Install a local-agent version that supports this database or back up and remove only the configured database file", err))
		}
		return redactor.Error(fmt.Errorf("open runtime database: %w", err))
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			logger.Error("database close failed", "error", closeErr)
		}
	}()
	if err := store.CleanupDedupe(ctx, time.Now().UTC()); err != nil {
		return redactor.Error(err)
	}
	if cfg.Memory.Enabled {
		if err := store.CleanupOutbox(ctx, time.Now().UTC().AddDate(0, 0, -cfg.Memory.RetentionDays)); err != nil {
			return redactor.Error(err)
		}
	}

	llm, err := newModel(cfg.Model, apiKey)
	if err != nil {
		return redactor.Error(err)
	}
	agent, err := adkagent.New(cfg.Agent.Name, llm)
	if err != nil {
		return redactor.Error(err)
	}
	modelCalls := modelcalllimiter.New(cfg.Runtime.MaxConcurrentModelCalls)

	sdkLog := log.New(&redactingWriter{target: a.logOutput, redactor: redactor}, "slack: ", log.LstdFlags)
	api := slackapi.New(
		botToken,
		slackapi.OptionAppLevelToken(appToken),
		slackapi.OptionLog(sdkLog),
	)
	authCtx, cancelAuth := optionalTimeout(ctx, time.Duration(cfg.Runtime.SlackAPITimeoutSeconds)*time.Second)
	auth, err := api.AuthTestContext(authCtx)
	cancelAuth()
	if err != nil {
		return redactor.Error(fmt.Errorf("authenticate Slack bot: %w", err))
	}
	if auth == nil || auth.UserID == "" {
		return errors.New("authenticate Slack bot: Slack returned no bot user ID")
	}

	slackTimeout := time.Duration(cfg.Runtime.SlackAPITimeoutSeconds) * time.Second
	publisher := slackadapter.NewPublisher(api, slackTimeout, logger)
	history := slackadapter.NewHistoryReader(api, auth.UserID, slackTimeout, logger)
	if cfg.Memory.Enabled {
		if err := store.ReconcileAssistantExchanges(ctx, history); err != nil {
			return redactor.Error(fmt.Errorf("reconcile assistant exchanges: %w", err))
		}
	}

	contextEnricher := slackadapter.NewContextEnricherFromSDK(logger, api, slackadapter.ContextEnricherConfig{
		Enabled:              cfg.Slack.Context.Enabled,
		MaxChars:             cfg.Slack.Context.MaxChars,
		Timeout:              time.Duration(cfg.Slack.Context.TimeoutSeconds) * time.Second,
		ProfileCacheTTL:      time.Duration(cfg.Slack.Context.ProfileCacheTTLMinutes) * time.Minute,
		ConversationCacheTTL: time.Duration(cfg.Slack.Context.ConversationCacheTTLMinutes) * time.Minute,
	})

	service, err := botusecase.New(botusecase.Config{
		AccessPolicy: domain.AccessPolicy{
			AllowAllUsers: cfg.Slack.AllowAllUsers, AllowedUserIDs: cfg.Slack.AllowedUserIDs,
			AllowedTeamIDs: cfg.Slack.AllowedTeamIDs, AllowedChannelIDs: cfg.Slack.AllowedChannelIDs,
		},
		ContextLimits: domain.ContextLimits{
			MaxMessages: cfg.Context.MaxMessages, MaxChars: cfg.Context.MaxChars,
		},
		RetainMessages:      cfg.Context.RetainMessagesPerConversation,
		MaxConcurrentCalls:  cfg.Runtime.MaxConcurrentModelCalls,
		ModelTimeout:        time.Duration(cfg.Runtime.ModelTimeoutSeconds) * time.Second,
		BusyMessage:         cfg.Runtime.BusyMessage,
		ModelErrorMessage:   cfg.Runtime.ModelErrorMessage,
		UnauthorizedMessage: cfg.Slack.UnauthorizedMessage,
	}, botusecase.Dependencies{
		Store: store, Agent: agent, History: history, Publisher: publisher, Logger: logger,
		ModelCalls: modelCalls, SanitizeContent: redactor.String,
		Enricher: contextEnricher,
	})
	if err != nil {
		return err
	}

	// Wire the durable ADK runtime. When no tools are registered, it behaves
	// identically to the legacy agent but persists session history.
	sessionSvc := adaptersqlite.NewAdkSessionService(store)
	if sessionSvc != nil {
		runtime, rtErr := adkagent.NewRuntime(adkagent.RuntimeConfig{
			AgentName:      cfg.Agent.Name,
			SessionService: sessionSvc,
			Model:          llm,
		})
		if rtErr != nil {
			return redactor.Error(fmt.Errorf("initialize ADK runtime: %w", rtErr))
		}
		service.AddRuntime(runtime, adaptersqlite.NewConfirmationStore(store))
		logger.Info("ADK durable runtime enabled", "session_service", "sqlite")
	}

	if cfg.Memory.Enabled {
		memorySvc, memErr := memoryusecase.New(memoryusecase.Config{
			Recall: domain.MemoryRecallConfig{
				Enabled:   true,
				MaxTopics: cfg.Memory.MaxTopicsRecall,
				MaxChars:  cfg.Memory.MaxCharsRecall,
				Timeout:   time.Duration(cfg.Memory.RecallTimeoutSeconds) * time.Second,
			},
			Limits:      domain.MemoryLimits{MaxTopics: cfg.Memory.MaxTopics, MaxLinks: cfg.Memory.MaxLinks, MaxTopicChars: cfg.Memory.MaxTopicChars},
			MaxPatchOps: cfg.Memory.MaxPatchOps,
		}, memoryusecase.Dependencies{Store: store, Logger: logger, SanitizeContent: redactor.String})
		if memErr != nil {
			return redactor.Error(fmt.Errorf("initialize memory service: %w", memErr))
		}

		curatorLLM := &memoryCuratorLLM{llm: llm}
		curator, curErr := memorycurator.New(curatorLLM, memorycurator.Config{
			Timeout: time.Duration(cfg.Memory.CuratorTimeoutSeconds) * time.Second, ModelCalls: modelCalls,
		})
		if curErr != nil {
			return redactor.Error(fmt.Errorf("initialize memory curator: %w", curErr))
		}

		projector := memoryprojector.New()
		memoryDir := paths.MemoryDir

		go runMemoryCurator(ctx, store, history, curator, memorySvc, projector, memoryDir,
			time.Duration(cfg.Memory.WorkerIntervalSeconds)*time.Second,
			cfg.Memory.CuratorMaxRetries, cfg.Memory.RetentionDays, logger)

		service.AddMemory(memorySvc, store)
		logger.Info("memory service enabled", "directory", memoryDir,
			"max_topics_recall", cfg.Memory.MaxTopicsRecall,
			"max_chars_recall", cfg.Memory.MaxCharsRecall)
	}

	socket := socketmode.New(api, socketmode.OptionLog(sdkLog))
	listener := slackadapter.NewListener(socket, slackadapter.NewRouter(auth.UserID), logger)
	logger.Info("local-agent starting",
		"agent", cfg.Agent.Name,
		"model", cfg.Model.Name,
		"model_base_url", cfg.Model.BaseURL,
		"database", paths.DatabaseFile,
		"allowed_users", len(cfg.Slack.AllowedUserIDs),
		"allow_all_users", cfg.Slack.AllowAllUsers,
		"max_concurrent_model_calls", cfg.Runtime.MaxConcurrentModelCalls,
	)
	err = listener.Run(ctx, func(eventCtx context.Context, invocation domain.Invocation) {
		if _, handleErr := service.Handle(eventCtx, invocation); handleErr != nil {
			logger.Error("Slack invocation processing failed", "event_id", invocation.EventID, "error", handleErr)
		}
	})
	if err != nil {
		return redactor.Error(err)
	}
	logger.Info("local-agent stopped")
	return nil
}

func requiredSecrets(cfg config.Config, values map[string]string) (apiKey, botToken, appToken string, err error) {
	apiKey = values[cfg.Model.APIKeyEnv]
	botToken = values[bootstrap.SlackBotTokenEnv]
	appToken = values[bootstrap.SlackAppTokenEnv]
	if strings.TrimSpace(apiKey) == "" {
		return "", "", "", fmt.Errorf("%s is not configured. Run: local-agent init", cfg.Model.APIKeyEnv)
	}
	if strings.TrimSpace(botToken) == "" {
		return "", "", "", errors.New("SLACK_BOT_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(botToken, "xoxb-") {
		return "", "", "", errors.New("SLACK_BOT_TOKEN must start with xoxb-. Run: local-agent doctor")
	}
	if strings.TrimSpace(appToken) == "" {
		return "", "", "", errors.New("SLACK_APP_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(appToken, "xapp-") {
		return "", "", "", errors.New("SLACK_APP_TOKEN must start with xapp-. Run: local-agent doctor")
	}
	return apiKey, botToken, appToken, nil
}

func startsWithValue(value, prefix string) bool {
	return len(value) > len(prefix) && value[:len(prefix)] == prefix
}

func optionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

type redactingWriter struct {
	target   io.Writer
	redactor secure.Redactor
}

func (w *redactingWriter) Write(data []byte) (int, error) {
	if w == nil || w.target == nil {
		return len(data), nil
	}
	if _, err := io.WriteString(w.target, w.redactor.String(string(data))); err != nil {
		return 0, err
	}
	return len(data), nil
}

type memoryCuratorLLM struct {
	llm *openaillm.OpenAICompatibleLLM
}

func (m *memoryCuratorLLM) GenerateText(ctx context.Context, prompt string) (string, error) {
	request := &model.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromText(prompt, genai.RoleUser),
		},
	}
	var response string
	for resp, err := range m.llm.GenerateContent(ctx, request, false) {
		if err != nil {
			return "", err
		}
		if resp != nil && resp.Content != nil {
			for _, part := range resp.Content.Parts {
				if part != nil && part.Text != "" {
					response += part.Text
				}
			}
		}
	}
	return response, nil
}

func runMemoryCurator(
	ctx context.Context,
	store *adaptersqlite.Store,
	finder port.AssistantExchangeFinder,
	curator *memorycurator.Curator,
	memoryService *memoryusecase.Service,
	projector *memoryprojector.Projector,
	memoryDir string,
	interval time.Duration,
	maxRetries int,
	retentionDays int,
	logger port.Logger,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.ReconcileAssistantExchanges(ctx, finder); err != nil {
				logger.Warn("assistant exchange reconciliation failed", "error", err)
			}
			if err := store.CleanupOutbox(ctx, time.Now().UTC().AddDate(0, 0, -retentionDays)); err != nil {
				logger.Warn("memory outbox cleanup failed", "error", err)
			}
			processOutbox(ctx, store, curator, memoryService, projector, memoryDir, maxRetries, logger)
		}
	}
}

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
			if len(trusted) == 0 {
				logger.Warn("memory curator proposal failed", "item_id", item.ID, "error", err)
				retryOutbox(ctx, store, item, maxRetries, err)
				return
			}
			logger.Warn("memory curator proposal failed; applying trusted entity operations", "item_id", item.ID, "error", err)
			patch = domain.MemoryPatch{ConversationKey: item.ConversationKey, ExchangeTS: item.ExchangeTS}
		}
		patch.Operations = mergeTrustedEntityOperations(trusted, patch.Operations)
		for _, message := range messages {
			if message.Role == domain.RoleUser && message.UserID != "" {
				patch.SourceAuthorID = message.UserID
				break
			}
		}
		if err := memoryService.Validate(patch); err != nil && len(trusted) > 0 {
			logger.Warn("optional curator patch rejected; applying trusted entity operations", "item_id", item.ID, "error", err)
			patch.Operations = trusted
		}

		if _, applyErr := memoryService.ValidateAndApply(ctx, patch); applyErr != nil {
			logger.Warn("memory patch validation failed", "item_id", item.ID, "error", applyErr)
			retryOutbox(ctx, store, item, maxRetries, applyErr)
			return
		}

		if err := projector.Project(ctx, store, memoryDir); err != nil {
			logger.Error("memory projection failed", "error", err)
			retryOutbox(ctx, store, item, maxRetries, err) // Patch receipt makes projection retries safe.
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
