package generatedfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type ExportError struct{ Message string }

func (e *ExportError) Error() string { return e.Message }

type Config struct {
	MaxFilenameChars int
	MaxContentBytes  int
}

type Dependencies struct {
	Uploader        port.GeneratedFileUploader
	Store           port.GeneratedFileOperationStore
	Clock           port.Clock
	Logger          port.Logger
	SanitizeContent func(string) string
}

type Service struct {
	cfg      Config
	uploader port.GeneratedFileUploader
	store    port.GeneratedFileOperationStore
	clock    port.Clock
	logger   port.Logger
	sanitize func(string) string
}

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Uploader == nil {
		return nil, errors.New("generated file uploader is required")
	}
	if deps.Store == nil {
		return nil, errors.New("generated file operation store is required")
	}
	if deps.SanitizeContent == nil {
		return nil, errors.New("generated file content sanitizer is required")
	}
	if cfg.MaxFilenameChars <= 0 {
		cfg.MaxFilenameChars = domain.MaxGeneratedFilenameRunes
	}
	if cfg.MaxContentBytes <= 0 {
		cfg.MaxContentBytes = domain.MaxGeneratedFileBytes
	}
	if cfg.MaxFilenameChars > domain.MaxGeneratedFilenameRunes {
		return nil, fmt.Errorf("generated file filename limit must not exceed %d characters", domain.MaxGeneratedFilenameRunes)
	}
	if cfg.MaxContentBytes > domain.MaxGeneratedFileBytes {
		return nil, fmt.Errorf("generated file content limit must not exceed %d bytes", domain.MaxGeneratedFileBytes)
	}
	if deps.Clock == nil {
		deps.Clock = systemClock{}
	}
	return &Service{cfg: cfg, uploader: deps.Uploader, store: deps.Store, clock: deps.Clock, logger: deps.Logger, sanitize: deps.SanitizeContent}, nil
}

type ExportRequest struct {
	OperationID     string
	ConversationKey domain.ConversationKey
	Actor           string
	Filename        string
	Format          domain.GeneratedFileFormat
	Content         []byte
}

type ExportResult struct {
	OperationID string
	SlackFileID string
	Filename    string
}

func (s *Service) Export(ctx context.Context, request ExportRequest) (ExportResult, error) {
	if strings.TrimSpace(request.OperationID) == "" {
		return ExportResult{}, &ExportError{Message: "operation ID is required"}
	}
	if request.ConversationKey == "" {
		return ExportResult{}, &ExportError{Message: "conversation key is required"}
	}
	if strings.TrimSpace(request.Actor) == "" {
		return ExportResult{}, &ExportError{Message: "actor is required"}
	}
	channelID, threadTS, err := destination(request.ConversationKey)
	if err != nil {
		return ExportResult{}, err
	}
	filename, mediaType, content, err := s.prepare(request.Filename, request.Format, request.Content)
	if err != nil {
		return ExportResult{}, err
	}
	digest := sha256.Sum256(content)
	now := s.clock.Now().UTC()
	op := domain.GeneratedFileOperation{ID: request.OperationID, ConversationKey: request.ConversationKey, Actor: request.Actor, Filename: filename, MediaType: mediaType, ContentSHA256: fmt.Sprintf("%x", digest), SizeBytes: len(content), Status: domain.GeneratedFileOpPendingConfirmation, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateGeneratedFileOperation(ctx, op); err != nil {
		if errors.Is(err, port.ErrGeneratedFileOperationExists) {
			existing, getErr := s.store.GetGeneratedFileOperation(ctx, request.OperationID)
			if getErr != nil {
				return ExportResult{}, fmt.Errorf("get existing generated file operation: %w", getErr)
			}
			if existing == nil || existing.ConversationKey != request.ConversationKey || existing.Actor != request.Actor || existing.Filename != filename || existing.MediaType != mediaType || existing.ContentSHA256 != op.ContentSHA256 || existing.SizeBytes != op.SizeBytes {
				return ExportResult{}, fmt.Errorf("generated file operation %q does not match the confirmed request", request.OperationID)
			}
			if existing.Status == domain.GeneratedFileOpCompleted && existing.SlackFileID != "" {
				return ExportResult{OperationID: existing.ID, SlackFileID: existing.SlackFileID, Filename: existing.Filename}, nil
			}
			return ExportResult{}, fmt.Errorf("generated file operation %q already exists and will not be retried", request.OperationID)
		}
		return ExportResult{}, fmt.Errorf("create generated file operation record: %w", err)
	}

	target, err := s.uploader.RequestUploadURL(ctx, filename, len(content))
	if err != nil {
		s.updateStatus(ctx, request.OperationID, failedStatus(err), "")
		return ExportResult{}, fmt.Errorf("request upload URL: %w", err)
	}
	if strings.TrimSpace(target.FileID) == "" || strings.TrimSpace(target.UploadURL) == "" {
		s.updateStatus(ctx, request.OperationID, domain.GeneratedFileOpUnknown, target.FileID)
		return ExportResult{}, errors.New("request upload URL: Slack returned an incomplete upload target")
	}
	if err := s.persistStatus(ctx, request.OperationID, domain.GeneratedFileOpURLRequested, target.FileID); err != nil {
		return ExportResult{}, fmt.Errorf("mark generated file upload URL requested: %w", err)
	}
	if err := s.uploader.UploadBytes(ctx, target, content); err != nil {
		s.updateStatus(ctx, request.OperationID, failedStatus(err), target.FileID)
		return ExportResult{}, fmt.Errorf("upload generated file bytes: %w", err)
	}
	if err := s.persistStatus(ctx, request.OperationID, domain.GeneratedFileOpBytesUploaded, target.FileID); err != nil {
		return ExportResult{}, fmt.Errorf("mark generated file bytes uploaded: %w", err)
	}
	if err := s.uploader.CompleteUpload(ctx, target.FileID, channelID, threadTS, filename); err != nil {
		s.updateStatus(ctx, request.OperationID, failedStatus(err), target.FileID)
		return ExportResult{}, fmt.Errorf("complete generated file upload: %w", err)
	}
	if err := s.persistStatus(ctx, request.OperationID, domain.GeneratedFileOpCompleted, target.FileID); err != nil {
		return ExportResult{}, fmt.Errorf("mark generated file completed: %w", err)
	}
	return ExportResult{OperationID: request.OperationID, SlackFileID: target.FileID, Filename: filename}, nil
}

func (s *Service) Validate(filename string, format domain.GeneratedFileFormat, content []byte) error {
	_, _, _, err := s.prepare(filename, format, content)
	return err
}

func (s *Service) prepare(filename string, format domain.GeneratedFileFormat, content []byte) (string, string, []byte, error) {
	filename = strings.TrimSpace(path.Base(strings.ReplaceAll(filename, "\\", "/")))
	if filename == "." || filename == "" || !utf8.ValidString(filename) || strings.ContainsAny(filename, "\x00\r\n") || hasControl(filename) {
		return "", "", nil, &ExportError{Message: "filename must be a non-empty single-line UTF-8 basename"}
	}
	if utf8.RuneCountInString(filename) > s.cfg.MaxFilenameChars {
		return "", "", nil, &ExportError{Message: fmt.Sprintf("filename exceeds %d characters", s.cfg.MaxFilenameChars)}
	}
	extension, mediaType := formatDetails(format)
	if extension == "" || !strings.EqualFold(path.Ext(filename), extension) {
		return "", "", nil, &ExportError{Message: fmt.Sprintf("filename must use %s for %s export", extension, format)}
	}
	if len(content) == 0 || len(content) > s.cfg.MaxContentBytes || !utf8.Valid(content) {
		return "", "", nil, &ExportError{Message: fmt.Sprintf("content must be valid UTF-8 and contain 1-%d bytes", s.cfg.MaxContentBytes)}
	}
	sanitized := []byte(s.sanitize(string(content)))
	if len(sanitized) == 0 || len(sanitized) > s.cfg.MaxContentBytes || !utf8.Valid(sanitized) {
		return "", "", nil, &ExportError{Message: "sanitized content exceeds configured limits or is invalid UTF-8"}
	}
	if format == domain.GeneratedFileJSON {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(sanitized))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return "", "", nil, &ExportError{Message: "JSON content must be valid"}
		}
		if err := ensureJSONEnd(decoder); err != nil {
			return "", "", nil, &ExportError{Message: "JSON content must contain exactly one value"}
		}
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "", "", nil, fmt.Errorf("encode JSON content: %w", err)
		}
		sanitized = append(encoded, '\n')
	}
	return filename, mediaType, sanitized, nil
}

func SerializeCSV(headers []string, rows [][]string) ([]byte, error) {
	if len(headers) == 0 {
		return nil, &ExportError{Message: "CSV headers are required"}
	}
	for _, row := range rows {
		if len(row) != len(headers) {
			return nil, &ExportError{Message: "every CSV row must have the same number of cells as headers"}
		}
	}
	var buffer strings.Builder
	writer := csv.NewWriter(&buffer)
	if err := writer.Write(headers); err != nil {
		return nil, fmt.Errorf("write CSV headers: %w", err)
	}
	writer.WriteAll(rows)
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("write CSV rows: %w", err)
	}
	return []byte(buffer.String()), nil
}

func formatDetails(format domain.GeneratedFileFormat) (string, string) {
	switch format {
	case domain.GeneratedFileText:
		return ".txt", "text/plain"
	case domain.GeneratedFileMarkdown:
		return ".md", "text/markdown"
	case domain.GeneratedFileCSV:
		return ".csv", "text/csv"
	case domain.GeneratedFileJSON:
		return ".json", "application/json"
	default:
		return "", ""
	}
}

func destination(key domain.ConversationKey) (channelID, threadTS string, err error) {
	parts := strings.Split(string(key), ":")
	if len(parts) == 4 && parts[0] == "slack" && parts[2] == "dm" && parts[3] != "" {
		return parts[3], "", nil
	}
	if len(parts) == 6 && parts[0] == "slack" && parts[2] == "channel" && parts[4] == "thread" && parts[3] != "" && parts[5] != "" {
		return parts[3], parts[5], nil
	}
	return "", "", errors.New("generated file destination is not a canonical Slack conversation")
}

func failedStatus(err error) domain.GeneratedFileOperationStatus {
	var uploadErr *port.GeneratedFileUploadError
	if errors.As(err, &uploadErr) && uploadErr.Ambiguous {
		return domain.GeneratedFileOpUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return domain.GeneratedFileOpUnknown
	}
	return domain.GeneratedFileOpFailed
}

func (s *Service) updateStatus(ctx context.Context, operationID string, status domain.GeneratedFileOperationStatus, fileID string) {
	if err := s.persistStatus(ctx, operationID, status, fileID); err != nil && s.logger != nil {
		s.logger.Error("generated file operation status update failed", "operation_id", operationID, "error", err)
	}
}

func (s *Service) persistStatus(ctx context.Context, operationID string, status domain.GeneratedFileOperationStatus, fileID string) error {
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.store.UpdateGeneratedFileOperation(updateCtx, operationID, status, fileID)
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("extra JSON value")
		}
		return err
	}
	return nil
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) }) >= 0
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
