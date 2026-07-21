package canvas_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/canvas"
)

type fakeCanvasCreator struct {
	result  port.CanvasCreateResult
	err     error
	created bool
	title   string
	content string
}

func (f *fakeCanvasCreator) CreateCanvas(_ context.Context, title, content string) (port.CanvasCreateResult, error) {
	f.created = true
	f.title = title
	f.content = content
	return f.result, f.err
}

type fakeCanvasStore struct {
	ops     map[string]domain.CanvasOperation
	updates []struct {
		id, canvasID string
		status       domain.CanvasOperationStatus
	}
}

func (f *fakeCanvasStore) CreateOperation(_ context.Context, op domain.CanvasOperation) error {
	if f.ops == nil {
		f.ops = make(map[string]domain.CanvasOperation)
	}
	if _, exists := f.ops[op.ID]; exists {
		return port.ErrCanvasOperationExists
	}
	f.ops[op.ID] = op
	return nil
}

func (f *fakeCanvasStore) UpdateOperationStatus(_ context.Context, id string, status domain.CanvasOperationStatus, canvasID string) error {
	f.updates = append(f.updates, struct {
		id, canvasID string
		status       domain.CanvasOperationStatus
	}{id, canvasID, status})
	if op, ok := f.ops[id]; ok {
		op.Status = status
		op.CanvasID = canvasID
		f.ops[id] = op
	}
	return nil
}

func (f *fakeCanvasStore) GetOperation(_ context.Context, id string) (*domain.CanvasOperation, error) {
	op, ok := f.ops[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return &op, nil
}

func TestCreateCanvasSuccess(t *testing.T) {
	creator := &fakeCanvasCreator{result: port.CanvasCreateResult{CanvasID: "F123"}}
	store := &fakeCanvasStore{}
	svc, err := canvas.New(canvas.Config{}, canvas.Dependencies{
		Creator: creator, Store: store, SanitizeContent: func(value string) string { return value },
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	result, err := svc.CreateCanvas(context.Background(), "op-1", "slack:T:dm:C", "U1", "Report", "# Hello")
	if err != nil {
		t.Fatalf("CreateCanvas() error: %v", err)
	}
	if result.CanvasID != "F123" {
		t.Fatalf("expected canvas ID F123, got %q", result.CanvasID)
	}
	if !creator.created {
		t.Fatal("canvas creator was not called")
	}
	if len(store.ops) != 1 {
		t.Fatalf("expected 1 operation, got %d", len(store.ops))
	}
	if len(store.updates) != 2 {
		t.Fatalf("expected 2 status updates, got %d", len(store.updates))
	}
	if store.updates[0].status != domain.CanvasOpCreating {
		t.Fatalf("expected creating status, got %s", store.updates[0].status)
	}
	if store.updates[1].status != domain.CanvasOpCompleted {
		t.Fatalf("expected completed status, got %s", store.updates[1].status)
	}
	if store.updates[1].canvasID != "F123" {
		t.Fatalf("expected canvas ID F123 in update, got %q", store.updates[1].canvasID)
	}
}

func TestCreateCanvasValidationErrors(t *testing.T) {
	creator := &fakeCanvasCreator{result: port.CanvasCreateResult{CanvasID: "F123"}}
	store := &fakeCanvasStore{}
	svc, _ := canvas.New(canvas.Config{MaxTitleChars: 10, MaxContentChars: 100}, canvas.Dependencies{
		Creator: creator, Store: store, SanitizeContent: func(value string) string { return value },
	})

	tests := []struct {
		name, opID, convKey, actor, title, content string
		wantErr                                    string
	}{
		{"empty opID", "", "key", "actor", "title", "content", "operation ID"},
		{"empty convKey", "op", "", "actor", "title", "content", "conversation key"},
		{"empty actor", "op", "key", "", "title", "content", "actor"},
		{"empty title", "op", "key", "actor", "", "content", "title"},
		{"long title", "op", "key", "actor", strings.Repeat("x", 11), "content", "exceeds"},
		{"empty content", "op", "key", "actor", "title", "", "content"},
		{"long content", "op", "key", "actor", "title", strings.Repeat("x", 101), "exceeds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateCanvas(context.Background(), tt.opID, domain.ConversationKey(tt.convKey), tt.actor, tt.title, tt.content)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestCreateCanvasAPIFailure(t *testing.T) {
	creator := &fakeCanvasCreator{err: errors.New("slack api error")}
	store := &fakeCanvasStore{}
	svc, _ := canvas.New(canvas.Config{}, canvas.Dependencies{
		Creator: creator, Store: store, SanitizeContent: func(value string) string { return value },
	})

	_, err := svc.CreateCanvas(context.Background(), "op-1", "slack:T:dm:C", "U1", "Title", "Content")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(store.updates) != 2 {
		t.Fatalf("expected 2 updates (creating, failed), got %d", len(store.updates))
	}
	if store.updates[1].status != domain.CanvasOpFailed {
		t.Fatalf("expected failed status, got %s", store.updates[1].status)
	}
}

func TestCreateCanvasAmbiguousFailure(t *testing.T) {
	creator := &fakeCanvasCreator{err: errors.New("context deadline exceeded")}
	store := &fakeCanvasStore{}
	svc, _ := canvas.New(canvas.Config{}, canvas.Dependencies{
		Creator: creator, Store: store, SanitizeContent: func(value string) string { return value },
	})

	_, err := svc.CreateCanvas(context.Background(), "op-1", "slack:T:dm:C", "U1", "Title", "Content")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if store.updates[1].status != domain.CanvasOpUnknown {
		t.Fatalf("expected unknown status, got %s", store.updates[1].status)
	}
}

func TestCreateCanvasSanitizesSecretsMentionsAndRejectsUnsafeContent(t *testing.T) {
	creator := &fakeCanvasCreator{result: port.CanvasCreateResult{CanvasID: "F123"}}
	store := &fakeCanvasStore{}
	svc, err := canvas.New(canvas.Config{}, canvas.Dependencies{
		Creator: creator,
		Store:   store,
		SanitizeContent: func(value string) string {
			return strings.ReplaceAll(value, "secret-value", "****")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateCanvas(context.Background(), "op-safe", "slack:T:dm:C", "U1", "Report secret-value", "Hello ![](@U123) <@U456> secret-value [safe](https://example.com)")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(creator.title+creator.content, "secret-value") || strings.Contains(creator.content, "Hello ![](@U123)") || strings.Contains(creator.content, "<@U456>") {
		t.Fatalf("unsafe content reached creator: title=%q content=%q", creator.title, creator.content)
	}

	for _, content := range []string{
		"[bad](javascript:alert)",
		string([]byte{'b', 'a', 'd', 0xff}),
		largeTable(151),
	} {
		creator.created = false
		if err := svc.ValidateCanvas("Report", content); err == nil {
			t.Fatalf("ValidateCanvas accepted unsafe content %q", content[:min(len(content), 30)])
		}
		if creator.created {
			t.Fatal("validation invoked canvas creator")
		}
	}
}

func TestCreateCanvasReplayReturnsCompletedOperationWithoutSecondCreation(t *testing.T) {
	creator := &fakeCanvasCreator{result: port.CanvasCreateResult{CanvasID: "F123"}}
	store := &fakeCanvasStore{}
	svc, _ := canvas.New(canvas.Config{}, canvas.Dependencies{
		Creator: creator, Store: store, SanitizeContent: func(value string) string { return value },
	})
	if _, err := svc.CreateCanvas(context.Background(), "op-replay", "slack:T:dm:C", "U1", "Report", "Content"); err != nil {
		t.Fatal(err)
	}
	creator.created = false
	result, err := svc.CreateCanvas(context.Background(), "op-replay", "slack:T:dm:C", "U1", "Report", "Content")
	if err != nil || result.CanvasID != "F123" || creator.created {
		t.Fatalf("replay result=%#v err=%v creator_called=%t", result, err, creator.created)
	}
}

func largeTable(rows int) string {
	var content strings.Builder
	content.WriteString("| A | B |\n| --- | --- |\n")
	for range rows {
		content.WriteString("| x | y |\n")
	}
	return content.String()
}

func TestNewRequiresCreator(t *testing.T) {
	_, err := canvas.New(canvas.Config{}, canvas.Dependencies{Store: &fakeCanvasStore{}})
	if err == nil || !strings.Contains(err.Error(), "canvas creator") {
		t.Fatalf("expected 'canvas creator is required', got %v", err)
	}
}

func TestNewRequiresStore(t *testing.T) {
	_, err := canvas.New(canvas.Config{}, canvas.Dependencies{Creator: &fakeCanvasCreator{}})
	if err == nil || !strings.Contains(err.Error(), "operation store") {
		t.Fatalf("expected 'operation store is required', got %v", err)
	}
}

func TestNewRequiresSanitizer(t *testing.T) {
	_, err := canvas.New(canvas.Config{}, canvas.Dependencies{Creator: &fakeCanvasCreator{}, Store: &fakeCanvasStore{}})
	if err == nil || !strings.Contains(err.Error(), "sanitizer") {
		t.Fatalf("expected sanitizer error, got %v", err)
	}
}
