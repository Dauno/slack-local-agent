package adkagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// RuntimeConfig holds the dependencies for a durable ADK agent runtime.
type RuntimeConfig struct {
	AgentName         string
	Instruction       string
	GlobalInstruction string
	SessionService    session.Service
	Model             model.LLM
	ToolFactory       port.AgentToolFactory
	// StaticTools are reusable ADK tools composed at startup, such as AgentTool
	// wrappers. Invocation-scoped tools continue to come from ToolFactory.
	StaticTools []tool.Tool
	// ProviderFamily is stamped onto new durable sessions and compared
	// defensively before each turn. Empty defaults to openai_compatible.
	ProviderFamily string
}

// Runtime adapts ADK's llmagent + durable session service into the
// application's port.AgentRuntime boundary.
type Runtime struct {
	agentName         string
	instruction       string
	globalInstruction string
	sessionService    session.Service
	model             model.LLM
	toolFactory       port.AgentToolFactory
	staticTools       []tool.Tool
	providerFamily    string
}

var _ port.AgentRuntime = (*Runtime)(nil)
var _ port.StreamingAgentRuntime = (*Runtime)(nil)

// NewRuntime creates an ADK-backed agent runtime.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if strings.TrimSpace(cfg.AgentName) == "" {
		return nil, errors.New("agent name is required")
	}
	if strings.ContainsAny(cfg.AgentName, "\r\n\x00") {
		return nil, errors.New("agent name must be a single line")
	}
	if cfg.Model == nil {
		return nil, errors.New("ADK model is required")
	}
	if cfg.SessionService == nil {
		return nil, errors.New("session service is required")
	}
	providerFamily := cfg.ProviderFamily
	if providerFamily == "" {
		providerFamily = domain.ProviderFamilyOpenAICompatible
	}
	return &Runtime{
		agentName:         cfg.AgentName,
		instruction:       cfg.Instruction,
		globalInstruction: cfg.GlobalInstruction,
		sessionService:    cfg.SessionService,
		model:             cfg.Model,
		toolFactory:       cfg.ToolFactory,
		staticTools:       append([]tool.Tool(nil), cfg.StaticTools...),
		providerFamily:    providerFamily,
	}, nil
}

// adkSessionID derives a deterministic ADK session ID from a conversation key.
func adkSessionID(key domain.ConversationKey) string {
	return "adk:" + string(key)
}

// buildAgent constructs a per-turn llmagent with tools and before-model callback.
func (r *Runtime) buildAgent(tools []tool.Tool, ephemeral beforeModelData) (agent.Agent, error) {
	instruction := r.instruction
	if instruction == "" {
		instruction = BaseInstruction(r.agentName)
		if len(tools) > 0 {
			instruction += " You may use only the registered function tools when they are relevant. Their arguments and results remain subject to application policy."
		}
	}

	agentCfg := llmagent.Config{
		Name:              technicalName,
		Description:       "Slack conversational assistant with tools",
		Model:             r.model,
		Mode:              llmagent.ModeChat,
		IncludeContents:   llmagent.IncludeContentsDefault,
		GlobalInstruction: r.globalInstruction,
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return instruction, nil
		},
	}

	if len(tools) > 0 {
		agentCfg.Tools = tools
	}
	if reference := ephemeral.reference(); reference != "" {
		agentCfg.BeforeModelCallbacks = []llmagent.BeforeModelCallback{
			injectEphemeralReference(reference),
		}
	}

	return llmagent.New(agentCfg)
}

// Run executes one agent turn against the durable session.
func (r *Runtime) Run(ctx context.Context, req port.AgentRequest) (port.AgentTurn, error) {
	if strings.TrimSpace(string(req.ConversationKey)) == "" {
		return port.AgentTurn{}, errors.New("conversation key is required")
	}
	if len(req.Messages) == 0 {
		return port.AgentTurn{}, fmt.Errorf("%w: at least one message is required", ErrInvalidHistory)
	}
	if err := validateMessages(req.Messages); err != nil {
		return port.AgentTurn{}, err
	}
	current := req.Messages[len(req.Messages)-1]
	if current.Role != domain.RoleUser {
		return port.AgentTurn{}, fmt.Errorf("%w: final message must have user role", ErrInvalidHistory)
	}

	sessionID := adkSessionID(req.ConversationKey)

	// Ensure session exists (idempotent).
	_, err := r.ensureSession(ctx, sessionID)
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("ensure ADK session: %w", err)
	}

	// Preload ephemeral context (memory + Slack data) into the current
	// model call via before-model callback. They must not become durable events.
	ephemeralCtx := buildBeforeModelContext(req)

	// Build tools for this turn.
	tools := append([]tool.Tool(nil), r.staticTools...)
	if r.toolFactory != nil {
		rawTools, toolErr := r.toolFactory.ToolsForInvocation(current.UserID, req.ConversationKey)
		if toolErr != nil {
			return port.AgentTurn{}, fmt.Errorf("build invocation tools: %w", toolErr)
		}
		for _, raw := range rawTools {
			if t, ok := raw.(tool.Tool); ok {
				tools = append(tools, t)
			}
		}
	}

	agent, err := r.buildAgent(tools, ephemeralCtx)
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("build agent: %w", err)
	}

	adkRunner, err := runner.New(runner.Config{
		AppName:        applicationName,
		Agent:          agent,
		SessionService: r.sessionService,
	})
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("create runner: %w", err)
	}

	input := genai.NewContentFromText(current.Content, genai.RoleUser)

	turn, err := runTurn(ctx, adkRunner, input, sessionID, current.UserID, req.ConversationKey)
	if err != nil {
		return port.AgentTurn{}, err
	}
	return turn, nil
}

// Stream executes one turn with ADK SSE mode and exposes only typed model text
// deltas. Function calls and arguments remain inside ADK.
func (r *Runtime) Stream(ctx context.Context, req port.AgentRequest, yield func(port.AgentStreamEvent) bool) {
	terminalError := func(err error) {
		yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: err})
	}
	if yield == nil {
		return
	}
	if strings.TrimSpace(string(req.ConversationKey)) == "" {
		terminalError(errors.New("conversation key is required"))
		return
	}
	if len(req.Messages) == 0 {
		terminalError(fmt.Errorf("%w: at least one message is required", ErrInvalidHistory))
		return
	}
	if err := validateMessages(req.Messages); err != nil {
		terminalError(err)
		return
	}
	current := req.Messages[len(req.Messages)-1]
	if current.Role != domain.RoleUser {
		terminalError(fmt.Errorf("%w: final message must have user role", ErrInvalidHistory))
		return
	}
	sessionID := adkSessionID(req.ConversationKey)
	if _, err := r.ensureSession(ctx, sessionID); err != nil {
		terminalError(fmt.Errorf("ensure ADK session: %w", err))
		return
	}
	tools := append([]tool.Tool(nil), r.staticTools...)
	if r.toolFactory != nil {
		rawTools, err := r.toolFactory.ToolsForInvocation(current.UserID, req.ConversationKey)
		if err != nil {
			terminalError(fmt.Errorf("build invocation tools: %w", err))
			return
		}
		for _, raw := range rawTools {
			if t, ok := raw.(tool.Tool); ok {
				tools = append(tools, t)
			}
		}
	}
	agent, err := r.buildAgent(tools, buildBeforeModelContext(req))
	if err != nil {
		terminalError(fmt.Errorf("build agent: %w", err))
		return
	}
	adkRunner, err := runner.New(runner.Config{AppName: applicationName, Agent: agent, SessionService: r.sessionService})
	if err != nil {
		terminalError(fmt.Errorf("create runner: %w", err))
		return
	}
	runStreamingTurn(ctx, adkRunner, genai.NewContentFromText(current.Content, genai.RoleUser), sessionID, current.UserID, req.ConversationKey, yield)
}

// Resume continues a pending confirmation by sending the user's decision.
func (r *Runtime) Resume(ctx context.Context, decision domain.ConfirmationDecision) (port.AgentTurn, error) {
	if strings.TrimSpace(string(decision.ConversationKey)) == "" {
		return port.AgentTurn{}, errors.New("confirmation conversation key is required")
	}
	if strings.TrimSpace(decision.WrapperCallID) == "" || strings.TrimSpace(decision.OriginalCallID) == "" {
		return port.AgentTurn{}, errors.New("confirmation call IDs are required")
	}
	if strings.TrimSpace(decision.Actor) == "" {
		return port.AgentTurn{}, errors.New("confirmation actor is required")
	}
	sessionID := adkSessionID(decision.ConversationKey)
	if _, err := r.ensureSession(ctx, sessionID); err != nil {
		return port.AgentTurn{}, fmt.Errorf("ensure ADK session for resume: %w", err)
	}

	tools := append([]tool.Tool(nil), r.staticTools...)
	if r.toolFactory != nil {
		rawTools, toolErr := r.toolFactory.ToolsForInvocation(decision.Actor, decision.ConversationKey)
		if toolErr != nil {
			return port.AgentTurn{}, fmt.Errorf("build invocation tools for resume: %w", toolErr)
		}
		for _, raw := range rawTools {
			if t, ok := raw.(tool.Tool); ok {
				tools = append(tools, t)
			}
		}
	}
	agent, err := r.buildAgent(tools, beforeModelData{})
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("build agent for resume: %w", err)
	}

	adkRunner, err := runner.New(runner.Config{
		AppName:        applicationName,
		Agent:          agent,
		SessionService: r.sessionService,
	})
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("create runner for resume: %w", err)
	}

	payload := decision.Payload
	if payload == nil {
		payload = make(map[string]any)
	}

	resumeContent := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{
			{
				FunctionResponse: &genai.FunctionResponse{
					ID:   decision.WrapperCallID,
					Name: toolconfirmation.FunctionCallName,
					Response: map[string]any{
						"confirmed": decision.Approved,
						"payload":   payload,
					},
				},
			},
		},
	}

	turn, err := runTurn(ctx, adkRunner, resumeContent, sessionID, decision.Actor, decision.ConversationKey)
	if err != nil {
		return port.AgentTurn{}, err
	}
	return turn, nil
}

func (r *Runtime) ensureSession(ctx context.Context, sessionID string) (session.Session, error) {
	created, err := r.sessionService.Create(ctx, &session.CreateRequest{
		AppName:   applicationName,
		UserID:    ephemeralUserID,
		SessionID: sessionID,
		State: map[string]any{
			domain.ProviderFamilyStateKey: r.providerFamily,
		},
	})
	if err != nil {
		// Session may already exist from a previous turn or crash recovery.
		resp, getErr := r.sessionService.Get(ctx, &session.GetRequest{
			AppName:   applicationName,
			UserID:    ephemeralUserID,
			SessionID: sessionID,
		})
		if getErr != nil {
			return nil, fmt.Errorf("create session: %w (get also failed: %v)", err, getErr)
		}
		if familyErr := r.checkProviderFamily(resp.Session); familyErr != nil {
			return nil, familyErr
		}
		return resp.Session, nil
	}
	return created.Session, nil
}

// ErrProviderFamilyMismatch indicates a durable session created by a different
// provider family. Structured history is never flattened across families.
var ErrProviderFamilyMismatch = errors.New("durable session provider family mismatch")

// checkProviderFamily defensively re-validates the stored provider family
// before a turn. Sessions without the marker are legacy openai_compatible.
func (r *Runtime) checkProviderFamily(sess session.Session) error {
	if sess == nil {
		return nil
	}
	stored := domain.ProviderFamilyOpenAICompatible
	if value, err := sess.State().Get(domain.ProviderFamilyStateKey); err == nil {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			stored = text
		}
	}
	if stored != r.providerFamily {
		return fmt.Errorf("%w: session %q was created by provider family %q but %q is configured. Run: local-agent init --reset-state",
			ErrProviderFamilyMismatch, sess.ID(), stored, r.providerFamily)
	}
	return nil
}

// --- turn execution ---

func runTurn(ctx context.Context, adkRunner *runner.Runner, input *genai.Content, sessionID, actor string, key domain.ConversationKey) (port.AgentTurn, error) {
	var (
		finalText           string
		pendingConfirmation *domain.PendingConfirmation
	)

	for event, runErr := range adkRunner.Run(ctx, ephemeralUserID, sessionID, input, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if runErr != nil {
			return port.AgentTurn{}, fmt.Errorf("run ADK agent: %w", runErr)
		}
		if event == nil || event.Content == nil {
			continue
		}

		// Check for confirmation requests.
		for _, part := range event.Content.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
				pendingConfirmation = extractConfirmation(part.FunctionCall)
				if pendingConfirmation != nil {
					pendingConfirmation.Actor = actor
					pendingConfirmation.ConversationKey = key
				}
			}
		}

		if event.IsFinalResponse() && event.Content.Role == genai.RoleModel {
			text, _ := eventText(event.Content)
			if strings.TrimSpace(text) != "" {
				finalText = text
			}
		}
	}

	if strings.TrimSpace(finalText) == "" && pendingConfirmation == nil {
		return port.AgentTurn{}, ErrNoResponse
	}

	return port.AgentTurn{
		Text:                strings.TrimSpace(finalText),
		PendingConfirmation: pendingConfirmation,
	}, nil
}

func runStreamingTurn(ctx context.Context, adkRunner *runner.Runner, input *genai.Content, sessionID, actor string, key domain.ConversationKey, yield func(port.AgentStreamEvent) bool) {
	var (
		allText             strings.Builder
		partialText         strings.Builder
		pendingConfirmation *domain.PendingConfirmation
		lastFinalText       string
	)
	for event, runErr := range adkRunner.Run(ctx, ephemeralUserID, sessionID, input, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if runErr != nil {
			yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: fmt.Errorf("run streaming ADK agent: %w", runErr)})
			return
		}
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
				pendingConfirmation = extractConfirmation(part.FunctionCall)
				if pendingConfirmation != nil {
					pendingConfirmation.Actor = actor
					pendingConfirmation.ConversationKey = key
				}
			}
		}
		text, _ := eventText(event.Content)
		if event.Partial && event.Content.Role == genai.RoleModel && text != "" {
			if partialText.Len() == 0 && allText.Len() > 0 {
				allText.WriteString("\n\n")
			}
			partialText.WriteString(text)
			allText.WriteString(text)
			if !yield(port.AgentStreamEvent{Kind: port.AgentStreamTextDelta, TextDelta: text}) {
				return
			}
			continue
		}
		if !event.Partial && event.Content.Role == genai.RoleModel {
			if partialText.Len() > 0 {
				if text != "" && text != partialText.String() {
					yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: errors.New("streamed ADK text differs from its final aggregate")})
					return
				}
				partialText.Reset()
			} else if text != "" {
				if allText.Len() > 0 {
					allText.WriteString("\n\n")
				}
				allText.WriteString(text)
			}
		}
		if event.IsFinalResponse() && event.Content.Role == genai.RoleModel && text != "" {
			lastFinalText = text
		}
	}
	text := strings.TrimSpace(allText.String())
	if text == "" {
		text = strings.TrimSpace(lastFinalText)
	}
	turn := &port.AgentTurn{Text: text, PendingConfirmation: pendingConfirmation}
	if pendingConfirmation != nil {
		yield(port.AgentStreamEvent{Kind: port.AgentStreamPendingConfirmation, Turn: turn})
		return
	}
	if text == "" {
		yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: ErrNoResponse})
		return
	}
	yield(port.AgentStreamEvent{Kind: port.AgentStreamCompleted, Turn: turn})
}

func extractConfirmation(fc *genai.FunctionCall) *domain.PendingConfirmation {
	if fc == nil {
		return nil
	}
	originalCall, err := toolconfirmation.OriginalCallFrom(fc)
	if err != nil || originalCall == nil {
		return nil
	}

	// Compute a stable parameter hash.
	var paramHash string
	if originalCall.Args != nil {
		hash := sha256.New()
		encoded, _ := json.Marshal(originalCall.Args)
		hash.Write(encoded)
		paramHash = fmt.Sprintf("%x", hash.Sum(nil))[:16]
	}

	return &domain.PendingConfirmation{
		WrapperCallID:  fc.ID,
		OriginalCallID: originalCall.ID,
		Summary:        fmt.Sprintf("Tool %q requires confirmation", originalCall.Name),
		ParameterHash:  paramHash,
		Expiry:         time.Now().Add(15 * time.Minute),
	}
}

// --- ephemeral context (before-model callback) ---

type beforeModelData struct {
	memory  []domain.MemorySnippet
	context domain.AgentContext
}

func buildBeforeModelContext(req port.AgentRequest) beforeModelData {
	return beforeModelData{
		memory:  req.Memory,
		context: req.Context,
	}
}

func (d beforeModelData) reference() string {
	var parts []string

	contextRef := domain.RenderContextReference(d.context, d.context.MaxChars)
	if contextRef != "" {
		parts = append(parts, contextRef)
	}
	if len(d.memory) > 0 {
		memRef := domain.RenderMemoryReference(d.memory)
		if memRef != "" {
			parts = append(parts, memRef)
		}
	}

	return strings.Join(parts, "\n\n")
}

func injectEphemeralReference(reference string) llmagent.BeforeModelCallback {
	return func(_ agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
		if request == nil {
			return nil, errors.New("ADK model request is nil")
		}
		if request.Config == nil {
			request.Config = &genai.GenerateContentConfig{}
		}
		if request.Config.SystemInstruction == nil {
			request.Config.SystemInstruction = genai.NewContentFromText(reference, genai.RoleUser)
			return nil, nil
		}
		request.Config.SystemInstruction.Parts = append(
			request.Config.SystemInstruction.Parts,
			genai.NewPartFromText("\n\n"+reference),
		)
		return nil, nil
	}
}
