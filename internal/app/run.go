package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/adkartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/Dauno/slack-local-agent/internal/adapter/fssandbox"
	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/adapter/opencodemanager"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	memoryusecase "github.com/Dauno/slack-local-agent/internal/usecase/memory"
	opencodeusecase "github.com/Dauno/slack-local-agent/internal/usecase/opencode"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
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

	defs, err := agentdef.Load(paths.StateDir)
	if err != nil {
		return fmt.Errorf("load agent definitions: %w", err)
	}

	var (
		rootModel          model.LLM
		rootFamily         = domain.ProviderFamilyOpenAICompatible
		rootIsAgentCLI     bool
		preparedAgentTools []preparedAgentTool
		preparedWorkflows  []preparedWorkflowTool
		curatorLLM         memorycurator.LLM
		agentName          string
		rootDef            *agentdef.AgentDef
		curatorDef         *agentdef.AgentDef
		attachmentDef      *agentdef.AgentDef
		attachmentModel    model.LLM
		apiKey             string
		botToken           string
		appToken           string
		modelBaseURL       string
		redactionSecrets   []string
		redactor           secure.Redactor
		logger             *logging.Logger
	)
	describedCLIProviders := make(map[string]bool)
	openCodeCoordinator := opencodeusecase.NewCoordinator()

	if defs != nil {
		rootDefCandidate, ok := defs.Agents["root_agent"]
		if !ok {
			return errors.New("agent definition root_agent is required when declarative agents are configured")
		}
		rootDef = &rootDefCandidate
		if cur, ok := defs.Agents["memory_curator"]; ok {
			curatorDef = &cur
		}
		if attachment, ok := defs.Agents["attachment_analyzer"]; ok {
			attachmentDef = &attachment
		}

		providerEnvs := defs.RequiredAPIKeyEnvs()
		allKeys := make([]string, 0, len(providerEnvs)+2)
		allKeys = append(allKeys, providerEnvs...)
		allKeys = append(allKeys, bootstrap.SlackBotTokenEnv, bootstrap.SlackAppTokenEnv)
		values, err := envfile.NewResolver(paths.EnvFile).Resolve(allKeys...)
		if err != nil {
			return fmt.Errorf("load runtime secrets: %w", err)
		}
		botToken = values[bootstrap.SlackBotTokenEnv]
		appToken = values[bootstrap.SlackAppTokenEnv]
		if err := requiredSlackTokens(botToken, appToken); err != nil {
			return err
		}
		for _, environment := range providerEnvs {
			redactionSecrets = append(redactionSecrets, values[environment])
		}
		redactionSecrets = append(redactionSecrets, botToken, appToken)
		// Construct redaction before any child-process handshake can emit a
		// diagnostic containing inherited credentials.
		redactor = secure.NewRedactor(redactionSecrets...)
		logger = logging.New(a.logOutput, cfg.Runtime.LogLevel, redactor)

		resolved, err := defs.ResolveModel(rootDef.Model)
		if err != nil {
			return fmt.Errorf("resolve root agent model: %w", err)
		}
		builtRoot, rootSecret, err := newModelForResolved(ctx, resolved, values, cfg, paths, logger, redactor.String)
		if err != nil {
			return redactor.Error(fmt.Errorf("build root model client: %w", err))
		}
		if err := handshakeSelectedAgentCLI(ctx, resolved, builtRoot, describedCLIProviders); err != nil {
			return redactor.Error(fmt.Errorf("validate root agent model: %w", err))
		}
		rootModel = builtRoot
		if resolved.IsAgentCLI() {
			rootIsAgentCLI = true
			rootFamily = domain.ProviderFamilyAgentCLI
			modelBaseURL = "agent_cli:" + resolved.Provider.Name
		} else {
			apiKey = rootSecret
			modelBaseURL = resolved.BaseURL
		}
		preparedAgentTools, err = prepareRootAgentTools(ctx, defs, *rootDef, values, cfg, paths, logger, redactor.String, describedCLIProviders, func(resolved *agentdef.ResolvedModel) (port.ExternalAgentRuntime, error) {
			return acpclient.NewWithCoordinator(resolved.Command, resolved.Args, openCodeCoordinator), nil
		})
		if err != nil {
			return redactor.Error(err)
		}
		preparedWorkflows, err = prepareRootWorkflowTools(ctx, defs, *rootDef, values, cfg, paths, logger, redactor.String, describedCLIProviders, paths.StateDir, func(resolved *agentdef.ResolvedModel) (port.ExternalAgentRuntime, error) {
			return acpclient.NewWithCoordinator(resolved.Command, resolved.Args, openCodeCoordinator), nil
		}, openCodeCoordinator)
		if err != nil {
			return redactor.Error(err)
		}

		if cfg.Memory.Enabled {
			if curatorDef == nil {
				return errors.New("agent definition memory_curator is required when memory is enabled")
			}
			curatorResolved, err := defs.ResolveModel(curatorDef.Model)
			if err != nil {
				return fmt.Errorf("resolve curator model: %w", err)
			}
			curatorModel, _, err := newModelForResolved(ctx, curatorResolved, values, cfg, paths, logger, redactor.String)
			if err != nil {
				return redactor.Error(fmt.Errorf("build curator model client: %w", err))
			}
			if err := handshakeSelectedAgentCLI(ctx, curatorResolved, curatorModel, describedCLIProviders); err != nil {
				return redactor.Error(fmt.Errorf("validate curator model: %w", err))
			}
			curatorLLM = &memoryCuratorLLM{
				llm:                   curatorModel,
				generateContentConfig: curatorResolved.GenerateContentConfig,
			}
		}
		if attachmentDef != nil {
			attachmentResolved, err := defs.ResolveModel(attachmentDef.Model)
			if err != nil {
				return fmt.Errorf("resolve attachment analyzer model: %w", err)
			}
			if err := validateAttachmentModel(attachmentResolved); err != nil {
				return err
			}
			attachmentBuilt, _, err := newModelForResolved(ctx, attachmentResolved, values, cfg, paths, logger, redactor.String)
			if err != nil {
				return redactor.Error(fmt.Errorf("build attachment analyzer model client: %w", err))
			}
			attachmentModel = attachmentBuilt
		}

		agentName = rootDef.Name
	} else {
		values, err := envfile.NewResolver(paths.EnvFile).Resolve(
			cfg.Model.APIKeyEnv, bootstrap.SlackBotTokenEnv, bootstrap.SlackAppTokenEnv,
		)
		if err != nil {
			return fmt.Errorf("load runtime secrets: %w", err)
		}
		var secretErr error
		apiKey, botToken, appToken, secretErr = requiredSecrets(cfg, values)
		if secretErr != nil {
			return secretErr
		}
		redactionSecrets = append(redactionSecrets, apiKey, botToken, appToken)
		redactor = secure.NewRedactor(redactionSecrets...)
		logger = logging.New(a.logOutput, cfg.Runtime.LogLevel, redactor)
		legacyModel, err := newModel(cfg.Model, apiKey)
		if err != nil {
			return fmt.Errorf("build model client: %w", err)
		}
		rootModel = legacyModel
		agentName = cfg.Agent.Name
		modelBaseURL = cfg.Model.BaseURL
	}

	if concrete, ok := curatorLLM.(*memoryCuratorLLM); ok {
		concrete.logger = logger
		concrete.sanitize = redactor.String
	}

	if rootModel == nil {
		return redactor.Error(errors.New("model client not initialized"))
	}

	store, err := adaptersqlite.OpenExisting(ctx, paths.DatabaseFile)
	if err != nil {
		if errors.Is(err, adaptersqlite.ErrDatabaseNotFound) {
			return errors.New("Local state not found. Run: local-agent init")
		}
		if errors.Is(err, adaptersqlite.ErrFutureSchema) {
			return redactor.Error(fmt.Errorf("%w. Install a local-agent version that supports this database or back up and remove only the configured database file", err))
		}
		if errors.Is(err, adaptersqlite.ErrStateResetNeeded) {
			return redactor.Error(fmt.Errorf("%w. Run: local-agent init --reset-state", err))
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

	modelCalls := modelcalllimiter.New(cfg.Runtime.MaxConcurrentModelCalls)

	sdkLog := log.New(&redactingWriter{target: a.logOutput, redactor: redactor}, "slack: ", log.LstdFlags)
	var grantedSlackScopes string
	api := slackapi.New(
		botToken,
		slackapi.OptionAppLevelToken(appToken),
		slackapi.OptionLog(sdkLog),
		slackapi.OptionOnResponseHeaders(func(path string, headers http.Header) {
			if path == "auth.test" {
				grantedSlackScopes = headers.Get("X-OAuth-Scopes")
			}
		}),
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
	if cfg.Canvases.Enabled && !hasSlackScope(grantedSlackScopes, "canvases:write") {
		return errors.New("initialize Canvas support: Slack bot token is missing canvases:write; regenerate the manifest and reinstall the app")
	}

	slackTimeout := time.Duration(cfg.Runtime.SlackAPITimeoutSeconds) * time.Second
	publisher := slackadapter.NewPublisher(api, slackTimeout, logger, cfg.Slack.PartLabels)
	history := slackadapter.NewHistoryReader(api, auth.UserID, slackTimeout, logger, cfg.Slack.PartLabels)
	fileLoader := slackadapter.NewFileLoader(api, botToken, slackTimeout)
	confirmationPublisher := slackadapter.NewConfirmationPublisher(api, auth.UserID, slackTimeout, logger)
	blockPublisher := slackadapter.NewBlockPublisher(api, slackTimeout, logger)
	artifactSvc := artifact.InMemoryService()
	attachmentInstruction := ""
	attachmentTimeout := 120 * time.Second
	if attachmentDef != nil {
		attachmentInstruction = attachmentDef.Instruction
		if attachmentDef.TimeoutSeconds > 0 {
			attachmentTimeout = time.Duration(attachmentDef.TimeoutSeconds) * time.Second
		}
	}
	attachmentProc := adkartifact.NewProcessor(artifactSvc, attachmentModel, attachmentInstruction, attachmentTimeout, modelCalls)
	if err := store.ReconcileAssistantExchanges(ctx, history); err != nil {
		return redactor.Error(fmt.Errorf("reconcile assistant exchanges: %w", err))
	}

	contextEnricher := slackadapter.NewContextEnricherFromSDK(logger, api, slackadapter.ContextEnricherConfig{
		Enabled:              cfg.Slack.Context.Enabled,
		MaxChars:             cfg.Slack.Context.MaxChars,
		Timeout:              time.Duration(cfg.Slack.Context.TimeoutSeconds) * time.Second,
		ProfileCacheTTL:      time.Duration(cfg.Slack.Context.ProfileCacheTTLMinutes) * time.Minute,
		ConversationCacheTTL: time.Duration(cfg.Slack.Context.ConversationCacheTTLMinutes) * time.Minute,
	})

	sessionSvc := adaptersqlite.NewAdkSessionService(store)
	if sessionSvc == nil {
		return errors.New("initialize ADK session service: SQLite store is unavailable")
	}
	families, err := sessionSvc.RootSessionProviderFamilies(ctx)
	if err != nil {
		return redactor.Error(fmt.Errorf("inspect durable session provider families: %w", err))
	}
	if err := enforceProviderFamily(families, rootFamily); err != nil {
		return redactor.Error(err)
	}

	// A CLI-backed root receives no local-agent ADK function tools: the
	// external CLI cannot return portable ADK function calls.
	var toolFactory port.AgentToolFactory
	if !rootIsAgentCLI {
		var sandboxService *sandboxusecase.Service
		if cfg.Sandbox.Enabled {
			projects := paths.SandboxProjectRoots
			if len(projects) == 0 {
				projects = cfg.Sandbox.Projects
			}
			executor, err := fssandbox.New(projects, cfg.Sandbox.MaxOutputBytes)
			if err != nil {
				return redactor.Error(fmt.Errorf("initialize filesystem sandbox: %w", err))
			}
			sandboxService, err = sandboxusecase.New(sandboxusecase.Config{
				AllowedCapabilities: []domain.Capability{
					domain.CapListRepos, domain.CapListDirectory, domain.CapReadFile, domain.CapListWorktrees,
				},
				CommandTimeout: time.Duration(cfg.Sandbox.CommandTimeoutSeconds) * time.Second,
				MaxOutputBytes: cfg.Sandbox.MaxOutputBytes,
			}, sandboxusecase.Dependencies{AuditStore: adaptersqlite.NewSandboxAuditStore(store), Executor: executor})
			if err != nil {
				return redactor.Error(fmt.Errorf("initialize sandbox service: %w", err))
			}
		}
		var canvasService *canvasusecase.Service
		if cfg.Canvases.Enabled {
			canvasCreator := slackadapter.NewCanvasCreator(api, time.Duration(cfg.Canvases.TimeoutSeconds)*time.Second)
			canvasStore := adaptersqlite.NewCanvasOperationStore(store)
			canvasService, err = canvasusecase.New(canvasusecase.Config{
				MaxTitleChars:   cfg.Canvases.MaxTitleChars,
				MaxContentChars: cfg.Canvases.MaxContentChars,
				MaxContentBytes: cfg.Canvases.MaxContentBytes,
			}, canvasusecase.Dependencies{
				Creator:         canvasCreator,
				Store:           canvasStore,
				Logger:          logger,
				SanitizeContent: redactor.String,
			})
			if err != nil {
				return redactor.Error(fmt.Errorf("initialize canvas service: %w", err))
			}
		}
		toolFactory = toolfactory.New(store, sandboxService, canvasService)
		if len(preparedAgentTools) > 0 || len(preparedWorkflows) > 0 {
			globalInstruction := ""
			if rootDef != nil {
				globalInstruction = rootDef.GlobalInstruction
			}
			toolFactory = newCompositeAgentToolFactory(toolFactory, preparedAgentTools, preparedWorkflows, globalInstruction)
		}
		if defs != nil {
			if provider, exists := defs.Providers["opencode"]; exists && provider.Type == agentdef.ProviderTypeACP {
				resolved, resolveErr := defs.ResolveModel("opencode/smoke")
				if resolveErr != nil {
					return redactor.Error(fmt.Errorf("resolve OpenCode management profile: %w", resolveErr))
				}
				primaryPath, pathErr := managementProbePath(paths.SandboxProjectRoots)
				if pathErr != nil {
					return redactor.Error(pathErr)
				}
				toolFactory = &openCodeManagementToolFactory{
					base: toolFactory, runtime: acpclient.NewWithCoordinator(resolved.Command, resolved.Args, openCodeCoordinator),
					manager: opencodemanager.New(resolved.Command), allowedIDs: cfg.OpenCode.Management.AllowedUserIDs,
					primaryPath: primaryPath, configOptions: domainConfigOptions(resolved), coordinator: openCodeCoordinator,
				}
			}
		}
	}

	rtInstruction := ""
	rtGlobalInstruction := ""
	if rootDef != nil {
		rtInstruction = rootDef.Instruction
		rtGlobalInstruction = rootDef.GlobalInstruction
	}

	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName:         agentName,
		Instruction:       rtInstruction,
		GlobalInstruction: rtGlobalInstruction,
		SessionService:    sessionSvc,
		Model:             rootModel,
		ToolFactory:       toolFactory,
		ProviderFamily:    rootFamily,
	})
	if err != nil {
		return redactor.Error(fmt.Errorf("initialize ADK runtime: %w", err))
	}
	confirmationStore := adaptersqlite.NewConfirmationStore(store)

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
		Store: store, Runtime: runtime, History: history, Publisher: publisher, Logger: logger, Exchange: store,
		ModelCalls: modelCalls, SanitizeContent: redactor.String,
		Enricher:              contextEnricher,
		ConfirmationStore:     confirmationStore,
		ConfirmationPublisher: confirmationPublisher,
		StructuredPublisher:   blockPublisher,
		FileLoader:            fileLoader,
		AttachmentProc:        attachmentProc,
		MaxAttachmentBytes:    int64(cfg.Slack.Files.MaxBytesPerFile),
		MaxAttachmentChars:    cfg.Slack.Files.MaxProcessedChars,
	})
	if err != nil {
		return err
	}

	if err := confirmationStore.ExpireDeliveries(ctx, time.Now().UTC()); err != nil {
		logger.Warn("confirmation delivery expiry failed", "error", err)
	}
	if err := service.ReconcileConfirmations(ctx, history); err != nil {
		return redactor.Error(fmt.Errorf("reconcile confirmation deliveries: %w", err))
	}
	logger.Info("ADK durable runtime enabled", "session_service", "sqlite")

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

		curTimeout := time.Duration(cfg.Memory.CuratorTimeoutSeconds) * time.Second
		curatorInstruction := ""
		if curatorDef != nil {
			curatorInstruction = curatorDef.Instruction
			if curatorDef.TimeoutSeconds > 0 {
				curTimeout = time.Duration(curatorDef.TimeoutSeconds) * time.Second
			}
		}
		if curatorLLM == nil {
			curatorLLM = &memoryCuratorLLM{llm: rootModel, logger: logger, sanitize: redactor.String}
		}
		curator, curErr := memorycurator.New(curatorLLM, memorycurator.Config{
			Timeout: curTimeout, ModelCalls: modelCalls,
			Instruction: curatorInstruction,
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
	modelName := cfg.Model.Name
	if rootDef != nil {
		resolved, _ := defs.ResolveModel(rootDef.Model)
		if resolved != nil {
			modelName = resolved.Model
		}
	}
	logger.Info("local-agent starting",
		"agent", agentName,
		"model", modelName,
		"model_base_url", modelBaseURL,
		"database", paths.DatabaseFile,
		"allowed_users", len(cfg.Slack.AllowedUserIDs),
		"allow_all_users", cfg.Slack.AllowAllUsers,
		"max_concurrent_model_calls", cfg.Runtime.MaxConcurrentModelCalls,
	)
	if defs != nil {
		logger.Info("declarative agent definitions loaded",
			"providers", len(defs.Providers),
			"agents", len(defs.Agents))
	} else {
		logger.Info("using legacy config.Model; migrate to .local-agent/providers/ and .local-agent/agents/ for declarative model configuration")
	}
	listener.SetInteractiveHandler(func(ictx context.Context, action domain.ConfirmationInteractiveAction) error {
		return service.HandleConfirmationInteractive(ictx, action)
	})
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

func requiredSlackTokens(botToken, appToken string) error {
	if strings.TrimSpace(botToken) == "" {
		return errors.New("SLACK_BOT_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(botToken, "xoxb-") {
		return errors.New("SLACK_BOT_TOKEN must start with xoxb-. Run: local-agent doctor")
	}
	if strings.TrimSpace(appToken) == "" {
		return errors.New("SLACK_APP_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(appToken, "xapp-") {
		return errors.New("SLACK_APP_TOKEN must start with xapp-. Run: local-agent doctor")
	}
	return nil
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
