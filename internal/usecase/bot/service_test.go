package bot

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

func TestMemoryContextFitsExactRenderedUnicodeBudget(t *testing.T) {
	snippets := []domain.MemorySnippet{{Title: "Topic", Slug: "topic", RevisionNumber: 1, Content: "abcdef🚀"}}
	full := domain.RenderMemoryReference(snippets)
	result := domain.FitMemorySnippets(snippets, len([]rune(full))-1)
	if len(result) != 1 || result[0].Content != "abcdef" {
		t.Fatalf("FitMemorySnippets() = %#v", result)
	}
	if got := len([]rune(domain.RenderMemoryReference(result))); got > len([]rune(full))-1 {
		t.Fatalf("rendered memory has %d runes, exceeds budget", got)
	}
	if result := domain.FitMemorySnippets(snippets, 1); len(result) != 0 {
		t.Fatalf("FitMemorySnippets() with no room = %#v", result)
	}
}

type fakeStore struct {
	mu           sync.Mutex
	claimed      bool
	claimAll     bool
	claimCalls   int
	hasAssistant bool
	hasCalls     int
	recent       map[domain.ConversationKey][]domain.Message
	appended     []domain.Message
	appendedMeta []domain.ConversationMetadata
}

func (s *fakeStore) ClaimDedupe(context.Context, []string, time.Time, time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimAll {
		return true, nil
	}
	if s.claimed {
		return false, nil
	}
	s.claimed = true
	return true, nil
}
func (s *fakeStore) HasAssistantMessage(context.Context, domain.ConversationKey) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasCalls++
	return s.hasAssistant, nil
}
func (s *fakeStore) RecentMessages(_ context.Context, key domain.ConversationKey, _ int) ([]domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Message(nil), s.recent[key]...), nil
}
func (s *fakeStore) AppendMessage(_ context.Context, metadata domain.ConversationMetadata, message domain.Message, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appended = append(s.appended, message)
	s.appendedMeta = append(s.appendedMeta, metadata)
	return nil
}
func (*fakeStore) CleanupDedupe(context.Context, time.Time) error { return nil }

type fakeFileLoader struct {
	calls  int
	loaded port.LoadedAttachment
	err    error
}

func (l *fakeFileLoader) Load(context.Context, domain.Attachment, int64) (port.LoadedAttachment, error) {
	l.calls++
	return l.loaded, l.err
}

type fakeAttachmentProcessor struct {
	calls     int
	requests  []port.AttachmentRequest
	processed port.ProcessedAttachment
	err       error
}

func (p *fakeAttachmentProcessor) Process(_ context.Context, request port.AttachmentRequest) (port.ProcessedAttachment, error) {
	p.calls++
	p.requests = append(p.requests, request)
	return p.processed, p.err
}

type fakeRuntime struct {
	runTurn        port.AgentTurn
	resumeTurn     port.AgentTurn
	err            error
	runRequest     port.AgentRequest
	resumeDecision domain.ConfirmationDecision
	runCalls       int
	resumeCalls    int
	block          <-chan struct{}
	started        chan<- struct{}
	onRun          func()
}

func (r *fakeRuntime) Run(ctx context.Context, request port.AgentRequest) (port.AgentTurn, error) {
	r.runCalls++
	r.runRequest = request
	if r.onRun != nil {
		r.onRun()
	}
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return port.AgentTurn{}, ctx.Err()
		}
	}
	return r.runTurn, r.err
}

func (r *fakeRuntime) Resume(_ context.Context, decision domain.ConfirmationDecision) (port.AgentTurn, error) {
	r.resumeCalls++
	r.resumeDecision = decision
	return r.resumeTurn, r.err
}

type fakeConfirmationStore struct {
	delivery *port.ConfirmationDelivery
	pending  []port.ConfirmationDelivery
}

func (*fakeConfirmationStore) CreateDelivery(context.Context, port.ConfirmationDelivery) error {
	return nil
}
func (s *fakeConfirmationStore) MarkPublished(_ context.Context, wrapperCallID, correlationID, slackMessageTS, rendererMode string) error {
	if s.delivery != nil && s.delivery.WrapperCallID == wrapperCallID {
		s.delivery.Status = port.ConfirmationPublished
		s.delivery.CorrelationID = correlationID
		s.delivery.SlackMessageTS = slackMessageTS
		s.delivery.RendererMode = rendererMode
	}
	for index := range s.pending {
		if s.pending[index].WrapperCallID == wrapperCallID {
			s.pending[index].Status = port.ConfirmationPublished
			s.pending[index].CorrelationID = correlationID
			s.pending[index].SlackMessageTS = slackMessageTS
			s.pending[index].RendererMode = rendererMode
		}
	}
	return nil
}
func (s *fakeConfirmationStore) MarkConsumed(_ context.Context, _ string) error {
	s.delivery.Status = port.ConfirmationConsumed
	return nil
}
func (s *fakeConfirmationStore) RejectDelivery(_ context.Context, _ string) error {
	s.delivery.Status = port.ConfirmationRejected
	return nil
}
func (s *fakeConfirmationStore) GetByWrapperCallID(context.Context, string) (*port.ConfirmationDelivery, error) {
	return s.delivery, nil
}
func (s *fakeConfirmationStore) ListPending(context.Context) ([]port.ConfirmationDelivery, error) {
	return append([]port.ConfirmationDelivery(nil), s.pending...), nil
}
func (*fakeConfirmationStore) ExpireDeliveries(context.Context, time.Time) error { return nil }

type fakeConfirmationPublisher struct {
	published []port.ConfirmationDelivery
	updated   []port.ConfirmationDelivery
	recovered port.ConfirmationPublishedResult
	found     bool
	err       error
	updateErr error
}

func (p *fakeConfirmationPublisher) PublishConfirmation(_ context.Context, delivery port.ConfirmationDelivery) (port.ConfirmationPublishedResult, error) {
	p.published = append(p.published, delivery)
	if p.err != nil {
		return port.ConfirmationPublishedResult{}, p.err
	}
	return port.ConfirmationPublishedResult{SlackMessageTS: "1700000001.000001"}, nil
}

func (p *fakeConfirmationPublisher) RecoverConfirmation(context.Context, port.ConfirmationDelivery) (port.ConfirmationPublishedResult, bool, error) {
	return p.recovered, p.found, p.err
}

func (p *fakeConfirmationPublisher) UpdateConfirmation(_ context.Context, delivery port.ConfirmationDelivery, _ string) error {
	p.updated = append(p.updated, delivery)
	return p.updateErr
}

type fakeExchangeFinder struct {
	found bool
	seen  []port.AssistantExchangeIntent
}

func (f *fakeExchangeFinder) FindPublishedAssistantExchange(_ context.Context, intent port.AssistantExchangeIntent) (string, bool, error) {
	f.seen = append(f.seen, intent)
	return "1700000000.000001", f.found, nil
}

type fakeExchangeWriter struct {
	calls            int
	prepares         int
	structured       int
	published        int
	discards         int
	metadata         domain.ConversationMetadata
	message          domain.Message
	prepared         port.PreparedAssistantExchange
	err              error
	onAppend         func()
	publishedTS      string
	memoryEligible   bool
	presentationJSON string
}

func (w *fakeExchangeWriter) PrepareAssistantExchange(_ context.Context, metadata domain.ConversationMetadata, message domain.Message, _ int, memoryEligible bool) (port.PreparedAssistantExchange, error) {
	w.prepares++
	w.metadata = metadata
	w.message = message
	w.memoryEligible = memoryEligible
	if w.prepared.ID == "" {
		w.prepared = port.PreparedAssistantExchange{ID: "intent", CorrelationID: "intent-correlation"}
	}
	return w.prepared, nil
}

func (w *fakeExchangeWriter) PrepareStructuredAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, presentationJSON string, retain int, memoryEligible bool) (port.PreparedAssistantExchange, error) {
	w.structured++
	w.presentationJSON = presentationJSON
	return w.PrepareAssistantExchange(ctx, metadata, message, retain, memoryEligible)
}

func (w *fakeExchangeWriter) MarkAssistantExchangePublished(_ context.Context, _ string, assistantTS string) error {
	w.published++
	w.publishedTS = assistantTS
	return nil
}

func (w *fakeExchangeWriter) FinalizeAssistantExchange(_ context.Context, _ string) error {
	w.calls++
	if w.onAppend != nil {
		w.onAppend()
	}
	return w.err
}

func (w *fakeExchangeWriter) DiscardAssistantExchange(context.Context, string) error {
	w.discards++
	return nil
}
func (*fakeExchangeWriter) ReconcileAssistantExchanges(context.Context, port.AssistantExchangeFinder) error {
	return nil
}

type fakeRecall struct{ snippets []domain.MemorySnippet }

func (r fakeRecall) Recall(context.Context, string, string) ([]domain.MemorySnippet, error) {
	return append([]domain.MemorySnippet(nil), r.snippets...), nil
}

type fakeEnricher struct {
	context  domain.AgentContext
	err      error
	calls    int
	onEnrich func()
}

func (e *fakeEnricher) Enrich(context.Context, domain.Invocation) (domain.AgentContext, error) {
	e.calls++
	if e.onEnrich != nil {
		e.onEnrich()
	}
	return e.context, e.err
}

type fakeHistory struct {
	history port.History
	calls   int
}

func (h *fakeHistory) RecentHistory(context.Context, domain.Invocation, domain.ContextLimits) (port.History, error) {
	h.calls++
	return h.history, nil
}

type publishedCall struct {
	target domain.ReplyTarget
	text   string
}

type fakePublisher struct {
	mu        sync.Mutex
	calls     []publishedCall
	err       error
	onPublish func()
}

type fakeStandardExperience struct {
	operation         *domain.ProgressOperation
	states            []domain.ProgressState
	progressOut       []domain.ProgressState
	promptCalls       int
	promptClaim       bool
	incremental       *domain.IncrementalOperation
	createdText       string
	updatedText       string
	finalText         string
	interrupted       string
	incrementalStates []domain.IncrementalStatus
	createErr         error
}

func (f *fakeStandardExperience) CreateProgress(_ context.Context, operation domain.ProgressOperation) error {
	f.operation = &operation
	return nil
}
func (f *fakeStandardExperience) MarkProgressPublished(_ context.Context, _ string, messageTS string) error {
	f.operation.MessageTS = messageTS
	return nil
}
func (f *fakeStandardExperience) SetProgressState(_ context.Context, _ string, state domain.ProgressState, _ time.Time) error {
	f.states = append(f.states, state)
	if f.operation != nil {
		f.operation.State = state
	}
	return nil
}
func (f *fakeStandardExperience) ListRecoverableProgress(context.Context) ([]domain.ProgressOperation, error) {
	if f.operation == nil {
		return nil, nil
	}
	return []domain.ProgressOperation{*f.operation}, nil
}
func (f *fakeStandardExperience) FindWaitingProgress(context.Context, domain.ConversationKey) (*domain.ProgressOperation, error) {
	return f.operation, nil
}
func (f *fakeStandardExperience) ClaimSuggestedPrompts(context.Context, string, string, domain.ConversationKey, time.Time) (string, bool, error) {
	if f.promptClaim {
		return "prompts-1", false, nil
	}
	f.promptClaim = true
	return "prompts-1", true, nil
}
func (*fakeStandardExperience) MarkSuggestedPromptsPublished(context.Context, string, string, time.Time) error {
	return nil
}
func (f *fakeStandardExperience) PrepareIncremental(_ context.Context, operation domain.IncrementalOperation) error {
	f.incremental = &operation
	return nil
}
func (f *fakeStandardExperience) MarkIncrementalCreated(_ context.Context, _, messageTS string, _ time.Time) error {
	f.incremental.MessageTS = messageTS
	return nil
}
func (f *fakeStandardExperience) AdvanceIncremental(_ context.Context, _ string, status domain.IncrementalStatus, sequence int, digest string, _ time.Time) error {
	f.incrementalStates = append(f.incrementalStates, status)
	if f.incremental != nil {
		f.incremental.Status, f.incremental.Sequence, f.incremental.PrefixDigest = status, sequence, digest
	}
	return nil
}
func (*fakeStandardExperience) ListUnfinishedIncremental(context.Context) ([]domain.IncrementalOperation, error) {
	return nil, nil
}
func (f *fakeStandardExperience) PublishProgress(_ context.Context, _ domain.ReplyTarget, operation domain.ProgressOperation) (port.PublishedResponse, error) {
	f.progressOut = append(f.progressOut, operation.State)
	return port.PublishedResponse{LastMessageTS: "1700000001.000001"}, nil
}
func (f *fakeStandardExperience) UpdateProgress(_ context.Context, operation domain.ProgressOperation) error {
	f.progressOut = append(f.progressOut, operation.State)
	return nil
}
func (*fakeStandardExperience) RecoverProgress(context.Context, domain.ProgressOperation) (port.PublishedResponse, bool, error) {
	return port.PublishedResponse{}, false, nil
}
func (f *fakeStandardExperience) PublishSuggestedPrompts(context.Context, domain.ReplyTarget, string, []string) (port.PublishedResponse, error) {
	f.promptCalls++
	return port.PublishedResponse{LastMessageTS: "1700000001.000002"}, nil
}
func (f *fakeStandardExperience) CreateIncremental(_ context.Context, _ domain.ReplyTarget, operation domain.IncrementalOperation, text string) (port.PublishedResponse, error) {
	f.createdText = text
	f.incremental = &operation
	if f.createErr != nil {
		return port.PublishedResponse{}, f.createErr
	}
	return port.PublishedResponse{LastMessageTS: "1700000001.000003"}, nil
}
func (f *fakeStandardExperience) UpdateIncremental(_ context.Context, operation domain.IncrementalOperation, text string) error {
	f.incremental, f.updatedText = &operation, text
	return nil
}
func (f *fakeStandardExperience) FinalizeIncremental(_ context.Context, operation domain.IncrementalOperation, text, _ string) error {
	f.incremental, f.finalText = &operation, text
	return nil
}
func (f *fakeStandardExperience) InterruptIncremental(_ context.Context, operation domain.IncrementalOperation, text string) error {
	f.incremental, f.interrupted = &operation, text
	return nil
}
func (*fakeStandardExperience) RecoverIncremental(context.Context, domain.IncrementalOperation) (port.PublishedResponse, bool, error) {
	return port.PublishedResponse{}, false, nil
}

type fakeStreamingRuntime struct {
	events []port.AgentStreamEvent
}

func (r *fakeStreamingRuntime) Stream(_ context.Context, _ port.AgentRequest, yield func(port.AgentStreamEvent) bool) {
	for _, event := range r.events {
		if !yield(event) {
			return
		}
	}
}

type fakeStructuredPublisher struct {
	calls         int
	target        domain.ReplyTarget
	presentation  domain.Presentation
	validationErr error
	err           error
}

func (p *fakeStructuredPublisher) ValidateStructured(presentation domain.Presentation) error {
	if p.validationErr != nil {
		return p.validationErr
	}
	return domain.ValidatePresentation(presentation)
}

func (p *fakeStructuredPublisher) PublishStructured(_ context.Context, target domain.ReplyTarget, presentation domain.Presentation) (port.PublishedResponse, error) {
	p.calls++
	p.target = target
	p.presentation = presentation
	return port.PublishedResponse{LastMessageTS: "1700000002.000003"}, p.err
}

func (p *fakePublisher) Publish(_ context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishedCall{target, text})
	if p.onPublish != nil {
		p.onPublish()
	}
	return port.PublishedResponse{LastMessageTS: "1700000002.000003"}, p.err
}

type trackingModelCallLimiter struct {
	mu   sync.Mutex
	held bool
}

func (l *trackingModelCallLimiter) TryAcquire() (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return nil, false
	}
	l.held = true
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.held = false
		})
	}, true
}

func botInvocation() domain.Invocation {
	return domain.Invocation{
		EventID: "Ev1", EventType: "message.im", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: "1700000000.000001", Text: "hello", Trigger: domain.TriggerDirectMessage,
	}
}

func newTestService(t *testing.T, store *fakeStore, runtime *fakeRuntime, history *fakeHistory, publisher *fakePublisher, mutate func(*Config)) *Service {
	return newTestServiceWithConfirmations(t, store, runtime, history, publisher, nil, mutate)
}

func newTestServiceWithConfirmations(t *testing.T, store *fakeStore, runtime *fakeRuntime, history *fakeHistory, publisher *fakePublisher, confirmations port.ConfirmationDeliveryStore, mutate func(*Config)) *Service {
	t.Helper()
	cfg := Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	if runtime == nil {
		runtime = &fakeRuntime{runTurn: port.AgentTurn{Text: "default answer"}}
	}
	service, err := New(cfg, Dependencies{
		Store: store, Runtime: runtime, History: history, Publisher: publisher, ConfirmationStore: confirmations,
		Clock: fakeClock{now: time.Unix(1700000000, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestHandleAuthorizedDM(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	history := &fakeHistory{}
	publisher := &fakePublisher{}
	service := newTestService(t, store, runtime, history, publisher, nil)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Messages) != 1 || runtime.runRequest.Messages[0].Content != "hello" {
		t.Fatalf("unexpected model calls/context: %d %#v", runtime.runCalls, runtime.runRequest.Messages)
	}
	if len(store.appended) != 2 || store.appended[0].Role != domain.RoleUser || store.appended[1].Role != domain.RoleAssistant {
		t.Fatalf("unexpected persisted messages: %#v", store.appended)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].target.ThreadTS != "" || publisher.calls[0].text != "answer" {
		t.Fatalf("unexpected publishes: %#v", publisher.calls)
	}
}

func TestHandleStandardDMProgressReachesTerminalState(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	standard := &fakeStandardExperience{}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}, &fakeHistory{}, &fakePublisher{}, nil)
	service.cfg.ProgressEnabled = true
	service.standardStore = standard
	service.progressPublisher = standard
	invocation := botInvocation()
	invocation.ThreadedDM = true

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	want := []domain.ProgressState{domain.ProgressWorking, domain.ProgressFinalizing, domain.ProgressCleared}
	if !reflect.DeepEqual(standard.progressOut, want) || !reflect.DeepEqual(standard.states, []domain.ProgressState{domain.ProgressFinalizing, domain.ProgressCleared}) {
		t.Fatalf("visible=%v durable=%v", standard.progressOut, standard.states)
	}
}

func TestHandleStandardDMSuggestedPromptsOnlyOnceForRoot(t *testing.T) {
	store := &fakeStore{claimAll: true, recent: make(map[domain.ConversationKey][]domain.Message)}
	standard := &fakeStandardExperience{}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}, &fakeHistory{}, &fakePublisher{}, nil)
	service.cfg.PromptsEnabled = true
	service.cfg.SuggestedPrompts = []string{"Ask one thing"}
	service.standardStore = standard
	service.promptPublisher = standard
	first := botInvocation()
	first.ThreadedDM = true
	if outcome, err := service.Handle(t.Context(), first); err != nil || outcome != OutcomeResponded {
		t.Fatalf("first outcome=%q err=%v", outcome, err)
	}
	second := first
	second.EventID, second.EventTS = "Ev2", "1700000002.000002"
	if outcome, err := service.Handle(t.Context(), second); err != nil || outcome != OutcomeResponded {
		t.Fatalf("second outcome=%q err=%v", outcome, err)
	}
	if standard.promptCalls != 1 {
		t.Fatalf("prompt publishes=%d, want 1", standard.promptCalls)
	}
}

func TestHandleStreamingDMFinalizesOneIncrementalMessage(t *testing.T) {
	text := strings.Repeat("a", 200) + " done"
	stream := &fakeStreamingRuntime{events: []port.AgentStreamEvent{
		{Kind: port.AgentStreamTextDelta, TextDelta: strings.Repeat("a", 200)},
		{Kind: port.AgentStreamTextDelta, TextDelta: " done"},
		{Kind: port.AgentStreamCompleted, Turn: &port.AgentTurn{Text: text}},
	}}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	standard := &fakeStandardExperience{}
	regular := &fakePublisher{}
	service := newTestService(t, store, &fakeRuntime{}, &fakeHistory{}, regular, nil)
	service.cfg.StreamingEnabled = true
	service.cfg.UpdateInterval = 3 * time.Second
	service.cfg.StreamingCarryRunes = 128
	service.streamingRuntime = stream
	service.standardStore = standard
	service.incrementalPublisher = standard
	invocation := botInvocation()
	invocation.ThreadedDM = true

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if standard.createdText == "" || standard.finalText != text || standard.interrupted != "" || len(regular.calls) != 0 {
		t.Fatalf("standard=%#v regular=%#v", standard, regular.calls)
	}
	if len(store.appended) != 2 || store.appended[1].Role != domain.RoleAssistant || store.appended[1].Content != text {
		t.Fatalf("persisted=%#v", store.appended)
	}
}

func TestHandleStreamingErrorAfterVisibleOutputDoesNotPostReplacement(t *testing.T) {
	stream := &fakeStreamingRuntime{events: []port.AgentStreamEvent{
		{Kind: port.AgentStreamTextDelta, TextDelta: strings.Repeat("a", 200)},
		{Kind: port.AgentStreamError, Err: errors.New("stream disconnected")},
	}}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	standard := &fakeStandardExperience{}
	regular := &fakePublisher{}
	service := newTestService(t, store, &fakeRuntime{}, &fakeHistory{}, regular, nil)
	service.cfg.StreamingEnabled = true
	service.cfg.UpdateInterval = 3 * time.Second
	service.cfg.StreamingCarryRunes = 128
	service.streamingRuntime = stream
	service.standardStore = standard
	service.incrementalPublisher = standard
	invocation := botInvocation()
	invocation.ThreadedDM = true

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeModelFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if standard.createdText == "" || !strings.Contains(standard.interrupted, "Interrupted") || len(regular.calls) != 0 {
		t.Fatalf("standard=%#v regular=%#v", standard, regular.calls)
	}
}

func TestHandleAmbiguousIncrementalCreateDoesNotPostReplacement(t *testing.T) {
	stream := &fakeStreamingRuntime{events: []port.AgentStreamEvent{{Kind: port.AgentStreamTextDelta, TextDelta: strings.Repeat("a", 200)}}}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	standard := &fakeStandardExperience{createErr: errors.New("connection closed after acceptance")}
	regular := &fakePublisher{}
	service := newTestService(t, store, &fakeRuntime{}, &fakeHistory{}, regular, nil)
	service.cfg.StreamingEnabled = true
	service.cfg.UpdateInterval = 3 * time.Second
	service.cfg.StreamingCarryRunes = 128
	service.streamingRuntime = stream
	service.standardStore = standard
	service.incrementalPublisher = standard
	invocation := botInvocation()
	invocation.ThreadedDM = true

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomePublishFailed || len(regular.calls) != 0 {
		t.Fatalf("outcome=%q err=%v replacement=%#v", outcome, err, regular.calls)
	}
}

func TestHandleStructuredTurnSanitizesAndPersistsCanonicalFallback(t *testing.T) {
	const secret = "xoxb-sensitive-token"
	presentation := &domain.Presentation{
		FallbackMarkdown: "Results " + secret,
		Sources:          []domain.Source{{Text: "Docs " + secret, URL: "https://example.com/" + secret}},
		Table: &domain.Table{
			Caption: "Report " + secret,
			Headers: []string{"Name", "Value"},
			Rows:    [][]string{{"item", secret}},
		},
	}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{}
	markdownPublisher := &fakePublisher{}
	structuredPublisher := &fakeStructuredPublisher{}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Presentation: presentation}}, &fakeHistory{}, markdownPublisher, nil)
	service.structuredPublisher = structuredPublisher
	service.sanitize = func(value string) string { return strings.ReplaceAll(value, secret, "redacted") }
	service.AddMemory(nil, writer)
	if err := domain.ValidatePresentation(sanitizePresentation(*presentation, service.sanitize)); err != nil {
		t.Fatalf("test presentation is invalid: %v", err)
	}

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v structured_calls=%d markdown_calls=%#v", outcome, err, structuredPublisher.calls, markdownPublisher.calls)
	}
	if structuredPublisher.calls != 1 || len(markdownPublisher.calls) != 0 {
		t.Fatalf("structured calls=%d markdown calls=%#v", structuredPublisher.calls, markdownPublisher.calls)
	}
	if writer.structured != 1 || writer.message.Content != "Results redacted" || strings.Contains(writer.presentationJSON, secret) {
		t.Fatalf("structured exchange=%#v", writer)
	}
	var persisted domain.Presentation
	if err := json.Unmarshal([]byte(writer.presentationJSON), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.FallbackMarkdown != structuredPublisher.presentation.FallbackMarkdown || structuredPublisher.target.CorrelationID != "intent-correlation" {
		t.Fatalf("persisted=%#v published=%#v target=%#v", persisted, structuredPublisher.presentation, structuredPublisher.target)
	}
}

func TestHandleStructuredTurnRejectsMissingFallbackBeforeStructuredPublish(t *testing.T) {
	presentation := &domain.Presentation{Sources: []domain.Source{{Text: "Docs", URL: "https://example.com"}}}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	markdownPublisher := &fakePublisher{}
	structuredPublisher := &fakeStructuredPublisher{}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Presentation: presentation}}, &fakeHistory{}, markdownPublisher, nil)
	service.structuredPublisher = structuredPublisher

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeModelFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if structuredPublisher.calls != 0 || len(markdownPublisher.calls) != 1 || markdownPublisher.calls[0].text != "model error" {
		t.Fatalf("structured calls=%d markdown calls=%#v", structuredPublisher.calls, markdownPublisher.calls)
	}
}

func TestHandleStructuredTurnRunsSlackPreflightBeforePreparingExchange(t *testing.T) {
	presentation := &domain.Presentation{FallbackMarkdown: "Table", Table: &domain.Table{Headers: []string{"A"}, Rows: [][]string{{"1"}}}}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{}
	markdownPublisher := &fakePublisher{}
	structuredPublisher := &fakeStructuredPublisher{validationErr: errors.New("table exceeds Slack limits")}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Presentation: presentation}}, &fakeHistory{}, markdownPublisher, nil)
	service.structuredPublisher = structuredPublisher
	service.exchange = writer

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeModelFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 0 || structuredPublisher.calls != 0 || len(markdownPublisher.calls) != 1 {
		t.Fatalf("writer=%#v structured_calls=%d markdown_calls=%#v", writer, structuredPublisher.calls, markdownPublisher.calls)
	}
}

func TestHandleRuntimeReceivesCanonicalConversationKey(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "durable answer"}}
	service := newTestService(t, store, runtime, &fakeHistory{}, &fakePublisher{}, nil)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 1 || runtime.runRequest.ConversationKey != "slack:T12345678:dm:D12345678" {
		t.Fatalf("runtime request = %#v", runtime.runRequest)
	}
}

func TestHandleConfirmationBindsActorAndConversation(t *testing.T) {
	invocation := botInvocation()
	key, err := invocation.ConversationKey()
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	confirmations := &fakeConfirmationStore{delivery: &port.ConfirmationDelivery{
		WrapperCallID: "wrapper", OriginalCallID: "original", SessionID: "adk:" + string(key),
		Actor: invocation.UserID, ConversationKey: key, Status: port.ConfirmationPublished,
		Expiry: time.Now().Add(time.Hour),
	}}
	service := newTestServiceWithConfirmations(t, store, runtime, &fakeHistory{}, &fakePublisher{}, confirmations, nil)

	if outcome := service.HandleConfirmation(t.Context(), invocation, "wrapper", true); outcome != OutcomeResponded {
		t.Fatalf("HandleConfirmation() = %q", outcome)
	}
	if runtime.resumeCalls != 1 || runtime.resumeDecision.Actor != invocation.UserID || runtime.resumeDecision.ConversationKey != key {
		t.Fatalf("resume decision = %#v", runtime.resumeDecision)
	}

	confirmations.delivery.Status = port.ConfirmationPublished
	confirmations.delivery.ConversationKey = "slack:T12345678:dm:D99999999"
	if outcome := service.HandleConfirmation(t.Context(), invocation, "wrapper", true); outcome != OutcomeIgnoredFollowup {
		t.Fatalf("cross-conversation HandleConfirmation() = %q", outcome)
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("cross-conversation confirmation resumed %d times", runtime.resumeCalls)
	}
}

func richConfirmationDelivery(t *testing.T) port.ConfirmationDelivery {
	t.Helper()
	invocation := botInvocation()
	key, err := invocation.ConversationKey()
	if err != nil {
		t.Fatal(err)
	}
	return port.ConfirmationDelivery{
		WrapperCallID: "wrapper", OriginalCallID: "original", SessionID: "adk:" + string(key),
		Actor: invocation.UserID, TeamID: invocation.TeamID, ChannelID: invocation.ChannelID,
		ConversationKey: key, Summary: "Delete worktree", ParameterHash: "abc123",
		Status: port.ConfirmationPublished, CorrelationID: "confirmation:wrapper",
		SlackMessageTS: "1700000001.000001", RendererMode: confirmationRendererMode,
		Expiry: time.Unix(1700003600, 0),
	}
}

func richConfirmationAction(delivery port.ConfirmationDelivery) domain.ConfirmationInteractiveAction {
	return domain.ConfirmationInteractiveAction{
		WrapperCallID: delivery.WrapperCallID, ConversationKey: delivery.ConversationKey,
		Actor: delivery.Actor, TeamID: delivery.TeamID, ChannelID: delivery.ChannelID,
		MessageTS: delivery.SlackMessageTS, ThreadTS: delivery.ThreadTS,
		CorrelationID: delivery.CorrelationID, RendererMode: delivery.RendererMode,
		ContentSHA256: port.ConfirmationContentDigest(delivery), Approved: true,
	}
}

func TestHandleInteractiveConfirmationBindsPublishedMessageAndPublishesResult(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	confirmations := &fakeConfirmationStore{delivery: &delivery}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	publisher := &fakePublisher{}
	richPublisher := &fakeConfirmationPublisher{}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, publisher, confirmations, nil)
	service.confirmationPublisher = richPublisher
	action := richConfirmationAction(delivery)
	action.CorrelationID = ""
	action.RendererMode = ""
	action.ContentSHA256 = ""

	if err := service.HandleConfirmationInteractive(t.Context(), action); err != nil {
		t.Fatal(err)
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("resume calls = %d, want 1", runtime.resumeCalls)
	}
	if len(richPublisher.updated) != 1 || richPublisher.updated[0].Status != port.ConfirmationConsumed {
		t.Fatalf("terminal updates = %#v", richPublisher.updated)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "completed" || publisher.calls[0].target.ChannelID != delivery.ChannelID {
		t.Fatalf("result publishes = %#v", publisher.calls)
	}
}

func TestHandleInteractiveConfirmationRejectsSpoofedMessage(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	confirmations := &fakeConfirmationStore{delivery: &delivery}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, &fakePublisher{}, confirmations, nil)
	service.confirmationPublisher = &fakeConfirmationPublisher{}
	action := richConfirmationAction(delivery)
	action.MessageTS = "1700000009.000009"

	if err := service.HandleConfirmationInteractive(t.Context(), action); err == nil {
		t.Fatal("spoofed message interaction returned nil")
	}
	if runtime.resumeCalls != 0 {
		t.Fatalf("spoofed interaction resumed %d times", runtime.resumeCalls)
	}
}

func TestHandleInteractiveConfirmationUpdateFailureDoesNotReplayDecision(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	confirmations := &fakeConfirmationStore{delivery: &delivery}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	publisher := &fakePublisher{}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, publisher, confirmations, nil)
	service.confirmationPublisher = &fakeConfirmationPublisher{updateErr: errors.New("Slack update failed")}

	if err := service.HandleConfirmationInteractive(t.Context(), richConfirmationAction(delivery)); err != nil {
		t.Fatal(err)
	}
	if runtime.resumeCalls != 1 || delivery.Status != port.ConfirmationConsumed || len(publisher.calls) != 1 {
		t.Fatalf("resume calls = %d, status = %q, publishes = %#v", runtime.resumeCalls, delivery.Status, publisher.calls)
	}
	if err := service.HandleConfirmationInteractive(t.Context(), richConfirmationAction(delivery)); err == nil {
		t.Fatal("replayed interaction returned nil")
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("replayed interaction resumed %d times", runtime.resumeCalls)
	}
}

func TestHandleInteractiveConfirmationUpdatesExpiredPrompt(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	delivery.Expiry = time.Unix(1699999999, 0)
	action := richConfirmationAction(delivery)
	confirmations := &fakeConfirmationStore{delivery: &delivery}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	richPublisher := &fakeConfirmationPublisher{}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, &fakePublisher{}, confirmations, nil)
	service.confirmationPublisher = richPublisher

	if err := service.HandleConfirmationInteractive(t.Context(), action); err == nil {
		t.Fatal("expired interaction returned nil")
	}
	if runtime.resumeCalls != 0 || len(richPublisher.updated) != 1 || richPublisher.updated[0].Status != port.ConfirmationExpired {
		t.Fatalf("resume calls = %d, updates = %#v", runtime.resumeCalls, richPublisher.updated)
	}
}

func TestHandleInteractiveConfirmationChecksCurrentAuthorization(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	confirmations := &fakeConfirmationStore{delivery: &delivery}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, &fakePublisher{}, confirmations, func(cfg *Config) {
		cfg.AccessPolicy.AllowedUserIDs = []string{"U99999999"}
	})
	service.confirmationPublisher = &fakeConfirmationPublisher{}

	if err := service.HandleConfirmationInteractive(t.Context(), richConfirmationAction(delivery)); err == nil {
		t.Fatal("unauthorized interaction returned nil")
	}
	if runtime.resumeCalls != 0 {
		t.Fatalf("unauthorized interaction resumed %d times", runtime.resumeCalls)
	}
}

func TestHandleConfirmationRejectsTypedCommandForRichPrompt(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	confirmations := &fakeConfirmationStore{delivery: &delivery}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	publisher := &fakePublisher{}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, publisher, confirmations, nil)

	if outcome := service.HandleConfirmation(t.Context(), botInvocation(), delivery.WrapperCallID, true); outcome != OutcomeIgnoredFollowup {
		t.Fatalf("HandleConfirmation() = %q", outcome)
	}
	if runtime.resumeCalls != 0 || len(publisher.calls) != 1 || !strings.Contains(publisher.calls[0].text, "buttons") {
		t.Fatalf("runtime calls = %d, publishes = %#v", runtime.resumeCalls, publisher.calls)
	}
}

func TestReconcileRichConfirmationRecoversPublishedTimestamp(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	delivery.Status = port.ConfirmationPending
	delivery.SlackMessageTS = ""
	confirmations := &fakeConfirmationStore{pending: []port.ConfirmationDelivery{delivery}}
	richPublisher := &fakeConfirmationPublisher{
		recovered: port.ConfirmationPublishedResult{SlackMessageTS: "1700000001.000001"},
		found:     true,
	}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, &fakeRuntime{}, &fakeHistory{}, &fakePublisher{}, confirmations, nil)
	service.confirmationPublisher = richPublisher

	if err := service.ReconcileConfirmations(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(richPublisher.published) != 0 || confirmations.pending[0].SlackMessageTS != "1700000001.000001" || confirmations.pending[0].Status != port.ConfirmationPublished {
		t.Fatalf("reconciled delivery = %#v, publishes = %#v", confirmations.pending[0], richPublisher.published)
	}
}

func TestReconcileConfirmationsRepublishesOnlyUnprovenPendingDelivery(t *testing.T) {
	publisher := &fakePublisher{}
	confirmations := &fakeConfirmationStore{pending: []port.ConfirmationDelivery{{
		WrapperCallID: "wrapper", OriginalCallID: "original", ChannelID: "D12345678",
		Summary: "Delete worktree", Expiry: time.Now().Add(time.Hour), Status: port.ConfirmationPending,
	}}}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, &fakeRuntime{}, &fakeHistory{}, publisher, confirmations, nil)

	finder := &fakeExchangeFinder{}
	if err := service.ReconcileConfirmations(t.Context(), finder); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].target.CorrelationID != "confirmation:wrapper" {
		t.Fatalf("republished calls = %#v", publisher.calls)
	}
	if text := publisher.calls[0].text; !strings.Contains(text, "**Call ID**") || strings.Contains(text, "\n*Call ID*") {
		t.Fatalf("confirmation prompt is not standard Markdown: %q", text)
	}
	if confirmations.pending[0].Status != port.ConfirmationPublished {
		t.Fatalf("delivery status = %q", confirmations.pending[0].Status)
	}

	confirmations.pending[0].Status = port.ConfirmationPending
	finder.found = true
	if err := service.ReconcileConfirmations(t.Context(), finder); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 1 {
		t.Fatalf("proven confirmation was republished: %#v", publisher.calls)
	}
}

func TestHandleUsesAtomicAssistantExchangeWriterWhenMemoryEnabled(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{}
	publisher := &fakePublisher{}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}, &fakeHistory{}, publisher, nil)
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 1 || writer.published != 1 || writer.publishedTS != "1700000002.000003" || writer.calls != 1 || !writer.memoryEligible {
		t.Fatalf("atomic writer = %#v", writer)
	}
	if got := publisher.calls[0].target.CorrelationID; got != "intent-correlation" {
		t.Fatalf("prepared response correlation = %q", got)
	}
	if len(store.appended) != 1 || store.appended[0].Role != domain.RoleUser {
		t.Fatalf("assistant bypassed atomic writer: %#v", store.appended)
	}
}

func TestHandleAtomicExchangeDoesNotQueueMemoryWhenMemoryIsDisabled(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}, &fakeHistory{}, &fakePublisher{}, nil)
	service.exchange = writer

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 1 || writer.memoryEligible {
		t.Fatalf("memory-disabled exchange = %#v", writer)
	}
}

func TestHandleAttachmentBoundsCurrentTurnAndKeepsDurableDelivery(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	writer := &fakeExchangeWriter{}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.ContextLimits.MaxChars = 500
	})
	loader := &fakeFileLoader{loaded: port.LoadedAttachment{
		ID: "F00000001", Name: "notes.txt", MIMEType: "text/plain", Data: []byte("source"),
	}}
	processor := &fakeAttachmentProcessor{processed: port.ProcessedAttachment{
		Name: "notes.txt", MIMEType: "text/plain", Text: strings.Repeat("界", 500),
	}}
	service.fileLoader = loader
	service.attachmentProc = processor
	service.maxAttachmentBytes = 5 * 1024 * 1024
	service.maxAttachmentChars = 400
	service.AddMemory(nil, writer)

	invocation := botInvocation()
	invocation.Text = "review"
	invocation.Attachments = []domain.Attachment{{ID: "F00000001", Name: "notes.txt", MIMEType: "text/plain", Size: 6}}
	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if loader.calls != 1 || processor.calls != 1 || len(runtime.runRequest.Messages) != 1 {
		t.Fatalf("attachment flow loader=%d processor=%d messages=%#v", loader.calls, processor.calls, runtime.runRequest.Messages)
	}
	current := runtime.runRequest.Messages[0].Content
	if len([]rune(current)) > 500 || !strings.Contains(current, "[TRUNCATED:") || !strings.HasSuffix(current, "</attachments>") {
		t.Fatalf("bounded current turn (%d runes) = %q", len([]rune(current)), current)
	}
	if writer.prepares != 1 || writer.published != 1 || writer.calls != 1 || writer.memoryEligible {
		t.Fatalf("attachment durable exchange = %#v", writer)
	}
	if got := publisher.calls[0].target.CorrelationID; got != "intent-correlation" {
		t.Fatalf("attachment correlation ID = %q", got)
	}
	if len(store.appended) != 1 || store.appended[0].Content != "review" {
		t.Fatalf("persisted attachment turn = %#v", store.appended)
	}
}

func TestRenderAttachmentsKeepsFramingWithinUnicodeBudget(t *testing.T) {
	const maxChars = 360
	rendered, err := renderAttachments([]port.ProcessedAttachment{{
		Name: "x.txt", MIMEType: "text/plain", Text: strings.Repeat("🚀", 300),
	}}, maxChars)
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(rendered)); got > maxChars {
		t.Fatalf("rendered attachment has %d runes, max %d", got, maxChars)
	}
	if !strings.Contains(rendered, "</attachment>") || !strings.HasSuffix(rendered, "</attachments>") || !strings.Contains(rendered, "[TRUNCATED:") {
		t.Fatalf("rendered framing is incomplete: %q", rendered)
	}
}

func TestHandlePublishesOnlyAfterExchangeIntentIsPrepared(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{err: errors.New("database unavailable")}
	preparedAtPublish := false
	publisher := &fakePublisher{onPublish: func() { preparedAtPublish = writer.prepares == 1 }}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}, &fakeHistory{}, publisher, nil)
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err == nil || outcome != "" {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 1 || writer.published != 1 || writer.calls != 1 {
		t.Fatalf("exchange writer calls: prepares=%d published=%d finalizes=%d", writer.prepares, writer.published, writer.calls)
	}
	if !preparedAtPublish || len(publisher.calls) != 1 || publisher.calls[0].text != "answer" {
		t.Fatalf("Slack publish did not occur before injected finalization failure: %#v", publisher.calls)
	}
}

func TestHandleEnforcesCombinedMessageAndRenderedMemoryBudget(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	service := newTestService(t, store, runtime, &fakeHistory{}, &fakePublisher{}, func(cfg *Config) {
		cfg.ContextLimits.MaxChars = 500
	})
	service.AddMemory(fakeRecall{snippets: []domain.MemorySnippet{{Title: "Topic", RevisionNumber: 1, Content: strings.Repeat("é", 200)}}}, nil)
	if outcome, err := service.Handle(t.Context(), botInvocation()); err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if len(runtime.runRequest.Memory) != 1 {
		t.Fatalf("memory = %#v", runtime.runRequest.Memory)
	}
	if got := len([]rune(runtime.runRequest.Messages[0].Content)) + len([]rune(domain.RenderMemoryReference(runtime.runRequest.Memory))); got > 500 {
		t.Fatalf("combined model context has %d runes, exceeds 500", got)
	}
}

func TestHandleAuthorizedMentionRepliesInItsThread(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "channel answer"}}
	publisher := &fakePublisher{}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	invocation := botInvocation()
	invocation.EventType = "app_mention"
	invocation.ChannelID = "C12345678"
	invocation.ChannelKind = domain.ChannelPublic
	invocation.Trigger = domain.TriggerMention

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.calls) != 1 || publisher.calls[0].target.ChannelID != invocation.ChannelID || publisher.calls[0].target.ThreadTS != invocation.EventTS {
		t.Fatalf("mention response target=%#v", publisher.calls)
	}
}

func TestUnauthorizedClaimsDedupeButTouchesNoConversation(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}
	history := &fakeHistory{}
	publisher := &fakePublisher{}
	service := newTestService(t, store, runtime, history, publisher, func(cfg *Config) {
		cfg.AccessPolicy.AllowedUserIDs = []string{"U99999999"}
	})

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeDenied {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if store.claimCalls != 1 || store.hasCalls != 0 || len(store.appended) != 0 || runtime.runCalls != 0 || history.calls != 0 {
		t.Fatalf("unauthorized side effects: store=%#v runtime=%d history=%d", store, runtime.runCalls, history.calls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "denied" {
		t.Fatalf("unexpected denial publish: %#v", publisher.calls)
	}
	outcome, err = service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeDuplicate || len(publisher.calls) != 1 {
		t.Fatalf("duplicate denial outcome=%q err=%v publishes=%d", outcome, err, len(publisher.calls))
	}
}

func TestDuplicateHasNoVisibleOrModelEffect(t *testing.T) {
	store := &fakeStore{claimed: true, recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeDuplicate || runtime.runCalls != 0 || len(publisher.calls) != 0 {
		t.Fatalf("duplicate processing: outcome=%q err=%v calls=%d publishes=%d", outcome, err, runtime.runCalls, len(publisher.calls))
	}
}

func TestThreadFollowupCanRecoverParticipation(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	history := &fakeHistory{history: port.History{
		BotParticipated: true,
		Messages:        []domain.Message{{Role: domain.RoleAssistant, Content: "previous", ExternalTS: "1699999999.000001"}},
	}}
	publisher := &fakePublisher{}
	service := newTestService(t, store, runtime, history, publisher, nil)
	i := botInvocation()
	i.EventID, i.EventType, i.ChannelID, i.ChannelKind = "Ev2", "message.channels", "C12345678", domain.ChannelPublic
	i.EventTS, i.ThreadTS, i.Trigger = "1700000001.000002", "1700000000.000001", domain.TriggerThreadReply

	outcome, err := service.Handle(t.Context(), i)
	if err != nil || outcome != OutcomeResponded || history.calls != 1 {
		t.Fatalf("outcome=%q err=%v history=%d", outcome, err, history.calls)
	}
	if len(runtime.runRequest.Messages) != 2 || runtime.runRequest.Messages[0].Content != "previous" {
		t.Fatalf("recovered context not passed to model: %#v", runtime.runRequest.Messages)
	}
}

func TestModelFailureKeepsOnlyUserMessage(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{err: errors.New("upstream failed")}
	publisher := &fakePublisher{}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeModelFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if len(store.appended) != 1 || store.appended[0].Role != domain.RoleUser {
		t.Fatalf("unexpected persistence: %#v", store.appended)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "model error" {
		t.Fatalf("unexpected model error response: %#v", publisher.calls)
	}
}

func TestPublishFailureDoesNotPersistAssistant(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{err: errors.New("Slack unavailable")}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomePublishFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if len(store.appended) != 1 || store.appended[0].Role != domain.RoleUser {
		t.Fatalf("assistant was persisted after publish failure: %#v", store.appended)
	}
}

func TestPublishErrorRetainsPreparedExchangeForRecovery(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{}
	publisher := &fakePublisher{err: errors.New("connection closed after Slack accepted reply")}
	service := newTestService(t, store, &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}, &fakeHistory{}, publisher, nil)
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomePublishFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 1 || writer.discards != 0 || writer.published != 0 || writer.calls != 0 {
		t.Fatalf("prepared exchange was not retained for recovery: %#v", writer)
	}
}

func TestSharedModelPermitOnlyCoversModelCall(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	limiter := &trackingModelCallLimiter{}
	var modelPermitAvailable, publishReleased, persistReleased bool
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}, onRun: func() {
		_, modelPermitAvailable = limiter.TryAcquire()
	}}
	publisher := &fakePublisher{onPublish: func() {
		release, acquired := limiter.TryAcquire()
		publishReleased = acquired
		if acquired {
			release()
		}
	}}
	writer := &fakeExchangeWriter{onAppend: func() {
		release, acquired := limiter.TryAcquire()
		persistReleased = acquired
		if acquired {
			release()
		}
	}}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	service.modelCalls = limiter
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if modelPermitAvailable || !publishReleased || !persistReleased {
		t.Fatalf("shared permit states: modelPermitAvailable=%t publishReleased=%t persistReleased=%t", modelPermitAvailable, publishReleased, persistReleased)
	}
}

func TestEnrichmentRunsBeforeModelTimeoutAndSharedPermit(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	limiter := &trackingModelCallLimiter{}
	var enrichmentPermitAvailable, modelPermitAvailable bool
	enricher := &fakeEnricher{
		context: domain.AgentContext{MaxChars: 10, Facts: []domain.ContextFact{{Key: "k", Value: "v"}}},
	}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}, onRun: func() {
		_, modelPermitAvailable = limiter.TryAcquire()
	}}
	service := newTestService(t, store, runtime, &fakeHistory{}, &fakePublisher{}, func(cfg *Config) {
		cfg.ModelTimeout = 10 * time.Millisecond
	})
	service.modelCalls = limiter
	service.enricher = enricher
	enricher.onEnrich = func() {
		time.Sleep(20 * time.Millisecond)
		release, acquired := limiter.TryAcquire()
		enrichmentPermitAvailable = acquired
		if acquired {
			release()
		}
	}
	delayedRelease := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond)
		close(delayedRelease)
	}()
	runtime.block = delayedRelease

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if !enrichmentPermitAvailable || modelPermitAvailable || enricher.calls != 1 || len(runtime.runRequest.Context.Facts) != 1 {
		t.Fatalf("enrichment/model permit state: enrichment=%t model=%t calls=%d context=%#v", enrichmentPermitAvailable, modelPermitAvailable, enricher.calls, runtime.runRequest.Context)
	}
}

func TestSecretsAreSanitizedOnlyForPersistence(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer contains xoxb-sensitive-token"}}
	publisher := &fakePublisher{}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	service.sanitize = func(value string) string {
		return strings.ReplaceAll(value, "xoxb-sensitive-token", "xoxb-****oken")
	}
	invocation := botInvocation()
	invocation.Text = "inspect xoxb-sensitive-token"
	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if runtime.runRequest.Messages[0].Content != invocation.Text {
		t.Fatalf("model did not receive the authorized current message: %#v", runtime.runRequest.Messages)
	}
	for _, message := range store.appended {
		if strings.Contains(message.Content, "xoxb-sensitive-token") {
			t.Fatalf("raw secret persisted: %#v", store.appended)
		}
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.calls) != 1 || strings.Contains(publisher.calls[0].text, "xoxb-sensitive-token") {
		t.Fatalf("raw secret posted to Slack: %#v", publisher.calls)
	}
}

func TestHandleReturnsBusyWithoutPersistingOrQueueing(t *testing.T) {
	tests := []struct {
		name          string
		secondChannel string
		maxCalls      int
	}{
		{name: "global limit", secondChannel: "D87654321", maxCalls: 1},
		{name: "per conversation limit", secondChannel: "D12345678", maxCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{claimAll: true, recent: make(map[domain.ConversationKey][]domain.Message)}
			block := make(chan struct{})
			started := make(chan struct{}, 1)
			runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}, block: block, started: started}
			publisher := &fakePublisher{}
			service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
				cfg.MaxConcurrentCalls = tt.maxCalls
			})

			firstDone := make(chan error, 1)
			go func() {
				outcome, err := service.Handle(t.Context(), botInvocation())
				if err == nil && outcome != OutcomeResponded {
					err = errors.New("first invocation did not respond")
				}
				firstDone <- err
			}()
			<-started

			second := botInvocation()
			second.EventID = "Ev2"
			second.EventTS = "1700000001.000002"
			second.ChannelID = tt.secondChannel
			outcome, err := service.Handle(t.Context(), second)
			if err != nil || outcome != OutcomeBusy {
				t.Fatalf("second outcome=%q err=%v", outcome, err)
			}
			store.mu.Lock()
			persistedWhileBusy := len(store.appended)
			store.mu.Unlock()
			if persistedWhileBusy != 1 || runtime.runCalls != 1 {
				t.Fatalf("busy invocation persisted or invoked model: messages=%d model_calls=%d", persistedWhileBusy, runtime.runCalls)
			}
			publisher.mu.Lock()
			busyPublished := len(publisher.calls) == 1 && publisher.calls[0].text == "busy"
			publisher.mu.Unlock()
			if !busyPublished {
				t.Fatalf("configured busy response was not published")
			}

			close(block)
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}
