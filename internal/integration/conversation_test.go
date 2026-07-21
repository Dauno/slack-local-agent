package integration_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

func TestConversationContextSurvivesRestartAndRemainsIsolated(t *testing.T) {
	database := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	firstRuntime := &recordingRuntime{responses: []string{"first answer"}}
	firstService := integrationService(t, store, firstRuntime)
	if outcome, err := firstService.Handle(t.Context(), dmInvocation("Ev1", "D12345678", "1700000000.000001", "first question")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("first outcome=%q err=%v", outcome, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	secondRuntime := &recordingRuntime{responses: []string{"second answer", "isolated answer"}}
	secondService := integrationService(t, reopened, secondRuntime)
	if outcome, err := secondService.Handle(t.Context(), dmInvocation("Ev2", "D12345678", "1700000001.000002", "second question")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("second outcome=%q err=%v", outcome, err)
	}
	if outcome, err := secondService.Handle(t.Context(), dmInvocation("Ev3", "D87654321", "1700000002.000003", "other conversation")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("isolated outcome=%q err=%v", outcome, err)
	}

	contexts := secondRuntime.contextsSnapshot()
	if len(contexts) != 2 {
		t.Fatalf("model contexts=%d", len(contexts))
	}
	want := []struct {
		role    domain.Role
		content string
	}{{domain.RoleUser, "first question"}, {domain.RoleAssistant, "first answer"}, {domain.RoleUser, "second question"}}
	if len(contexts[0]) != len(want) {
		t.Fatalf("restored context=%#v", contexts[0])
	}
	for index := range want {
		if contexts[0][index].Role != want[index].role || contexts[0][index].Content != want[index].content {
			t.Fatalf("restored context[%d]=%#v want=%#v", index, contexts[0][index], want[index])
		}
	}
	if len(contexts[1]) != 1 || contexts[1][0].Content != "other conversation" {
		t.Fatalf("conversation context leaked across DMs: %#v", contexts[1])
	}
}

func TestThreadedDMContextSurvivesRestartAndIsolatesRoots(t *testing.T) {
	database := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureDMIdentityMode(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	firstRuntime := &recordingRuntime{responses: []string{"first answer", "second answer"}}
	firstService := integrationService(t, store, firstRuntime)
	firstRoot := dmInvocation("Ev-thread-1", "D12345678", "1700000000.000001", "first root")
	firstRoot.ThreadedDM = true
	secondRoot := dmInvocation("Ev-thread-2", "D12345678", "1700000001.000002", "second root")
	secondRoot.ThreadedDM = true
	if outcome, err := firstService.Handle(t.Context(), firstRoot); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("first root outcome=%q err=%v", outcome, err)
	}
	if outcome, err := firstService.Handle(t.Context(), secondRoot); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("second root outcome=%q err=%v", outcome, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.EnsureDMIdentityMode(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	secondRuntime := &recordingRuntime{responses: []string{"continued answer"}}
	secondService := integrationService(t, reopened, secondRuntime)
	reply := dmInvocation("Ev-thread-3", "D12345678", "1700000002.000003", "continue first")
	reply.ThreadedDM = true
	reply.ThreadTS = firstRoot.EventTS
	if outcome, err := secondService.Handle(t.Context(), reply); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("reply outcome=%q err=%v", outcome, err)
	}

	contexts := secondRuntime.contextsSnapshot()
	if len(contexts) != 1 {
		t.Fatalf("model contexts=%d", len(contexts))
	}
	want := []string{"first root", "first answer", "continue first"}
	if len(contexts[0]) != len(want) {
		t.Fatalf("threaded context=%#v", contexts[0])
	}
	for index, content := range want {
		if contexts[0][index].Content != content {
			t.Fatalf("threaded context[%d]=%q, want %q", index, contexts[0][index].Content, content)
		}
	}
}

func integrationService(t *testing.T, store port.ConversationStore, runtime port.AgentRuntime) *botusecase.Service {
	t.Helper()
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20_000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{Store: store, Runtime: runtime, Publisher: integrationPublisher{}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func dmInvocation(eventID, channelID, timestamp, text string) domain.Invocation {
	return domain.Invocation{
		EventID: eventID, EventType: "message.im", TeamID: "T12345678",
		ChannelID: channelID, ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: timestamp, Text: text, Trigger: domain.TriggerDirectMessage,
	}
}

type recordingRuntime struct {
	mu        sync.Mutex
	responses []string
	contexts  [][]domain.Message
}

func (r *recordingRuntime) Run(_ context.Context, req port.AgentRequest) (port.AgentTurn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contexts = append(r.contexts, append([]domain.Message(nil), req.Messages...))
	response := r.responses[0]
	r.responses = r.responses[1:]
	return port.AgentTurn{Text: response}, nil
}

func (r *recordingRuntime) Resume(_ context.Context, _ domain.ConfirmationDecision) (port.AgentTurn, error) {
	return port.AgentTurn{}, nil
}

func (r *recordingRuntime) contextsSnapshot() [][]domain.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([][]domain.Message, len(r.contexts))
	for index := range r.contexts {
		result[index] = append([]domain.Message(nil), r.contexts[index]...)
	}
	return result
}

type integrationPublisher struct{}

func (integrationPublisher) Publish(_ context.Context, _ domain.ReplyTarget, _ string) (port.PublishedResponse, error) {
	return port.PublishedResponse{LastMessageTS: "1700000010.000010"}, nil
}
