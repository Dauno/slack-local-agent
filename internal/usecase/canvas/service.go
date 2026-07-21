package canvas

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type CanvasError struct {
	Message string
}

func (e CanvasError) Error() string { return e.Message }

type Config struct {
	MaxTitleChars   int
	MaxContentChars int
	MaxContentBytes int
}

type Dependencies struct {
	Creator         port.CanvasCreator
	Store           port.CanvasOperationStore
	Clock           port.Clock
	Logger          port.Logger
	SanitizeContent func(string) string
}

type Service struct {
	cfg      Config
	creator  port.CanvasCreator
	store    port.CanvasOperationStore
	clock    port.Clock
	logger   port.Logger
	sanitize func(string) string
}

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Creator == nil {
		return nil, errors.New("canvas creator is required")
	}
	if deps.Store == nil {
		return nil, errors.New("canvas operation store is required")
	}
	if deps.SanitizeContent == nil {
		return nil, errors.New("canvas content sanitizer is required")
	}
	if cfg.MaxTitleChars <= 0 {
		cfg.MaxTitleChars = 150
	}
	if cfg.MaxContentChars <= 0 {
		cfg.MaxContentChars = 50000
	}
	if cfg.MaxContentBytes <= 0 {
		cfg.MaxContentBytes = 5 * 1024 * 1024
	}
	if deps.Clock == nil {
		deps.Clock = systemClock{}
	}
	return &Service{
		cfg: cfg, creator: deps.Creator, store: deps.Store,
		clock: deps.Clock, logger: deps.Logger, sanitize: deps.SanitizeContent,
	}, nil
}

type CreateCanvasResult struct {
	OperationID string
	CanvasID    string
}

func (s *Service) CreateCanvas(ctx context.Context, operationID string, conversationKey domain.ConversationKey, actor string, title string, content string) (CreateCanvasResult, error) {
	if strings.TrimSpace(operationID) == "" {
		return CreateCanvasResult{}, &CanvasError{Message: "operation ID is required"}
	}
	if conversationKey == "" {
		return CreateCanvasResult{}, &CanvasError{Message: "conversation key is required"}
	}
	if strings.TrimSpace(actor) == "" {
		return CreateCanvasResult{}, &CanvasError{Message: "actor is required"}
	}

	var err error
	title, content, err = s.prepareCanvas(title, content)
	if err != nil {
		return CreateCanvasResult{}, err
	}

	contentDigest := sha256.Sum256([]byte(content))
	contentSHA256 := fmt.Sprintf("%x", contentDigest)

	now := s.clock.Now().UTC()
	op := domain.CanvasOperation{
		ID:              operationID,
		ConversationKey: conversationKey,
		Actor:           actor,
		Title:           title,
		ContentSHA256:   contentSHA256,
		Status:          domain.CanvasOpReady,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.store.CreateOperation(ctx, op); err != nil {
		if errors.Is(err, port.ErrCanvasOperationExists) {
			existing, getErr := s.store.GetOperation(ctx, operationID)
			if getErr != nil {
				return CreateCanvasResult{}, fmt.Errorf("get existing canvas operation: %w", getErr)
			}
			if existing == nil || existing.ConversationKey != conversationKey || existing.Actor != actor || existing.Title != title || existing.ContentSHA256 != contentSHA256 {
				return CreateCanvasResult{}, fmt.Errorf("canvas operation %q does not match the confirmed request", operationID)
			}
			if existing.Status == domain.CanvasOpCompleted && existing.CanvasID != "" {
				return CreateCanvasResult{OperationID: operationID, CanvasID: existing.CanvasID}, nil
			}
			return CreateCanvasResult{}, fmt.Errorf("canvas operation %q already exists and will not be retried", operationID)
		}
		return CreateCanvasResult{}, fmt.Errorf("create canvas operation record: %w", err)
	}

	if err := s.store.UpdateOperationStatus(ctx, operationID, domain.CanvasOpCreating, ""); err != nil {
		return CreateCanvasResult{}, fmt.Errorf("mark canvas operation creating: %w", err)
	}

	result, err := s.creator.CreateCanvas(ctx, title, content)
	if err != nil {
		finalStatus := domain.CanvasOpFailed
		if isAmbiguous(err) {
			finalStatus = domain.CanvasOpUnknown
		}
		if updateErr := s.store.UpdateOperationStatus(ctx, operationID, finalStatus, ""); updateErr != nil && s.logger != nil {
			s.logger.Error("canvas operation status update failed", "operation_id", operationID, "error", updateErr)
		}
		return CreateCanvasResult{}, fmt.Errorf("create canvas: %w", err)
	}
	if !canvasIDPattern.MatchString(result.CanvasID) {
		if updateErr := s.store.UpdateOperationStatus(ctx, operationID, domain.CanvasOpUnknown, ""); updateErr != nil && s.logger != nil {
			s.logger.Error("canvas operation status update failed", "operation_id", operationID, "error", updateErr)
		}
		return CreateCanvasResult{}, errors.New("create canvas: Slack returned an invalid canvas ID")
	}

	if err := s.store.UpdateOperationStatus(ctx, operationID, domain.CanvasOpCompleted, result.CanvasID); err != nil {
		return CreateCanvasResult{}, fmt.Errorf("mark canvas operation completed: %w", err)
	}

	return CreateCanvasResult{OperationID: operationID, CanvasID: result.CanvasID}, nil
}

func (s *Service) ValidateCanvas(title, content string) error {
	_, _, err := s.prepareCanvas(title, content)
	return err
}

var (
	canvasMentionPattern     = regexp.MustCompile(`!\[\]\((?:[@#][A-Z0-9]+|https://[^)\s]+\.slack\.com/(?:team|archives)/[A-Z0-9]+)\)`)
	legacyMentionPattern     = regexp.MustCompile(`<(?:[@#][A-Z0-9]+|![^>\s]+)>`)
	markdownLinkPattern      = regexp.MustCompile(`!?\[[^\]\n]*\]\(\s*([^\s)]+)`)
	markdownReferencePattern = regexp.MustCompile(`(?m)^\s*\[[^\]\n]+\]:\s*(\S+)`)
	markdownAutolinkPattern  = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9+.-]*:[^<>\s]+)>`)
	canvasIDPattern          = regexp.MustCompile(`^[A-Z0-9]+$`)
)

func (s *Service) prepareCanvas(title, content string) (string, string, error) {
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if !utf8.ValidString(title) || !utf8.ValidString(content) {
		return "", "", &CanvasError{Message: "canvas title and content must be valid UTF-8"}
	}
	if title == "" {
		return "", "", &CanvasError{Message: "canvas title is required"}
	}
	if content == "" {
		return "", "", &CanvasError{Message: "canvas content is required"}
	}
	if utf8.RuneCountInString(title) > s.cfg.MaxTitleChars {
		return "", "", &CanvasError{Message: fmt.Sprintf("title exceeds %d characters", s.cfg.MaxTitleChars)}
	}
	if utf8.RuneCountInString(content) > s.cfg.MaxContentChars {
		return "", "", &CanvasError{Message: fmt.Sprintf("content exceeds %d characters", s.cfg.MaxContentChars)}
	}
	if len(content) > s.cfg.MaxContentBytes {
		return "", "", &CanvasError{Message: fmt.Sprintf("content exceeds %d bytes", s.cfg.MaxContentBytes)}
	}

	title = s.sanitize(title)
	content = s.sanitize(content)
	content = canvasMentionPattern.ReplaceAllStringFunc(content, func(match string) string { return `\` + match })
	content = legacyMentionPattern.ReplaceAllStringFunc(content, func(match string) string { return "&lt;" + match[1:] })
	if err := validateCanvasLinks(content); err != nil {
		return "", "", err
	}
	if err := validateCanvasTables(content); err != nil {
		return "", "", err
	}
	if utf8.RuneCountInString(title) > s.cfg.MaxTitleChars || utf8.RuneCountInString(content) > s.cfg.MaxContentChars || len(content) > s.cfg.MaxContentBytes {
		return "", "", &CanvasError{Message: "sanitized canvas content exceeds configured limits"}
	}
	return title, content, nil
}

func validateCanvasLinks(content string) error {
	groups := [][]string{}
	for _, pattern := range []*regexp.Regexp{markdownLinkPattern, markdownReferencePattern, markdownAutolinkPattern} {
		for _, match := range pattern.FindAllStringSubmatch(content, -1) {
			groups = append(groups, match)
		}
	}
	for _, match := range groups {
		target := strings.Trim(strings.TrimSpace(match[1]), "<>")
		if strings.HasPrefix(target, "@") || strings.HasPrefix(target, "#") {
			continue
		}
		parsed, err := url.ParseRequestURI(target)
		if err != nil {
			return &CanvasError{Message: "canvas content contains a malformed link"}
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			if parsed.Host == "" {
				return &CanvasError{Message: "canvas links must include a host"}
			}
		case "mailto":
			if parsed.Opaque == "" {
				return &CanvasError{Message: "canvas mail links must include an address"}
			}
		default:
			return &CanvasError{Message: fmt.Sprintf("canvas link scheme %q is not allowed", parsed.Scheme)}
		}
	}
	return nil
}

func validateCanvasTables(content string) error {
	lines := strings.Split(content, "\n")
	for index := 1; index < len(lines); index++ {
		separator := strings.Trim(strings.TrimSpace(lines[index]), "|")
		cells := strings.Split(separator, "|")
		if len(cells) == 0 {
			continue
		}
		valid := true
		for _, cell := range cells {
			cell = strings.Trim(strings.TrimSpace(cell), ":")
			if len(cell) < 3 || strings.Trim(cell, "-") != "" {
				valid = false
				break
			}
		}
		if !valid || !strings.Contains(lines[index-1], "|") {
			continue
		}
		cellCount := len(strings.Split(strings.Trim(strings.TrimSpace(lines[index-1]), "|"), "|"))
		for row := index + 1; row < len(lines) && strings.Contains(lines[row], "|") && strings.TrimSpace(lines[row]) != ""; row++ {
			cellCount += len(strings.Split(strings.Trim(strings.TrimSpace(lines[row]), "|"), "|"))
		}
		if cellCount > 300 {
			return &CanvasError{Message: "canvas tables must not exceed 300 cells"}
		}
	}
	return nil
}

func isAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var createErr *port.CanvasCreateError
	if errors.As(err, &createErr) {
		return createErr.Ambiguous
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	errStr := err.Error()
	return strings.Contains(errStr, "context deadline exceeded") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "internal_error") ||
		strings.Contains(errStr, "fatal_error") ||
		strings.Contains(errStr, "service_unavailable")
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
