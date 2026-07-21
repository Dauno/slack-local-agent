package port

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

type ConversationStore interface {
	ClaimDedupe(ctx context.Context, keys []string, createdAt, expiresAt time.Time) (bool, error)
	HasAssistantMessage(ctx context.Context, key domain.ConversationKey) (bool, error)
	RecentMessages(ctx context.Context, key domain.ConversationKey, limit int) ([]domain.Message, error)
	AppendMessage(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, retain int) error
	CleanupDedupe(ctx context.Context, now time.Time) error
}

// AgentRequest bundles conversation history, recalled memory, and enriched
// context into one model call. Future facts stay out of the bot use case.
type AgentRequest struct {
	ConversationKey domain.ConversationKey
	Messages        []domain.Message
	Memory          []domain.MemorySnippet
	Context         domain.AgentContext
}

// --- Structured agent runtime (replaces Agent.Respond for tool-aware turns) ---

// AgentTurn is the structured result of one agent invocation. It carries
// assistant text, a provider-neutral Presentation, or a PendingConfirmation.
type AgentTurn struct {
	Text                string
	PendingConfirmation *domain.PendingConfirmation
	Presentation        *domain.Presentation
}

// AgentRuntime runs one request or resumes a pending confirmation.
type AgentRuntime interface {
	Run(ctx context.Context, request AgentRequest) (AgentTurn, error)
	Resume(ctx context.Context, decision domain.ConfirmationDecision) (AgentTurn, error)
}

// AgentToolFactory creates a set of ADK tools scoped to an actor and
// conversation. It is called once per agent turn before the model call.
// Returning no tools is valid and produces a text-only agent. A non-nil
// error must fail the turn: a partial tool list would make runtime behavior
// differ from validated configuration.
//
// The factory returns []tool.Tool directly (ADK concrete types) to avoid
// the Go generics problem with functiontool construction. The adapter
// only imports the tool package, not the individual tool implementations.
type AgentToolFactory interface {
	ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error)
}

// ContextEnricher resolves a bounded, structured view of the invoking Slack
// user and conversation before a primary model call. Slack API failures and
// missing scopes must never prevent a normal response.
type ContextEnricher interface {
	Enrich(ctx context.Context, invocation domain.Invocation) (domain.AgentContext, error)
}

// ErrModelCallLimitReached indicates that the process-wide model-call budget is
// exhausted. Callers can use it to apply their own backpressure behavior.
var ErrModelCallLimitReached = errors.New("maximum concurrent model calls reached")

// ModelCallLimiter bounds all model calls made by one running agent process.
// The composition root supplies one instance to both foreground and background
// model consumers.
type ModelCallLimiter interface {
	TryAcquire() (release func(), acquired bool)
}

type History struct {
	Messages        []domain.Message
	BotParticipated bool
}

type HistoryReader interface {
	RecentHistory(ctx context.Context, invocation domain.Invocation, limits domain.ContextLimits) (History, error)
}

type ResponsePublisher interface {
	Publish(ctx context.Context, target domain.ReplyTarget, text string) (PublishedResponse, error)
}

type PublishedResponse struct {
	LastMessageTS string
}

// PreparedAssistantExchange is returned before publication. CorrelationID is
// attached to every Slack chunk and is required for crash recovery.
type PreparedAssistantExchange struct {
	ID            string
	CorrelationID string
}

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type Clock interface {
	Now() time.Time
}

// MemoryRetriever provides synchronous recall of curated memory topics.
// It is called before each model invocation and must never block the normal
// response path.
type MemoryRetriever interface {
	Recall(ctx context.Context, query, ownerKey string) ([]domain.MemorySnippet, error)
}

// AssistantExchangeWriter durably stages an assistant exchange before it is
// published, then finalizes it and its curation work item after publishing.
// A staged exchange can be reconciled if the post-publish database write fails.
type AssistantExchangeWriter interface {
	PrepareAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, retain int, memoryEligible bool) (PreparedAssistantExchange, error)
	PrepareStructuredAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, presentationJSON string, retain int, memoryEligible bool) (PreparedAssistantExchange, error)
	MarkAssistantExchangePublished(ctx context.Context, intentID, assistantTS string) error
	FinalizeAssistantExchange(ctx context.Context, intentID string) error
	DiscardAssistantExchange(ctx context.Context, intentID string) error
	ReconcileAssistantExchanges(ctx context.Context, finder AssistantExchangeFinder) error
}

// AssistantExchangeIntent is the bounded data required to prove that a
// prepared reply was accepted by Slack after a process crash.
type AssistantExchangeIntent struct {
	ID               string
	ChannelID        string
	ChannelKind      domain.ChannelKind
	RootTS           string
	Content          string
	CorrelationID    string
	PresentationJSON string
}

// AssistantExchangeFinder returns an actual Slack timestamp only when every
// Slack reply chunk exposes the exact durable CorrelationID. Content and time
// alone must never finalize a prepared exchange.
type AssistantExchangeFinder interface {
	FindPublishedAssistantExchange(ctx context.Context, intent AssistantExchangeIntent) (assistantTS string, found bool, err error)
}

// MemoryStore owns topic CRUD, revision history, outbox claims, retention, and
// provenance. It is a low-level data access interface for SQLite-backed memory.
type MemoryStore interface {
	CreateTopic(ctx context.Context, slug, title, description string, tags []string, content, changeReason string) (domain.Topic, error)
	GetTopic(ctx context.Context, slug string) (domain.Topic, error)
	GetTopicByID(ctx context.Context, id domain.TopicID) (domain.Topic, *domain.TopicRevision, error)
	ListTopics(ctx context.Context) ([]domain.Topic, error)
	DeleteTopic(ctx context.Context, id domain.TopicID) error

	AddRevision(ctx context.Context, topicID domain.TopicID, expectedRev int, content, changeReason string) (domain.TopicRevision, error)
	AddEvidence(ctx context.Context, revisionID int, sourceKey domain.ConversationKey, sourceTS, authorID string, evidenceType domain.EvidenceType) (int, error)
	AddEvidenceBatch(ctx context.Context, evidence []domain.Evidence) error
	GetEvidence(ctx context.Context, topicID domain.TopicID) ([]domain.Evidence, error)
	ListRevisions(ctx context.Context, topicID domain.TopicID) ([]domain.TopicRevision, error)

	SearchTopics(ctx context.Context, query string, maxTopics, maxChars int) ([]domain.MemorySnippet, error)
	SearchTopicsForOwner(ctx context.Context, query, ownerKey string, maxTopics, maxChars int) ([]domain.MemorySnippet, error)
	SearchPersonTopicsByOwner(ctx context.Context, ownerKey string, maxTopics, maxChars int) ([]domain.MemorySnippet, error)
	SearchTopicReferences(ctx context.Context, query string, maxTopics int) ([]domain.TopicReference, error)
	GetTopicReference(ctx context.Context, slug string) (*domain.TopicReference, error)
	FindSimilarTopic(ctx context.Context, title string) (*domain.Topic, error)
	TopicExistsBySlug(ctx context.Context, slug string) (bool, error)

	AddTopicLink(ctx context.Context, sourceID, targetID domain.TopicID, relation string, revisionID int) error
	RemoveTopicLink(ctx context.Context, sourceID, targetID domain.TopicID) error
	GetTopicLinks(ctx context.Context, topicID domain.TopicID) ([]domain.TopicLink, error)
	ApplyMemoryPatch(ctx context.Context, patch domain.MemoryPatch, limits domain.MemoryLimits) (bool, error)

	EnqueueOutboxItem(ctx context.Context, conversationKey domain.ConversationKey, exchangeTS string) error
	ClaimNextOutboxItem(ctx context.Context) (*domain.OutboxItem, error)
	LoadOutboxMessages(ctx context.Context, item *domain.OutboxItem) ([]domain.Message, error)
	CompleteOutboxItem(ctx context.Context, id int, leaseUntil time.Time) error
	FailOutboxItem(ctx context.Context, id int, leaseUntil time.Time, reason string) error
	RetryOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error
	RescheduleOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error
	CleanupOutbox(ctx context.Context, before time.Time) error
}

// MemoryCurator receives one completed exchange and returns a schema-validated
// patch proposal. It may use an LLM internally but cannot write storage
// directly or change memory policy.
type MemoryCurator interface {
	ProposePatch(ctx context.Context, conversationKey domain.ConversationKey, exchangeTS string, messages []domain.Message, topics []domain.TopicReference) (domain.MemoryPatch, error)
}

// ProjectionSnapshot holds a consistent point-in-time view of all memory state
// required to render an OKF bundle. It is read under a single transaction.
type ProjectionSnapshot struct {
	Topics    []domain.Topic
	Revisions map[domain.TopicID][]domain.TopicRevision
	Links     map[domain.TopicID][]domain.TopicLink
	Evidence  map[domain.TopicID][]domain.Evidence
}

// ProjectionReader returns a consistent snapshot of the memory store suitable
// for projecting an OKF bundle. It must be read under a single transaction.
type ProjectionReader interface {
	ReadProjectionSnapshot(ctx context.Context) (ProjectionSnapshot, error)
}

// OKFProjector materializes committed SQLite memory state into an Open
// Knowledge Format bundle on the filesystem. It is never a writable source of
// truth.
type OKFProjector interface {
	Project(ctx context.Context, reader ProjectionReader, outputDir string) error
}

// --- Attachment handling ---

type LoadedAttachment struct {
	ID       string
	Name     string
	MIMEType string
	Data     []byte
}

type FileLoader interface {
	Load(ctx context.Context, attachment domain.Attachment, maxBytes int64) (LoadedAttachment, error)
}

type AttachmentRequest struct {
	ProcessingID string
	UserID       string
	Attachment   LoadedAttachment
}

type ProcessedAttachment struct {
	Name     string
	MIMEType string
	Text     string
}

type AttachmentProcessor interface {
	Process(ctx context.Context, request AttachmentRequest) (ProcessedAttachment, error)
}

// --- Confirmation delivery (Phase 2) ---

// ConfirmationDelivery represents a durable bridge between an ADK confirmation
// event and Slack publication.
type ConfirmationDelivery struct {
	WrapperCallID   string
	OriginalCallID  string
	SessionID       string
	Actor           string
	TeamID          string
	ChannelID       string
	ThreadTS        string
	ConversationKey domain.ConversationKey
	Summary         string
	ParameterHash   string
	Status          ConfirmationDeliveryStatus
	CorrelationID   string
	SlackMessageTS  string
	RendererMode    string
	Expiry          time.Time
}

// ConfirmationContentDigest binds a rendered confirmation to its durable
// identity and presentation without exposing tool parameters.
func ConfirmationContentDigest(delivery ConfirmationDelivery) string {
	canonical, _ := json.Marshal(struct {
		WrapperCallID  string `json:"wrapper_call_id"`
		OriginalCallID string `json:"original_call_id"`
		Actor          string `json:"actor"`
		TeamID         string `json:"team_id"`
		ChannelID      string `json:"channel_id"`
		ThreadTS       string `json:"thread_ts"`
		Summary        string `json:"summary"`
		ParameterHash  string `json:"parameter_hash"`
		Expiry         int64  `json:"expiry"`
	}{
		WrapperCallID: delivery.WrapperCallID, OriginalCallID: delivery.OriginalCallID,
		Actor: delivery.Actor, TeamID: delivery.TeamID, ChannelID: delivery.ChannelID,
		ThreadTS: delivery.ThreadTS, Summary: delivery.Summary,
		ParameterHash: delivery.ParameterHash, Expiry: delivery.Expiry.Unix(),
	})
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
}

type ConfirmationDeliveryStatus string

const (
	ConfirmationPending   ConfirmationDeliveryStatus = "pending"
	ConfirmationPublished ConfirmationDeliveryStatus = "published"
	ConfirmationApproved  ConfirmationDeliveryStatus = "approved"
	ConfirmationRejected  ConfirmationDeliveryStatus = "rejected"
	ConfirmationExpired   ConfirmationDeliveryStatus = "expired"
	ConfirmationConsumed  ConfirmationDeliveryStatus = "consumed"
	ConfirmationFailed    ConfirmationDeliveryStatus = "failed"
)

// ConfirmationDeliveryStore persists and retrieves confirmation deliveries.
type ConfirmationDeliveryStore interface {
	CreateDelivery(ctx context.Context, delivery ConfirmationDelivery) error
	MarkPublished(ctx context.Context, wrapperCallID, correlationID, slackMessageTS, rendererMode string) error
	MarkConsumed(ctx context.Context, wrapperCallID string) error
	RejectDelivery(ctx context.Context, wrapperCallID string) error
	GetByWrapperCallID(ctx context.Context, wrapperCallID string) (*ConfirmationDelivery, error)
	ListPending(ctx context.Context) ([]ConfirmationDelivery, error)
	ExpireDeliveries(ctx context.Context, now time.Time) error
}

// StructuredPublisher renders provider-neutral structured response data
// (Presentation) using provider-specific rich presentation (e.g. Slack
// Block Kit context and table blocks). It is selected by the bot only when
// the turn contains a validated Presentation.
type StructuredPublisher interface {
	ValidateStructured(presentation domain.Presentation) error
	PublishStructured(ctx context.Context, target domain.ReplyTarget, presentation domain.Presentation) (PublishedResponse, error)
}

// ConfirmationPublisher publishes and updates confirmation prompts using
// provider-specific rich presentation (e.g. Slack Block Kit buttons).
type ConfirmationPublisher interface {
	PublishConfirmation(ctx context.Context, delivery ConfirmationDelivery) (ConfirmationPublishedResult, error)
	RecoverConfirmation(ctx context.Context, delivery ConfirmationDelivery) (ConfirmationPublishedResult, bool, error)
	UpdateConfirmation(ctx context.Context, delivery ConfirmationDelivery, terminalText string) error
}

// ConfirmationPublishedResult carries the opaque message identifier
// returned by the provider when a confirmation prompt is published.
type ConfirmationPublishedResult struct {
	SlackMessageTS string
}

// ExternalAgentRuntime invokes an ACP-compatible external agent.
type ExternalAgentRuntime interface {
	Run(ctx context.Context, request domain.AcpInvocationRequest) (domain.AcpInvocationResult, error)
	Probe(ctx context.Context, primaryPath string, additionalPaths []string, configOptions []domain.ACPConfigOption) error
	Describe(ctx context.Context) (domain.ACPInitResult, error)
}

// OpenCodeManager handles OpenCode lifecycle operations.
type OpenCodeManager interface {
	Status(ctx context.Context) (domain.OpenCodeManagementResult, error)
	Probe(ctx context.Context) error
	Upgrade(ctx context.Context) (domain.OpenCodeManagementResult, error)
	Rollback(ctx context.Context) (domain.OpenCodeManagementResult, error)
}

type OpenCodeCoordinator interface {
	TryInvocation() (release func(), acquired bool)
	TryMaintenance() (release func(), acquired bool)
}

type CanvasCreateResult struct {
	CanvasID string
}

type CanvasCreateError struct {
	Err       error
	Ambiguous bool
}

func (e *CanvasCreateError) Error() string { return e.Err.Error() }
func (e *CanvasCreateError) Unwrap() error { return e.Err }

type CanvasCreator interface {
	CreateCanvas(ctx context.Context, title string, documentContent string) (CanvasCreateResult, error)
}

type CanvasOperationStore interface {
	CreateOperation(ctx context.Context, op domain.CanvasOperation) error
	UpdateOperationStatus(ctx context.Context, operationID string, status domain.CanvasOperationStatus, canvasID string) error
	GetOperation(ctx context.Context, operationID string) (*domain.CanvasOperation, error)
}

var ErrCanvasOperationExists = errors.New("canvas operation already exists")
