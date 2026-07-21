package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/fssandbox"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

// --- fakeLLMServer ---

type fakeLLMResponse struct {
	statusCode int
	text       string
	toolCalls  []fakeToolCall
}

type fakeToolCall struct {
	id        string
	name      string
	arguments string
}

type fakeLLMServer struct {
	*httptest.Server
	mu        sync.Mutex
	responses []fakeLLMResponse
	requests  []decodedLLMRequest
	callCount int
	blockCh   chan struct{}
	blocked   bool
}

func newFakeLLMTextServer(text string) *fakeLLMServer {
	s := &fakeLLMServer{
		responses: []fakeLLMResponse{{text: text}},
		blockCh:   make(chan struct{}),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.callCount++
		var body decodedLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			s.requests = append(s.requests, body)
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildChatCompletionResponse(fakeLLMResponse{text: text}))
	}))
	return s
}

func newFakeLLMToolServer(toolName, toolArgs, finalText string) *fakeLLMServer {
	s := &fakeLLMServer{
		blockCh: make(chan struct{}),
	}
	s.responses = []fakeLLMResponse{
		{toolCalls: []fakeToolCall{{id: "call_e2e_001", name: toolName, arguments: toolArgs}}},
		{text: finalText},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.callCount++
		var body decodedLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			s.requests = append(s.requests, body)
		}
		idx := s.callCount - 1
		if idx >= len(s.responses) {
			idx = len(s.responses) - 1
		}
		resp := s.responses[idx]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildChatCompletionResponse(resp))
	}))
	return s
}

func newFakeLLMErrorServer() *fakeLLMServer {
	s := &fakeLLMServer{
		responses: []fakeLLMResponse{{statusCode: http.StatusInternalServerError}},
		blockCh:   make(chan struct{}),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.callCount++
		var body decodedLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			s.requests = append(s.requests, body)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	return s
}

func newFakeLLMBlockingServer(started chan<- struct{}) *fakeLLMServer {
	blockCh := make(chan struct{})
	s := &fakeLLMServer{blockCh: blockCh, blocked: true}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.callCount++
		var body decodedLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			s.requests = append(s.requests, body)
		}
		s.mu.Unlock()
		if started != nil {
			started <- struct{}{}
		}
		<-blockCh
	}))
	return s
}

func (s *fakeLLMServer) Unblock() {
	if s.blocked {
		close(s.blockCh)
		s.blocked = false
	}
}

func (s *fakeLLMServer) requestsSnapshot() []decodedLLMRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]decodedLLMRequest, len(s.requests))
	copy(result, s.requests)
	return result
}

func buildChatCompletionResponse(resp fakeLLMResponse) map[string]any {
	result := map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "e2e-model",
	}
	if len(resp.toolCalls) > 0 {
		toolCalls := make([]any, 0, len(resp.toolCalls))
		for _, tc := range resp.toolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   tc.id,
				"type": "function",
				"function": map[string]any{
					"name":      tc.name,
					"arguments": tc.arguments,
				},
			})
		}
		result["choices"] = []any{
			map[string]any{
				"index":         0,
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": toolCalls,
				},
			},
		}
		return result
	}
	result["choices"] = []any{
		map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": resp.text,
			},
		},
	}
	return result
}

type decodedLLMRequest struct {
	Model    string            `json:"model,omitempty"`
	Messages []json.RawMessage `json:"messages,omitempty"`
}

// --- recordingPublisher ---

type recordingPublisher struct {
	mu    sync.Mutex
	calls []publishCall
}

type publishCall struct {
	target domain.ReplyTarget
	text   string
}

func (p *recordingPublisher) Publish(_ context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishCall{target, text})
	return port.PublishedResponse{LastMessageTS: "1700000010.000010"}, nil
}

func (p *recordingPublisher) Snapshot() []publishCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]publishCall, len(p.calls))
	copy(result, p.calls)
	return result
}

// --- fakeHistoryReader ---

type fakeHistoryReader struct {
	history port.History
}

func (h *fakeHistoryReader) RecentHistory(_ context.Context, _ domain.Invocation, _ domain.ContextLimits) (port.History, error) {
	return h.history, nil
}

// --- e2e sandbox ---

func newE2ESandbox(t *testing.T) (dir string, projects map[string]string) {
	t.Helper()
	dir = t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	mustWrite(t, filepath.Join(dir, "README.md"), "# Test\n")
	mustWrite(t, filepath.Join(dir, ".env"), "SECRET=value\n")
	must(os.MkdirAll(filepath.Join(dir, "subdir"), 0755))
	mustWrite(t, filepath.Join(dir, "subdir", "note.txt"), "hello\n")
	return dir, map[string]string{"workspace": dir}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// --- e2e service builder ---

type e2eServiceConfig struct {
	llmServer   *fakeLLMServer
	staticTools []tool.Tool
	useSandbox  bool
}

type e2eServiceOption func(*e2eServiceConfig)

func withStaticTool(t tool.Tool) e2eServiceOption {
	return func(cfg *e2eServiceConfig) {
		cfg.staticTools = append(cfg.staticTools, t)
	}
}

func withLLM(server *fakeLLMServer) e2eServiceOption {
	return func(cfg *e2eServiceConfig) {
		cfg.llmServer = server
	}
}

func withSandbox() e2eServiceOption {
	return func(cfg *e2eServiceConfig) {
		cfg.useSandbox = true
	}
}

type e2eDeps struct {
	Service    *botusecase.Service
	Publisher  *recordingPublisher
	Store      *adaptersqlite.Store
	SandboxDir string
}

func newE2EService(t *testing.T, opts ...e2eServiceOption) *e2eDeps {
	t.Helper()

	cfg := &e2eServiceConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.llmServer == nil {
		cfg.llmServer = newFakeLLMTextServer("Completed. The tool call was successful.")
	}
	t.Cleanup(cfg.llmServer.Close)

	// SQLite store.
	database := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// Publisher.
	publisher := &recordingPublisher{}

	// LLM adapter.
	llm, err := openaillm.New(
		openaillm.WithAPIKey("e2e-key"),
		openaillm.WithBaseURL(cfg.llmServer.URL+"/"),
		openaillm.WithModel("e2e-model"),
	)
	if err != nil {
		t.Fatal(err)
	}

	// ADK session service.
	adkSessionService := adaptersqlite.NewAdkSessionService(store)

	// Sandbox (optional).
	var sandboxService *sandboxusecase.Service
	var toolFact *toolfactory.Factory
	var sandboxDir string

	if cfg.useSandbox {
		sbDir, sbProjects := newE2ESandbox(t)
		sandboxDir = sbDir

		sandboxExecutor, err := fssandbox.New(sbProjects, 64*1024)
		if err != nil {
			t.Fatal(err)
		}

		sandboxAudit := adaptersqlite.NewSandboxAuditStore(store)
		sandboxService, err = sandboxusecase.New(
			sandboxusecase.Config{
				AllowedCapabilities: []domain.Capability{
					domain.CapListRepos, domain.CapListDirectory,
					domain.CapReadFile, domain.CapListWorktrees,
				},
				MaxOutputBytes: 64 * 1024,
			},
			sandboxusecase.Dependencies{
				AuditStore: sandboxAudit,
				Executor:   sandboxExecutor,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	toolFact = toolfactory.New(store, sandboxService, nil)

	// ADK Runtime.
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName:      "Dev Agent",
		Instruction:    "You are a test assistant. Answer concisely.",
		Model:          llm,
		SessionService: adkSessionService,
		ToolFactory:    toolFact,
		StaticTools:    cfg.staticTools,
		ProviderFamily: domain.ProviderFamilyOpenAICompatible,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Confirmation store.
	confirmationStore := adaptersqlite.NewConfirmationStore(store)

	// Bot service.
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store:             store,
		Runtime:           runtime,
		Publisher:         publisher,
		ConfirmationStore: confirmationStore,
	})
	if err != nil {
		t.Fatal(err)
	}

	return &e2eDeps{
		Service:    service,
		Publisher:  publisher,
		Store:      store,
		SandboxDir: sandboxDir,
	}
}

// --- e2e invocation helpers ---

func e2eDMInvocation(eventID, text string) domain.Invocation {
	return domain.Invocation{
		EventID: eventID, EventType: "message.im", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: "1700000000.000001", Text: text, Trigger: domain.TriggerDirectMessage,
	}
}

func e2eChannelInvocation(eventID, text string) domain.Invocation {
	return domain.Invocation{
		EventID: eventID, EventType: "app_mention", TeamID: "T12345678",
		ChannelID: "C12345678", ChannelKind: domain.ChannelPublic, UserID: "U12345678",
		EventTS: "1700000000.000001", Text: text, Trigger: domain.TriggerMention,
	}
}

func e2eThreadInvocation(eventID, threadTS, text string) domain.Invocation {
	return domain.Invocation{
		EventID: eventID, EventType: "message.channels", TeamID: "T12345678",
		ChannelID: "C12345678", ChannelKind: domain.ChannelPublic, UserID: "U12345678",
		EventTS: "1700000001.000002", ThreadTS: threadTS, Text: text,
		Trigger: domain.TriggerThreadReply,
	}
}

func e2eUnauthorizedInvocation(text string) domain.Invocation {
	return domain.Invocation{
		EventID: "Ev-unauth", EventType: "message.im", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, UserID: "U99999999",
		EventTS: "1700000000.000001", Text: text, Trigger: domain.TriggerDirectMessage,
	}
}

// --- demo tool for confirmation tests ---

type e2eDemoArgs struct {
	Value string `json:"value" jsonschema:"the value to process"`
}

type e2eDemoResult struct {
	Status string `json:"status"`
}

func newE2EConfirmableTool(counter *atomic.Int64) tool.Tool {
	t, err := functiontool.New(
		functiontool.Config{
			Name:                "demo_tool",
			Description:         "A demonstration tool that requires user confirmation before execution",
			RequireConfirmation: true,
		},
		func(ctx agent.Context, args e2eDemoArgs) (e2eDemoResult, error) {
			counter.Add(1)
			return e2eDemoResult{Status: "executed"}, nil
		},
	)
	if err != nil {
		panic(err)
	}
	return t
}

// --- confirmation delivery / Store helpers ---

// requirePublishCallCount fails if the publisher has not recorded exactly n calls.
func requirePublishCallCount(t *testing.T, pub *recordingPublisher, n int) []publishCall {
	t.Helper()
	calls := pub.Snapshot()
	if len(calls) != n {
		t.Fatalf("publisher calls = %d, want %d. calls: %#v", len(calls), n, calls)
	}
	return calls
}

// lookupConfirmationDelivery retrieves a confirmation delivery by wrapper call ID.
func lookupConfirmationDelivery(t *testing.T, store port.ConfirmationDeliveryStore, wrapperCallID string) *port.ConfirmationDelivery {
	t.Helper()
	delivery, err := store.GetByWrapperCallID(t.Context(), wrapperCallID)
	if err != nil {
		t.Fatalf("get confirmation delivery: %v", err)
	}
	return delivery
}

// createAndAssertDelivery is a convenience that creates an expectation from the Handle pipeline.
func createAndAssertDelivery(t *testing.T, store port.ConfirmationDeliveryStore, wrapperCallID string) *port.ConfirmationDelivery {
	t.Helper()
	delivery, err := store.GetByWrapperCallID(t.Context(), wrapperCallID)
	if err != nil {
		t.Fatalf("GetByWrapperCallID: %v", err)
	}
	if delivery == nil {
		t.Fatalf("confirmation delivery %q not found", wrapperCallID)
	}
	return delivery
}

// newDeliveryStore creates a dedicated SQLite-backed confirmation delivery store.
func newDeliveryStore(t *testing.T) port.ConfirmationDeliveryStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "confirm.db")
	store, err := adaptersqlite.Initialize(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return adaptersqlite.NewConfirmationStore(store)
}

// stubConfirmationDelivery creates a pending confirmation delivery in the given store.
func stubConfirmationDelivery(t *testing.T, store port.ConfirmationDeliveryStore, wrapperCallID, actor string, key domain.ConversationKey) {
	t.Helper()
	if err := store.CreateDelivery(t.Context(), port.ConfirmationDelivery{
		WrapperCallID:   wrapperCallID,
		OriginalCallID:  "orig-" + wrapperCallID,
		SessionID:       fmt.Sprintf("adk:%s", key),
		Actor:           actor,
		TeamID:          "T12345678",
		ChannelID:       "D12345678",
		ConversationKey: key,
		Summary:         "Test confirmation",
		ParameterHash:   "abc123",
		Status:          port.ConfirmationPublished,
		Expiry:          time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}
