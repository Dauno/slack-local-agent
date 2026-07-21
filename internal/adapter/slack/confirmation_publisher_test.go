package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func newTestCallback(actionID, wrapperCallID, teamID, userID, channelID, ts, threadTS string) slackapi.InteractionCallback {
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		Team: slackapi.Team{ID: teamID},
		User: slackapi.User{ID: userID},
		Message: slackapi.Message{
			Msg: slackapi.Msg{
				Timestamp: ts, ThreadTimestamp: threadTS,
				Metadata: slackapi.SlackMetadata{
					EventType: confirmationMetadataEventType,
					EventPayload: map[string]any{
						"correlation_id": "confirmation:" + wrapperCallID,
						"render_mode":    confirmationRenderMode,
						"content_sha256": strings.Repeat("a", 64),
					},
				},
			},
		},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: actionID, Value: wrapperCallID},
			},
		},
	}
	cb.Channel.ID = channelID
	return cb
}

func newViewCallback() slackapi.InteractionCallback {
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission,
		Team: slackapi.Team{ID: "T12345678"},
		User: slackapi.User{ID: "U12345678"},
		Message: slackapi.Message{
			Msg: slackapi.Msg{Timestamp: "1720000001.000001"},
		},
	}
	cb.Channel.ID = "C12345678"
	return cb
}

func TestNormalizeInteractiveActionApprove(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "1720000000.000000")

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction returned false for valid approve")
	}
	if !action.Approved {
		t.Error("expected Approved=true")
	}
	if action.WrapperCallID != "wrapper-abc" {
		t.Errorf("WrapperCallID = %q, want %q", action.WrapperCallID, "wrapper-abc")
	}
	if action.Actor != "U12345678" {
		t.Errorf("Actor = %q, want %q", action.Actor, "U12345678")
	}
}

func TestNormalizeInteractiveActionReject(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(rejectActionID, "wrapper-xyz", "T12345678", "U12345678", "D12345678", "1720000001.000001", "")

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction returned false for valid reject")
	}
	if action.Approved {
		t.Error("expected Approved=false for reject")
	}
	if action.WrapperCallID != "wrapper-xyz" {
		t.Errorf("WrapperCallID = %q, want %q", action.WrapperCallID, "wrapper-xyz")
	}
}

func TestNormalizeInteractiveActionThreadedDMKey(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-dm", "T12345678", "U12345678", "D12345678", "1720000001.000001", "1720000000.000000")

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction returned false for threaded DM")
	}
	want := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1720000000.000000")
	if action.ConversationKey != want {
		t.Fatalf("ConversationKey = %q, want %q", action.ConversationKey, want)
	}
}

func TestNormalizeInteractiveActionUnknownActionID(t *testing.T) {
	t.Parallel()
	callback := newTestCallback("unknown.action", "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")

	_, ok := normalizeInteractiveAction(&callback)
	if ok {
		t.Error("normalizeInteractiveAction should return false for unknown action ID")
	}
}

func TestNormalizeInteractiveActionEmptyBlockActions(t *testing.T) {
	t.Parallel()
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		Team: slackapi.Team{ID: "T12345678"},
		User: slackapi.User{ID: "U12345678"},
		Message: slackapi.Message{
			Msg: slackapi.Msg{Timestamp: "1720000001.000001"},
		},
	}
	cb.Channel.ID = "C12345678"

	_, ok := normalizeInteractiveAction(&cb)
	if ok {
		t.Error("normalizeInteractiveAction should return false for nil block actions")
	}
}

func TestNormalizeInteractiveActionEmptyValue(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")

	_, ok := normalizeInteractiveAction(&callback)
	if ok {
		t.Error("normalizeInteractiveAction should return false for empty value")
	}
}

func TestNormalizeInteractiveActionWrongType(t *testing.T) {
	t.Parallel()
	callback := newViewCallback()

	_, ok := normalizeInteractiveAction(&callback)
	if ok {
		t.Error("normalizeInteractiveAction should return false for non-block-action type")
	}
}

func TestNormalizeInteractiveActionNilCallback(t *testing.T) {
	t.Parallel()
	_, ok := normalizeInteractiveAction(nil)
	if ok {
		t.Error("normalizeInteractiveAction should return false for nil callback")
	}
}

func TestNormalizeInteractiveActionAcceptsDocumentedPayloadWithoutMetadata(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	callback.Message.Metadata = slackapi.SlackMetadata{}

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction rejected Slack's documented payload without message metadata")
	}
	if action.CorrelationID != "" || action.RendererMode != "" || action.ContentSHA256 != "" {
		t.Fatalf("optional metadata fields = %#v", action)
	}
}

func TestNormalizeInteractiveActionRejectsConflictingContainer(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	callback.Container.MessageTs = "1720000002.000002"

	if _, ok := normalizeInteractiveAction(&callback); ok {
		t.Fatal("normalizeInteractiveAction accepted conflicting message timestamps")
	}
}

func TestRenderConfirmationBlocks(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		Summary: "Write file", CorrelationID: "confirmation:wrapper-abc",
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	blocks := renderConfirmationBlocks(delivery)
	if len(blocks) != 3 {
		t.Fatalf("renderConfirmationBlocks returned %d blocks, want 3", len(blocks))
	}
}

func TestConfirmationMetadata(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		Summary: "Write file", CorrelationID: "confirmation:wrapper-abc",
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	metadata := confirmationMetadata(delivery)
	if metadata.EventType != confirmationMetadataEventType {
		t.Errorf("metadata EventType = %q, want %q", metadata.EventType, confirmationMetadataEventType)
	}
}

func TestConfirmationContentDigestDeterministic(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		Summary: "Write file", Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	d1 := confirmationContentDigest(delivery)
	d2 := confirmationContentDigest(delivery)
	if d1 != d2 {
		t.Errorf("digest not deterministic: %q vs %q", d1, d2)
	}
	if d1 == "" {
		t.Error("digest is empty")
	}
}

func TestListenerIgnoresInteractiveWhenNoHandlerSet(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) {})
	}()

	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{Type: "interactive", EnvelopeID: "interactive-1"},
	}

	deadline := time.After(time.Second)
	for !client.wasAcked("interactive-1") {
		select {
		case <-deadline:
			t.Fatal("interactive envelope was not acknowledged")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

func TestListenerDispatchesInteractiveEvents(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	received := make(chan domain.ConfirmationInteractiveAction, 1)
	listener.SetInteractiveHandler(func(_ context.Context, action domain.ConfirmationInteractiveAction) error {
		received <- action
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) {})
	}()

	callback := newTestCallback(approveActionID, "wrapper-test", "T12345678", "U12345678", "C12345678", "1720000001.000001", "1720000000.000000")
	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{Type: "interactive", EnvelopeID: "interactive-dispatch"},
	}

	select {
	case action := <-received:
		if !action.Approved {
			t.Error("expected approved=true")
		}
		if action.WrapperCallID != "wrapper-test" {
			t.Errorf("WrapperCallID = %q", action.WrapperCallID)
		}
		if action.Actor != "U12345678" {
			t.Errorf("Actor = %q", action.Actor)
		}
	case <-time.After(time.Second):
		t.Fatal("interactive handler was not dispatched")
	}

	if !client.wasAcked("interactive-dispatch") {
		t.Fatal("interactive envelope was not acknowledged before dispatch")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

func TestNonBlockActionInteractiveIgnored(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	called := atomic.Bool{}
	listener.SetInteractiveHandler(func(_ context.Context, _ domain.ConfirmationInteractiveAction) error {
		called.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) {})
	}()

	callback := newTestCallback("other.action", "val", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{Type: "interactive", EnvelopeID: "interactive-ignored"},
	}

	deadline := time.After(time.Second)
	for !client.wasAcked("interactive-ignored") {
		select {
		case <-deadline:
			t.Fatal("interactive envelope was not acknowledged")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	time.Sleep(20 * time.Millisecond)
	if called.Load() {
		t.Error("handler should not be called for unknown action ID")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

type fakeConfirmationBlockClient struct {
	mu            sync.Mutex
	postedChans   []string
	postedBlocks  [][]slackapi.Block
	updatedChans  []string
	updatedTS     []string
	updatedBlocks [][]slackapi.Block
	fallbackTexts []string
	messages      []slackapi.Message
	hasMore       bool
	postErr       error
	updateErr     error
	historyErr    error
}

func (c *fakeConfirmationBlockClient) PostBlocks(_ context.Context, channelID, fallbackText string, blocks []slackapi.Block, _ slackapi.SlackMetadata, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.postErr != nil {
		return "", c.postErr
	}
	c.postedChans = append(c.postedChans, channelID)
	c.postedBlocks = append(c.postedBlocks, blocks)
	c.fallbackTexts = append(c.fallbackTexts, fallbackText)
	return fmt.Sprintf("172000000%d.000001", len(c.postedChans)), nil
}

func (c *fakeConfirmationBlockClient) ConfirmationMessages(_ context.Context, _, _ string, _ int) ([]slackapi.Message, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slackapi.Message(nil), c.messages...), c.hasMore, c.historyErr
}

func (c *fakeConfirmationBlockClient) UpdateBlocks(_ context.Context, channelID, messageTS string, blocks []slackapi.Block, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updatedChans = append(c.updatedChans, channelID)
	c.updatedTS = append(c.updatedTS, messageTS)
	c.updatedBlocks = append(c.updatedBlocks, blocks)
	return nil
}

func TestConfirmationPublisherPublish(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		ChannelID: "C12345678", Summary: "Write file",
		CorrelationID: "confirmation:wrapper-abc",
		Expiry:        time.Now().Add(15 * time.Minute),
	}

	result, err := pub.PublishConfirmation(context.Background(), delivery)
	if err != nil {
		t.Fatalf("PublishConfirmation() error = %v", err)
	}
	if result.SlackMessageTS == "" {
		t.Error("SlackMessageTS is empty")
	}
	if len(client.postedChans) != 1 || client.postedChans[0] != "C12345678" {
		t.Errorf("posted channel = %v, want [C12345678]", client.postedChans)
	}
	if len(client.fallbackTexts) != 1 || !strings.Contains(client.fallbackTexts[0], "Write file") {
		t.Fatalf("accessible fallback = %q", client.fallbackTexts)
	}
}

func TestConfirmationPublisherUpdate(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		ChannelID: "C12345678", Summary: "Write file",
		SlackMessageTS: "1720000001.000001",
		Status:         port.ConfirmationConsumed,
	}

	err := pub.UpdateConfirmation(context.Background(), delivery, "done")
	if err != nil {
		t.Fatalf("UpdateConfirmation() error = %v", err)
	}
	if len(client.updatedChans) != 1 || client.updatedChans[0] != "C12345678" {
		t.Errorf("updated channel = %v", client.updatedChans)
	}
	if client.updatedTS[0] != "1720000001.000001" {
		t.Errorf("updated timestamp = %q", client.updatedTS[0])
	}
}

func TestConfirmationPublisherUpdateNonTerminalStatus(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID:  "wrapper-abc",
		ChannelID:      "C12345678",
		SlackMessageTS: "1720000001.000001",
		Status:         port.ConfirmationPending,
	}

	err := pub.UpdateConfirmation(context.Background(), delivery, "text")
	if err == nil {
		t.Fatal("UpdateConfirmation should fail for non-terminal status")
	}
}

func TestConfirmationPublisherPublishError(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{postErr: errors.New("slack down")}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", ChannelID: "C12345678",
		Expiry: time.Now().Add(15 * time.Minute),
	}
	_, err := pub.PublishConfirmation(context.Background(), delivery)
	if err == nil {
		t.Fatal("PublishConfirmation should return error")
	}
}

func TestConfirmationPublisherNilClient(t *testing.T) {
	t.Parallel()
	pub := newConfirmationPublisher(nil, "U99999999", 5*time.Second, nil)
	delivery := port.ConfirmationDelivery{ChannelID: "C12345678", Expiry: time.Now()}
	_, err := pub.PublishConfirmation(context.Background(), delivery)
	if err == nil {
		t.Fatal("PublishConfirmation with nil client should error")
	}
}

func TestConfirmationPublisherRecoversMatchingPrompt(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "C12345678", ThreadTS: "1720000000.000000",
		Summary: "Write file", ParameterHash: "abc123", CorrelationID: "confirmation:wrapper-abc",
		RendererMode: confirmationRenderMode, Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	client := &fakeConfirmationBlockClient{messages: []slackapi.Message{{Msg: slackapi.Msg{
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: confirmationMetadata(delivery),
	}}}}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	result, found, err := pub.RecoverConfirmation(t.Context(), delivery)
	if err != nil || !found || result.SlackMessageTS != "1720000001.000001" {
		t.Fatalf("RecoverConfirmation() = %#v, %t, %v", result, found, err)
	}
}

func TestConfirmationPublisherRecoveryFailsClosed(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "D12345678", Summary: "Write file",
		CorrelationID: "confirmation:wrapper-abc", RendererMode: confirmationRenderMode,
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	metadata := confirmationMetadata(delivery)
	metadata.EventPayload["content_sha256"] = strings.Repeat("0", 64)
	client := &fakeConfirmationBlockClient{messages: []slackapi.Message{{Msg: slackapi.Msg{
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: metadata,
	}}}}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	if _, _, err := pub.RecoverConfirmation(t.Context(), delivery); err == nil {
		t.Fatal("RecoverConfirmation accepted mismatched digest")
	}

	client.messages = nil
	client.hasMore = true
	if _, _, err := pub.RecoverConfirmation(t.Context(), delivery); err == nil {
		t.Fatal("RecoverConfirmation accepted incomplete history")
	}

	client.messages = []slackapi.Message{{Msg: slackapi.Msg{
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: confirmationMetadata(delivery),
	}}}
	if _, _, err := pub.RecoverConfirmation(t.Context(), delivery); err == nil {
		t.Fatal("RecoverConfirmation accepted a match from incomplete history")
	}
}
