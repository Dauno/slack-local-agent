package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type Config struct {
	Recall      domain.MemoryRecallConfig
	Limits      domain.MemoryLimits
	MaxPatchOps int
}

type Dependencies struct {
	Store           port.MemoryStore
	Logger          port.Logger
	SanitizeContent func(string) string
}

type Outcome string

const (
	OutcomeRecallEmpty   Outcome = "recall_empty"
	OutcomeRecallHit     Outcome = "recall_hit"
	OutcomeRecallError   Outcome = "recall_error"
	OutcomeApplyCreated  Outcome = "apply_created"
	OutcomeApplyUpdated  Outcome = "apply_updated"
	OutcomeApplyNoop     Outcome = "apply_noop"
	OutcomeApplyRejected Outcome = "apply_rejected"
)

type Service struct {
	cfg      Config
	store    port.MemoryStore
	logger   port.Logger
	sanitize func(string) string
}

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("memory store is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("logger is required")
	}
	if cfg.MaxPatchOps <= 0 || cfg.Limits.MaxTopics <= 0 || cfg.Limits.MaxTopicChars <= 0 || cfg.Limits.MaxLinks < 0 {
		return nil, errors.New("memory limits must be positive (max links may be zero)")
	}
	if deps.SanitizeContent == nil {
		deps.SanitizeContent = func(value string) string { return value }
	}
	return &Service{cfg: cfg, store: deps.Store, logger: deps.Logger, sanitize: deps.SanitizeContent}, nil
}

func (s *Service) Recall(ctx context.Context, query, ownerKey string) ([]domain.MemorySnippet, error) {
	snippets, _, err := s.recall(ctx, query, ownerKey)
	return snippets, err
}

// RelevantTopics supplies a bounded set of existing topic identities and
// revisions to the curator; the model never has to invent revision numbers.
func (s *Service) RelevantTopics(ctx context.Context, messages []domain.Message) ([]domain.TopicReference, error) {
	queries := domain.EntityMemorySearchQueries(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == domain.RoleUser && strings.TrimSpace(messages[i].Content) != "" {
			queries = append(queries, messages[i].Content)
			break
		}
	}
	seen := make(map[string]struct{})
	result := make([]domain.TopicReference, 0, s.cfg.Recall.MaxTopics)
	for _, query := range queries {
		if len(result) == s.cfg.Recall.MaxTopics {
			break
		}
		topics, err := s.store.SearchTopicReferences(ctx, query, s.cfg.Recall.MaxTopics-len(result))
		if err != nil {
			return nil, err
		}
		for _, topic := range topics {
			if _, exists := seen[topic.Slug]; exists {
				continue
			}
			seen[topic.Slug] = struct{}{}
			result = append(result, topic)
			if len(result) == s.cfg.Recall.MaxTopics {
				break
			}
		}
	}
	return result, nil
}

// TrustedEntityOperations resolves candidate entity slugs exactly so an
// existing entity is revised even when FTS recall is capped or misses it.
func (s *Service) TrustedEntityOperations(ctx context.Context, conversationKey domain.ConversationKey, messages []domain.Message) ([]domain.MemoryOp, error) {
	candidates := domain.EntityMemoryCandidates(messages)
	ownerKey := ""
	for _, message := range messages {
		if message.Role == domain.RoleUser {
			ownerKey = domain.SlackOwnerKey(conversationKey, message.UserID)
			break
		}
	}
	topics := make([]domain.TopicReference, 0, len(candidates))
	for _, candidate := range candidates {
		slug := candidate.Slug
		if candidate.BundlePath == "people" {
			slug = domain.ScopedPersonTopicSlug(slug, ownerKey)
		}
		topic, err := s.store.GetTopicReference(ctx, slug)
		if err != nil {
			return nil, err
		}
		if topic != nil {
			topics = append(topics, *topic)
		}
	}
	return domain.TrustedEntityMemoryOperations(messages, topics, ownerKey), nil
}

func (s *Service) recall(ctx context.Context, query, ownerKey string) ([]domain.MemorySnippet, Outcome, error) {
	if !s.cfg.Recall.Enabled || strings.TrimSpace(query) == "" {
		return nil, OutcomeRecallEmpty, nil
	}
	if s.cfg.Recall.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.Recall.Timeout)
		defer cancel()
	}
	fts, err := s.store.SearchTopicsForOwner(ctx, query, ownerKey, s.cfg.Recall.MaxTopics, s.cfg.Recall.MaxChars)
	if err != nil {
		s.logger.Warn("memory recall failed", "error", err)
		return nil, OutcomeRecallError, err
	}
	snippets := fts
	if isFirstPersonMemoryQuery(query) && strings.TrimSpace(ownerKey) != "" {
		// First-person questions need provenance recall in addition to normal FTS.
		personal, personalErr := s.store.SearchPersonTopicsByOwner(ctx, ownerKey, s.cfg.Recall.MaxTopics, s.cfg.Recall.MaxChars)
		if personalErr != nil {
			s.logger.Warn("personal memory recall failed", "error", personalErr)
		} else {
			snippets = mergeRecallSnippets(fts, personal, s.cfg.Recall.MaxTopics, s.cfg.Recall.MaxChars)
		}
	}
	if len(snippets) == 0 {
		return nil, OutcomeRecallEmpty, nil
	}
	s.logger.Debug("memory recall matched", "topics", len(snippets))
	return snippets, OutcomeRecallHit, nil
}

func isFirstPersonMemoryQuery(query string) bool {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	has := func(values ...string) bool {
		for _, word := range words {
			for _, value := range values {
				if word == value {
					return true
				}
			}
		}
		return false
	}
	hasPhrase := func(first, second string) bool {
		for index := 0; index+1 < len(words); index++ {
			if words[index] == first && words[index+1] == second {
				return true
			}
		}
		return false
	}
	return ((hasPhrase("de", "mi") || hasPhrase("de", "mí") || hasPhrase("sobre", "mi") || hasPhrase("sobre", "mí") || hasPhrase("about", "me")) &&
		has("sabes", "sabe", "recuerdas", "recuerda", "conoces", "conoce", "know", "remember")) ||
		(has("quien", "quién", "who") && has("soy", "am"))
}

func mergeRecallSnippets(first, second []domain.MemorySnippet, maxTopics, maxChars int) []domain.MemorySnippet {
	if maxTopics <= 0 {
		maxTopics = 3
	}
	if maxChars <= 0 {
		maxChars = 2_000
	}
	seen := make(map[domain.TopicID]struct{}, len(first)+len(second))
	result := make([]domain.MemorySnippet, 0, maxTopics)
	remaining := maxChars
	for _, snippets := range [][]domain.MemorySnippet{first, second} {
		for _, snippet := range snippets {
			if len(result) == maxTopics {
				return result
			}
			if _, exists := seen[snippet.TopicID]; exists {
				continue
			}
			if len([]rune(snippet.Content)) > remaining {
				continue
			}
			seen[snippet.TopicID] = struct{}{}
			result = append(result, snippet)
			remaining -= len([]rune(snippet.Content))
		}
	}
	return result
}

func (s *Service) ValidateAndApply(ctx context.Context, patch domain.MemoryPatch) (Outcome, error) {
	if len(patch.Operations) == 0 {
		return OutcomeApplyNoop, nil
	}
	if strings.TrimSpace(string(patch.ConversationKey)) == "" || strings.TrimSpace(patch.ExchangeTS) == "" {
		return OutcomeApplyRejected, errors.New("patch must reference a source conversation exchange")
	}
	if err := s.validatePatch(patch); err != nil {
		return OutcomeApplyRejected, err
	}
	patch, err := s.withoutExistingCreates(ctx, patch)
	if err != nil {
		return OutcomeApplyRejected, err
	}
	if len(patch.Operations) == 0 {
		return OutcomeApplyNoop, nil
	}
	patch = s.redactPatch(patch) // Apply redaction only after raw credentials and control text were rejected.
	applied, err := s.store.ApplyMemoryPatch(ctx, patch, s.cfg.Limits)
	if err != nil {
		return OutcomeApplyRejected, err
	}
	if !applied {
		return OutcomeApplyNoop, nil
	}
	for _, op := range patch.Operations {
		if op.Type == domain.MemoryOpCreateTopic {
			return OutcomeApplyCreated, nil
		}
	}
	return OutcomeApplyUpdated, nil
}

// withoutExistingCreates prevents a curator retry from repeatedly failing when
// it proposes a topic slug that was already persisted by an earlier exchange.
func (s *Service) withoutExistingCreates(ctx context.Context, patch domain.MemoryPatch) (domain.MemoryPatch, error) {
	ownerKey := domain.SlackOwnerKey(patch.ConversationKey, patch.SourceAuthorID)
	operations := make([]domain.MemoryOp, 0, len(patch.Operations))
	created := make(map[string]struct{})
	for _, op := range patch.Operations {
		if op.Type != domain.MemoryOpCreateTopic {
			operations = append(operations, op)
			continue
		}

		slug := op.TopicSlug
		if op.BundlePath == "people" {
			slug = domain.ScopedPersonTopicSlug(slug, ownerKey)
		}
		if _, exists := created[slug]; exists {
			// Preserve duplicate operations in one patch so SQLite rejects the
			// malformed patch atomically instead of silently dropping data.
			operations = append(operations, op)
			continue
		}
		exists, err := s.store.TopicExistsBySlug(ctx, slug)
		if err != nil {
			return domain.MemoryPatch{}, fmt.Errorf("check existing memory topic %q: %w", slug, err)
		}
		if exists {
			continue
		}
		created[slug] = struct{}{}
		operations = append(operations, op)
	}
	patch.Operations = operations
	return patch, nil
}

// Validate checks a proposed patch without writing it, allowing optional
// curator output to be discarded safely when it makes a merged patch unsafe.
func (s *Service) Validate(patch domain.MemoryPatch) error {
	return s.validatePatch(patch)
}

func (s *Service) validatePatch(patch domain.MemoryPatch) error {
	reasons := make([]string, 0)
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }
	if len(patch.Operations) > s.cfg.MaxPatchOps {
		add("patch has %d operations; maximum is %d", len(patch.Operations), s.cfg.MaxPatchOps)
	}
	for _, field := range []struct{ name, value string }{
		{"conversation key", string(patch.ConversationKey)}, {"exchange timestamp", patch.ExchangeTS}, {"source author", patch.SourceAuthorID},
	} {
		if err := domain.ValidateMemoryReferenceText(field.value); err != nil {
			add("patch: %s: %v", field.name, err)
		}
	}
	for i, op := range patch.Operations {
		prefix := fmt.Sprintf("operation %d (%s)", i, op.Type)
		if err := domain.ValidateMemoryReferenceText(op.Type); err != nil {
			add("%s: operation type: %v", prefix, err)
		}
		if !domain.ValidMemoryOps[op.Type] {
			add("%s: unknown operation type %q", prefix, op.Type)
			continue
		}
		if err := domain.ValidateSlug(op.TopicSlug); err != nil {
			add("%s: %v", prefix, err)
		}
		for _, field := range []struct{ name, value string }{
			{"topic slug", op.TopicSlug}, {"target topic slug", op.TargetTopicSlug},
			{"topic title", op.TopicTitle}, {"topic description", op.TopicDesc}, {"content", op.Content},
			{"change reason", op.ChangeReason}, {"decision", op.Decision}, {"question", op.Question},
			{"link relation", op.LinkRelation},
		} {
			if err := domain.ValidateMemoryReferenceText(field.value); err != nil {
				add("%s: %s: %v", prefix, field.name, err)
			}
		}
		for _, tag := range op.Tags {
			if err := domain.ValidateMemoryReferenceText(tag); err != nil {
				add("%s: tag: %v", prefix, err)
			}
		}
		switch op.Type {
		case domain.MemoryOpCreateTopic:
			if err := domain.ValidateTopicTitle(op.TopicTitle); err != nil {
				add("%s: %v", prefix, err)
			}
			if err := domain.ValidateTopicContent(op.Content, s.cfg.Limits.MaxTopicChars); err != nil {
				add("%s: %v", prefix, err)
			}
		case domain.MemoryOpRevise, domain.MemoryOpCorrect:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if err := domain.ValidateTopicContent(op.Content, s.cfg.Limits.MaxTopicChars); err != nil {
				add("%s: %v", prefix, err)
			}
		case domain.MemoryOpDecide:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if strings.TrimSpace(op.Decision) == "" {
				add("%s: decision text must not be empty", prefix)
			}
		case domain.MemoryOpQuestionAdd, domain.MemoryOpQuestionResolve:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if strings.TrimSpace(op.Question) == "" {
				add("%s: question text must not be empty", prefix)
			}
		case domain.MemoryOpLinkAdd, domain.MemoryOpLinkRemove:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if err := domain.ValidateSlug(op.TargetTopicSlug); err != nil {
				add("%s: target topic: %v", prefix, err)
			}
			if op.Type == domain.MemoryOpLinkAdd && strings.TrimSpace(op.LinkRelation) == "" {
				add("%s: link relation must not be empty", prefix)
			}
		}
	}
	if len(reasons) > 0 {
		return &domain.MemoryValidationError{Reasons: reasons}
	}
	return nil
}

func (s *Service) redactPatch(patch domain.MemoryPatch) domain.MemoryPatch {
	patch.Operations = append([]domain.MemoryOp(nil), patch.Operations...)
	patch.SourceAuthorID = s.sanitize(patch.SourceAuthorID)
	for i := range patch.Operations {
		op := &patch.Operations[i]
		op.Tags = append([]string(nil), op.Tags...)
		op.Type = s.sanitize(op.Type)
		op.TopicSlug = s.sanitize(op.TopicSlug)
		op.TargetTopicSlug = s.sanitize(op.TargetTopicSlug)
		op.TopicTitle = s.sanitize(op.TopicTitle)
		op.TopicDesc = s.sanitize(op.TopicDesc)
		op.Content = s.sanitize(op.Content)
		op.ChangeReason = s.sanitize(op.ChangeReason)
		op.Decision = s.sanitize(op.Decision)
		op.Question = s.sanitize(op.Question)
		op.LinkRelation = s.sanitize(op.LinkRelation)
		for j := range op.Tags {
			op.Tags[j] = s.sanitize(op.Tags[j])
		}
	}
	return patch
}
