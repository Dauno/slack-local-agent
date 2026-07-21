package generatedfile_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/generatedfile"
)

type fakeUploader struct {
	target      port.GeneratedFileUploadTarget
	err         error
	uploadErr   error
	completeErr error
	content     []byte
	channel     string
	thread      string
	completedID string
	requestHook func()
	requests    int
}

func (f *fakeUploader) RequestUploadURL(context.Context, string, int) (port.GeneratedFileUploadTarget, error) {
	f.requests++
	if f.requestHook != nil {
		f.requestHook()
	}
	return f.target, f.err
}
func (f *fakeUploader) UploadBytes(_ context.Context, _ port.GeneratedFileUploadTarget, content []byte) error {
	f.content = append([]byte(nil), content...)
	return f.uploadErr
}
func (f *fakeUploader) CompleteUpload(_ context.Context, fileID, channel, thread, _ string) error {
	f.completedID, f.channel, f.thread = fileID, channel, thread
	return f.completeErr
}

type fakeStore struct {
	operations map[string]domain.GeneratedFileOperation
}

func (f *fakeStore) CreateGeneratedFileOperation(_ context.Context, op domain.GeneratedFileOperation) error {
	if f.operations == nil {
		f.operations = map[string]domain.GeneratedFileOperation{}
	}
	if _, ok := f.operations[op.ID]; ok {
		return port.ErrGeneratedFileOperationExists
	}
	f.operations[op.ID] = op
	return nil
}
func (f *fakeStore) UpdateGeneratedFileOperation(ctx context.Context, id string, status domain.GeneratedFileOperationStatus, fileID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	op := f.operations[id]
	op.Status, op.SlackFileID = status, fileID
	f.operations[id] = op
	return nil
}
func (f *fakeStore) GetGeneratedFileOperation(_ context.Context, id string) (*domain.GeneratedFileOperation, error) {
	op, ok := f.operations[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return &op, nil
}

func TestExportRedactsBeforeUploadAndSharesOnlyConversation(t *testing.T) {
	uploader := &fakeUploader{target: port.GeneratedFileUploadTarget{FileID: "F123", UploadURL: "https://upload.example.test/secret"}}
	store := &fakeStore{}
	svc, err := generatedfile.New(generatedfile.Config{}, generatedfile.Dependencies{Uploader: uploader, Store: store, SanitizeContent: func(value string) string { return strings.ReplaceAll(value, "secret", "[REDACTED]") }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Export(context.Background(), generatedfile.ExportRequest{OperationID: "file:1", ConversationKey: "slack:T1:channel:C1:thread:1.2", Actor: "U1", Filename: "../report.json", Format: domain.GeneratedFileJSON, Content: []byte(`{"token":"secret","b":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Filename != "report.json" || result.SlackFileID != "F123" {
		t.Fatalf("result = %#v", result)
	}
	if string(uploader.content) != "{\n  \"b\": 2,\n  \"token\": \"[REDACTED]\"\n}\n" {
		t.Fatalf("uploaded content = %q", uploader.content)
	}
	if uploader.channel != "C1" || uploader.thread != "1.2" || uploader.completedID != "F123" {
		t.Fatalf("destination = %q/%q id=%q", uploader.channel, uploader.thread, uploader.completedID)
	}
	op := store.operations["file:1"]
	if op.Status != domain.GeneratedFileOpCompleted || op.SlackFileID != "F123" || strings.Contains(fmtOperation(op), "secret") || strings.Contains(fmtOperation(op), "upload.example") {
		t.Fatalf("operation = %#v", op)
	}
}

func TestExportDoesNotRetryExistingIncompleteOperation(t *testing.T) {
	uploader := &fakeUploader{target: port.GeneratedFileUploadTarget{FileID: "F123", UploadURL: "https://upload.example.test"}, uploadErr: errors.New("bad request")}
	store := &fakeStore{}
	svc, _ := generatedfile.New(generatedfile.Config{}, generatedfile.Dependencies{Uploader: uploader, Store: store, SanitizeContent: func(value string) string { return value }})
	request := generatedfile.ExportRequest{OperationID: "file:1", ConversationKey: "slack:T1:dm:D1", Actor: "U1", Filename: "report.txt", Format: domain.GeneratedFileText, Content: []byte("body")}
	if _, err := svc.Export(context.Background(), request); err == nil {
		t.Fatal("first export succeeded")
	}
	if _, err := svc.Export(context.Background(), request); err == nil || !strings.Contains(err.Error(), "will not be retried") {
		t.Fatalf("retry error = %v", err)
	}
}

func TestExportRejectsInvalidDestinationBeforeCreatingOrUploading(t *testing.T) {
	uploader := &fakeUploader{target: port.GeneratedFileUploadTarget{FileID: "F123", UploadURL: "https://upload.example.test"}}
	store := &fakeStore{}
	svc, _ := generatedfile.New(generatedfile.Config{}, generatedfile.Dependencies{Uploader: uploader, Store: store, SanitizeContent: func(value string) string { return value }})

	_, err := svc.Export(context.Background(), generatedfile.ExportRequest{OperationID: "file:1", ConversationKey: "not-slack", Actor: "U1", Filename: "report.txt", Format: domain.GeneratedFileText, Content: []byte("body")})
	if err == nil || !strings.Contains(err.Error(), "canonical Slack conversation") {
		t.Fatalf("Export() error = %v", err)
	}
	if uploader.requests != 0 || len(store.operations) != 0 {
		t.Fatalf("invalid destination mutated state: requests=%d operations=%d", uploader.requests, len(store.operations))
	}
}

func TestExportPersistsUnknownStateAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	uploader := &fakeUploader{err: context.Canceled, requestHook: cancel}
	store := &fakeStore{}
	svc, _ := generatedfile.New(generatedfile.Config{}, generatedfile.Dependencies{Uploader: uploader, Store: store, SanitizeContent: func(value string) string { return value }})

	_, err := svc.Export(ctx, generatedfile.ExportRequest{OperationID: "file:1", ConversationKey: "slack:T1:dm:D1", Actor: "U1", Filename: "report.txt", Format: domain.GeneratedFileText, Content: []byte("body")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Export() error = %v, want context cancellation", err)
	}
	if got := store.operations["file:1"].Status; got != domain.GeneratedFileOpUnknown {
		t.Fatalf("operation status = %q, want unknown", got)
	}
}

func TestExportJSONPreservesLargeIntegerPrecision(t *testing.T) {
	uploader := &fakeUploader{target: port.GeneratedFileUploadTarget{FileID: "F123", UploadURL: "https://upload.example.test"}}
	svc, _ := generatedfile.New(generatedfile.Config{}, generatedfile.Dependencies{Uploader: uploader, Store: &fakeStore{}, SanitizeContent: func(value string) string { return value }})

	_, err := svc.Export(context.Background(), generatedfile.ExportRequest{OperationID: "file:1", ConversationKey: "slack:T1:dm:D1", Actor: "U1", Filename: "report.json", Format: domain.GeneratedFileJSON, Content: []byte(`{"value":9007199254740993}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(uploader.content); got != "{\n  \"value\": 9007199254740993\n}\n" {
		t.Fatalf("uploaded JSON = %q", got)
	}
}

func TestNewRejectsLimitsAboveSecurityMaximum(t *testing.T) {
	deps := generatedfile.Dependencies{Uploader: &fakeUploader{}, Store: &fakeStore{}, SanitizeContent: func(value string) string { return value }}
	if _, err := generatedfile.New(generatedfile.Config{MaxFilenameChars: domain.MaxGeneratedFilenameRunes + 1}, deps); err == nil {
		t.Fatal("New() accepted an excessive filename limit")
	}
	if _, err := generatedfile.New(generatedfile.Config{MaxContentBytes: domain.MaxGeneratedFileBytes + 1}, deps); err == nil {
		t.Fatal("New() accepted an excessive content limit")
	}
}

func TestSerializeCSVUsesRFC4180Escaping(t *testing.T) {
	content, err := generatedfile.SerializeCSV([]string{"name", "note"}, [][]string{{"Ada", "a,b\nquoted"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "name,note\nAda,\"a,b\nquoted\"\n" {
		t.Fatalf("CSV = %q", content)
	}
}

func fmtOperation(op domain.GeneratedFileOperation) string {
	return op.ID + op.Filename + op.MediaType + op.ContentSHA256 + op.SlackFileID
}
