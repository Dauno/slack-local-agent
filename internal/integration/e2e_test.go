package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

// E2E-01: Basic pipeline — authorized DM → runtime → publish → persist
func TestE2E_BasicPipeline(t *testing.T) {
	t.Parallel()
	deps := newE2EService(t)
	pub := deps.Publisher

	outcome, err := deps.Service.Handle(t.Context(), e2eDMInvocation("Ev-e2e-01", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("outcome = %q", outcome)
	}

	calls := pub.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("publisher calls = %d, want 1", len(calls))
	}
	if calls[0].text == "" {
		t.Fatal("published text is empty")
	}
}

// E2E-02: Confirmation approval — tool call → pending → approve → execute once → replay rejected
func TestE2E_ConfirmationApproval(t *testing.T) {
	t.Parallel()

	var toolCounter atomic.Int64
	deps := newE2EService(t,
		withLLM(newFakeLLMToolServer("demo_tool", `{"value":"test"}`, "Completed. The tool call was successful.")),
		withStaticTool(newE2EConfirmableTool(&toolCounter)),
	)
	pub := deps.Publisher

	// Step 1: Send a message that triggers a confirmation-producing tool call.
	outcome, err := deps.Service.Handle(t.Context(), e2eDMInvocation("Ev-conf-01", "execute the demo tool"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("first outcome = %q", outcome)
	}

	calls := pub.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 publish (confirmation prompt), got %d", len(calls))
	}

	// Extract wrapper call ID from the confirmation prompt.
	wrapperCallID := extractWrapperCallID(calls[0].text)
	if wrapperCallID == "" {
		t.Fatalf("could not extract wrapper call ID from: %q", calls[0].text)
	}
	if toolCounter.Load() != 0 {
		t.Fatalf("tool executed %d times before approval, want 0", toolCounter.Load())
	}

	// Step 2: Approve the confirmation (different timestamp for dedupe).
	approval := e2eDMInvocation("Ev-conf-02", "approve "+wrapperCallID)
	approval.EventTS = "1700000001.000001"
	outcome, err = deps.Service.Handle(t.Context(), approval)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("approval outcome = %q", outcome)
	}

	// Tool must have been executed exactly once.
	if toolCounter.Load() != 1 {
		t.Fatalf("tool executed %d times after approval, want 1", toolCounter.Load())
	}

	// Step 3: Replay the same approval — must be rejected (different timestamp for dedupe).
	replay := e2eDMInvocation("Ev-conf-03", "approve "+wrapperCallID)
	replay.EventTS = "1700000002.000001"
	outcome, err = deps.Service.Handle(t.Context(), replay)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeIgnoredFollowup {
		t.Fatalf("replay outcome = %q, want ignored_followup", outcome)
	}
	if toolCounter.Load() != 1 {
		t.Fatalf("tool executed %d times after replay, want 1", toolCounter.Load())
	}
}

// E2E-03: Confirmation rejection — tool call → pending → reject → not executed
func TestE2E_ConfirmationRejection(t *testing.T) {
	t.Parallel()

	var toolCounter atomic.Int64
	deps := newE2EService(t,
		withLLM(newFakeLLMToolServer("demo_tool", `{"value":"test"}`, "Rejected.")),
		withStaticTool(newE2EConfirmableTool(&toolCounter)),
	)
	pub := deps.Publisher

	// Step 1: Trigger confirmation.
	outcome, err := deps.Service.Handle(t.Context(), e2eDMInvocation("Ev-rej-01", "execute the demo tool"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("first outcome = %q", outcome)
	}

	calls := pub.Snapshot()
	wrapperCallID := extractWrapperCallID(calls[0].text)
	if wrapperCallID == "" {
		t.Fatalf("could not extract wrapper call ID from: %q", calls[0].text)
	}

	// Step 2: Reject (different timestamp for dedupe).
	rejection := e2eDMInvocation("Ev-rej-02", "reject "+wrapperCallID)
	rejection.EventTS = "1700000001.000001"
	outcome, err = deps.Service.Handle(t.Context(), rejection)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("rejection outcome = %q", outcome)
	}

	if toolCounter.Load() != 0 {
		t.Fatalf("tool executed %d times after rejection, want 0", toolCounter.Load())
	}
}

// E2E-04: Unauthorized user → denied
func TestE2E_UnauthorizedUser(t *testing.T) {
	t.Parallel()
	deps := newE2EService(t)
	pub := deps.Publisher

	outcome, err := deps.Service.Handle(t.Context(), e2eUnauthorizedInvocation("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeDenied {
		t.Fatalf("outcome = %q, want denied", outcome)
	}

	calls := pub.Snapshot()
	if len(calls) != 1 || calls[0].text != "denied" {
		t.Fatalf("expected denied message, got %#v", calls)
	}
}

// E2E-05: Duplicate message — same dedupe key returns duplicate once
func TestE2E_DuplicateMessage(t *testing.T) {
	t.Parallel()
	deps := newE2EService(t)
	pub := deps.Publisher

	// First message succeeds.
	outcome, err := deps.Service.Handle(t.Context(), e2eDMInvocation("Ev-dedup-01", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("first outcome = %q", outcome)
	}

	// Identical message (same EventID + same timestamp) → duplicate.
	outcome, err = deps.Service.Handle(t.Context(), e2eDMInvocation("Ev-dedup-01", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeDuplicate {
		t.Fatalf("duplicate outcome = %q, want duplicate", outcome)
	}

	// Only the first message was published.
	calls := pub.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("publisher calls = %d, want 1", len(calls))
	}
}

// E2E-06: Thread reply without prior bot participation → ignored
func TestE2E_ThreadReplyWithoutParticipation(t *testing.T) {
	t.Parallel()
	deps := newE2EService(t)
	pub := deps.Publisher

	// No history → no bot participation → ignored.
	outcome, err := deps.Service.Handle(t.Context(), e2eThreadInvocation("Ev-thr-01", "1700000000.000001", "follow up"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeIgnoredFollowup {
		t.Fatalf("outcome = %q, want ignored_followup", outcome)
	}

	calls := pub.Snapshot()
	if len(calls) != 0 {
		t.Fatalf("publisher calls = %d, want 0", len(calls))
	}
}

// E2E-07: Sandbox blocks restricted path (.env) and produces safe output
func TestE2E_SandboxBlocksRestrictedPath(t *testing.T) {
	t.Parallel()

	deps := newE2EService(t,
		withLLM(newFakeLLMToolServer("read_file", `{"project":"workspace","path":".env"}`, `The file .env is not available due to security restrictions.`)),
		withSandbox(),
	)
	pub := deps.Publisher

	outcome, err := deps.Service.Handle(t.Context(), e2eDMInvocation("Ev-sb-01", "read the .env file"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("outcome = %q", outcome)
	}

	calls := pub.Snapshot()
	if len(calls) != 1 {
		t.Fatalf("publisher calls = %d, want 1", len(calls))
	}
	text := calls[0].text
	if strings.Contains(text, "/") || strings.Contains(text, "\\") || strings.Contains(text, "SECRET") {
		t.Fatalf("published text contains host path or secret: %q", text)
	}
}

// E2E-08: Model error (HTTP 500) → OutcomeModelFailed
func TestE2E_ModelFailure(t *testing.T) {
	t.Parallel()
	deps := newE2EService(t, withLLM(newFakeLLMErrorServer()))
	pub := deps.Publisher

	outcome, err := deps.Service.Handle(t.Context(), e2eDMInvocation("Ev-err-01", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeModelFailed {
		t.Fatalf("outcome = %q, want model_failed", outcome)
	}

	calls := pub.Snapshot()
	if len(calls) != 1 || calls[0].text != "model error" {
		t.Fatalf("expected model error message, got %#v", calls)
	}
}

// E2E-09: Busy message — second concurrent call returns busy
func TestE2E_BusyMessage(t *testing.T) {
	t.Parallel()

	database := filepath.Join(t.TempDir(), "busy.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	blockingRuntime := &blockingRuntime{started: started, unblock: unblock}

	busyPub := &recordingPublisher{}
	confStore := adaptersqlite.NewConfirmationStore(store)
	busyService, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 1,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store:             store,
		Runtime:           blockingRuntime,
		Publisher:         busyPub,
		ConfirmationStore: confStore,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Start first invocation (blocks on runtime).
	firstDone := make(chan error, 1)
	go func() {
		o, e := busyService.Handle(t.Context(), e2eDMInvocation("Ev-busy-01", "first request"))
		if e == nil && o != botusecase.OutcomeResponded {
			e = fmt.Errorf("first outcome = %q", o)
		}
		firstDone <- e
	}()

	// Wait for the first call to reach the runtime.
	<-started

	// Second invocation with different event TS → per-conversation limiter → busy.
	second := e2eDMInvocation("Ev-busy-02", "second request")
	second.EventTS = "1700000001.000001"
	outcome, err := busyService.Handle(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeBusy {
		t.Fatalf("second outcome = %q, want busy", outcome)
	}

	busyCalls := busyPub.Snapshot()
	if len(busyCalls) != 1 || busyCalls[0].text != "busy" {
		t.Fatalf("expected busy message, got %#v", busyCalls)
	}

	// Unblock the first call.
	close(unblock)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

type blockingRuntime struct {
	started chan<- struct{}
	unblock <-chan struct{}
}

func (r *blockingRuntime) Run(_ context.Context, _ port.AgentRequest) (port.AgentTurn, error) {
	r.started <- struct{}{}
	<-r.unblock
	return port.AgentTurn{Text: "answer"}, nil
}

func (r *blockingRuntime) Resume(_ context.Context, _ domain.ConfirmationDecision) (port.AgentTurn, error) {
	return port.AgentTurn{Text: "resumed"}, nil
}

// E2E-10: Context survives restart — close/reopen SQLite, verify prior context
func TestE2E_ContextSurvivesRestart(t *testing.T) {
	t.Parallel()

	// Build service manually to control store lifetime.
	database := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}

	textLLM := newFakeLLMTextServer("first answer")
	t.Cleanup(textLLM.Close)

	llm, err := openaillm.New(
		openaillm.WithAPIKey("e2e-key"),
		openaillm.WithBaseURL(textLLM.URL+"/"),
		openaillm.WithModel("e2e-model"),
	)
	if err != nil {
		t.Fatal(err)
	}

	adkSession := adaptersqlite.NewAdkSessionService(store)
	toolFact := toolfactory.New(store, nil, nil)

	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName:      "Dev Agent",
		Instruction:    "You are a test assistant.",
		Model:          llm,
		SessionService: adkSession,
		ToolFactory:    toolFact,
	})
	if err != nil {
		t.Fatal(err)
	}

	pub1 := &recordingPublisher{}
	service1, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store:     store,
		Runtime:   runtime,
		Publisher: pub1,
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := service1.Handle(t.Context(), e2eDMInvocation("Ev-restart-01", "first question"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("first outcome = %q", outcome)
	}

	// Close and reopen.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })

	textLLM2 := newFakeLLMTextServer("second answer")
	t.Cleanup(textLLM2.Close)

	llm2, err := openaillm.New(
		openaillm.WithAPIKey("e2e-key"),
		openaillm.WithBaseURL(textLLM2.URL+"/"),
		openaillm.WithModel("e2e-model"),
	)
	if err != nil {
		t.Fatal(err)
	}

	adkSession2 := adaptersqlite.NewAdkSessionService(reopened)
	toolFact2 := toolfactory.New(reopened, nil, nil)

	runtime2, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName:      "Dev Agent",
		Instruction:    "You are a test assistant.",
		Model:          llm2,
		SessionService: adkSession2,
		ToolFactory:    toolFact2,
	})
	if err != nil {
		t.Fatal(err)
	}

	pub2 := &recordingPublisher{}
	service2, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store:     reopened,
		Runtime:   runtime2,
		Publisher: pub2,
	})
	if err != nil {
		t.Fatal(err)
	}

	second := e2eDMInvocation("Ev-restart-02", "second question")
	second.EventTS = "1700000001.000001"
	outcome, err = service2.Handle(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != botusecase.OutcomeResponded {
		t.Fatalf("second outcome = %q", outcome)
	}

	// Inspect model requests — second call should contain both prior exchanges.
	requests := textLLM2.requestsSnapshot()
	if len(requests) == 0 {
		t.Fatal("no model requests recorded after restart")
	}

	allText := ""
	for _, req := range requests {
		for _, msg := range req.Messages {
			allText += string(msg)
		}
	}
	if !strings.Contains(allText, "first question") || !strings.Contains(allText, "first answer") {
		t.Fatalf("restored context missing prior messages. raw: %q", allText)
	}
}

// --- helpers ---

func extractWrapperCallID(text string) string {
	prefix := "approve "
	idx := strings.Index(text, prefix)
	if idx < 0 {
		return ""
	}
	after := text[idx+len(prefix):]
	end := strings.Index(after, "`")
	if end < 0 {
		end = strings.IndexAny(after, " \n")
	}
	if end < 0 {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(after[:end])
}
