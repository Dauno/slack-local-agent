package toolfactory_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

type stubConversationStore struct {
	messages []domain.Message
}

var _ port.ConversationStore = (*stubConversationStore)(nil)

func (s *stubConversationStore) ClaimDedupe(_ context.Context, _ []string, _, _ time.Time) (bool, error) {
	return true, nil
}
func (s *stubConversationStore) HasAssistantMessage(_ context.Context, _ domain.ConversationKey) (bool, error) {
	return false, nil
}
func (s *stubConversationStore) RecentMessages(_ context.Context, _ domain.ConversationKey, limit int) ([]domain.Message, error) {
	return s.messages[:min(limit, len(s.messages))], nil
}
func (s *stubConversationStore) AppendMessage(_ context.Context, _ domain.ConversationMetadata, _ domain.Message, _ int) error {
	return nil
}
func (s *stubConversationStore) CleanupDedupe(_ context.Context, _ time.Time) error { return nil }

type stubAuditStore struct {
	records []domain.ToolAuditRecord
	updates []domain.ToolLifecycleState
}

func (s *stubAuditStore) InsertAudit(_ context.Context, record domain.ToolAuditRecord) error {
	s.records = append(s.records, record)
	return nil
}
func (s *stubAuditStore) UpdateAuditState(_ context.Context, _ string, state domain.ToolLifecycleState, _ time.Time) error {
	s.updates = append(s.updates, state)
	return nil
}
func (s *stubAuditStore) GetAuditByCallID(_ context.Context, _ string) (*domain.ToolAuditRecord, error) {
	return nil, nil
}

type stubExecutor struct {
	listReposResult string
	operations      []sandboxusecase.SandboxOperation
}

type stubCanvasCreator struct{}

func (stubCanvasCreator) CreateCanvas(context.Context, string, string) (port.CanvasCreateResult, error) {
	return port.CanvasCreateResult{CanvasID: "F123"}, nil
}

type stubCanvasStore struct{}

func (stubCanvasStore) CreateOperation(context.Context, domain.CanvasOperation) error { return nil }
func (stubCanvasStore) UpdateOperationStatus(context.Context, string, domain.CanvasOperationStatus, string) error {
	return nil
}
func (stubCanvasStore) GetOperation(context.Context, string) (*domain.CanvasOperation, error) {
	return nil, nil
}

func (s *stubExecutor) Execute(_ context.Context, op sandboxusecase.SandboxOperation) (sandboxusecase.SandboxResult, error) {
	s.operations = append(s.operations, op)
	switch op.Capability {
	case domain.CapListRepos:
		return sandboxusecase.SandboxResult{Output: s.listReposResult}, nil
	case domain.CapListDirectory:
		return sandboxusecase.SandboxResult{Output: "main.go\ninternal/", Truncated: true}, nil
	case domain.CapReadFile:
		return sandboxusecase.SandboxResult{Output: "package main\n\nfunc main() {}", Truncated: false}, nil
	}
	return sandboxusecase.SandboxResult{}, errors.New("unsupported")
}

type stubToolContext struct {
	agent.ContextMock
	callID string
	ctx    context.Context
}

func (c *stubToolContext) FunctionCallID() string      { return c.callID }
func (c *stubToolContext) Deadline() (time.Time, bool) { return c.context().Deadline() }
func (c *stubToolContext) Done() <-chan struct{}       { return c.context().Done() }
func (c *stubToolContext) Err() error                  { return c.context().Err() }
func (c *stubToolContext) Value(key any) any           { return c.context().Value(key) }
func (c *stubToolContext) context() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

type runnableFunctionTool interface {
	Name() string
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
}

func TestFactoryWithoutSandboxExposesOnlyConversationTools(t *testing.T) {
	store := &stubConversationStore{}
	f := toolfactory.New(store, nil, nil)
	if f == nil {
		t.Fatal("factory should not be nil")
	}
	tools, err := f.ToolsForInvocation("U12345678", domain.ConversationKey("test:conv"))
	if err != nil {
		t.Fatalf("ToolsForInvocation error = %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool without sandbox, got %d", len(tools))
	}
}

func TestFactoryWithSandboxExposesAllReadOnlyTools(t *testing.T) {
	store := &stubConversationStore{}
	audit := &stubAuditStore{}
	executor := &stubExecutor{}
	sb, err := sandboxusecase.New(sandboxusecase.Config{
		AllowedCapabilities: []domain.Capability{
			domain.CapListRepos, domain.CapListDirectory, domain.CapReadFile, domain.CapListWorktrees,
		},
		CommandTimeout: 30 * time.Second,
		MaxOutputBytes: 65536,
	}, sandboxusecase.Dependencies{
		AuditStore: audit,
		Executor:   executor,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := toolfactory.New(store, sb, nil)
	if f == nil {
		t.Fatal("factory should not be nil")
	}
	tools, err := f.ToolsForInvocation("U12345678", domain.ConversationKey("test:conv"))
	if err != nil {
		t.Fatalf("ToolsForInvocation error = %v", err)
	}
	if len(tools) != 5 { // list_messages + 4 sandbox tools
		t.Fatalf("expected 5 tools with sandbox, got %d", len(tools))
	}
	wantNames := []string{"list_messages", "list_repos", "list_directory", "read_file", "list_worktrees"}
	var listDirectory runnableFunctionTool
	for i, candidate := range tools {
		named, ok := candidate.(interface{ Name() string })
		if !ok {
			t.Fatalf("tool %d does not expose a name: %T", i, candidate)
		}
		if named.Name() != wantNames[i] {
			t.Fatalf("tool %d name = %q, want %q", i, named.Name(), wantNames[i])
		}
		if named.Name() == "list_directory" {
			listDirectory, ok = candidate.(runnableFunctionTool)
			if !ok {
				t.Fatalf("list_directory is not runnable: %T", candidate)
			}
		}
	}
	if listDirectory == nil {
		t.Fatal("list_directory tool not found")
	}

	declaration := listDirectory.Declaration()
	schemaData, err := json.Marshal(declaration.ParametersJsonSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["project"]; !ok {
		t.Fatalf("list_directory schema has no project property: %s", schemaData)
	}
	if _, ok := schema.Properties["path"]; !ok {
		t.Fatalf("list_directory schema has no path property: %s", schemaData)
	}
	if !reflect.DeepEqual(schema.Required, []string{"project"}) {
		t.Fatalf("list_directory required fields = %v, want [project]", schema.Required)
	}

	result, err := listDirectory.Run(&stubToolContext{callID: "call-list-directory"}, map[string]any{
		"project": "workspace",
		"path":    ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := result["entries"]
	if !reflect.DeepEqual(entries, []any{"main.go", "internal/"}) &&
		!reflect.DeepEqual(entries, []string{"main.go", "internal/"}) {
		t.Fatalf("list_directory entries = %#v", entries)
	}
	if truncated, ok := result["truncated"].(bool); !ok || !truncated {
		t.Fatalf("list_directory truncated = %#v", result["truncated"])
	}
	if len(executor.operations) != 1 {
		t.Fatalf("executor operations = %d, want 1", len(executor.operations))
	}
	op := executor.operations[0]
	if op.Capability != domain.CapListDirectory || op.Actor != "U12345678" || op.Args["project"] != "workspace" || op.Args["path"] != "." {
		t.Fatalf("executor operation = %#v", op)
	}
	if len(audit.records) != 1 || audit.records[0].OriginalCallID != "call-list-directory" || audit.records[0].Capability != domain.CapListDirectory {
		t.Fatalf("audit records = %#v", audit.records)
	}
	if !reflect.DeepEqual(audit.updates, []domain.ToolLifecycleState{domain.ToolStateRunning, domain.ToolStateCompleted}) {
		t.Fatalf("audit updates = %v", audit.updates)
	}
}

func TestFactoryExposesCanvasWithoutSandbox(t *testing.T) {
	svc, err := canvasusecase.New(canvasusecase.Config{}, canvasusecase.Dependencies{
		Creator: stubCanvasCreator{}, Store: stubCanvasStore{}, SanitizeContent: func(value string) string { return value },
	})
	if err != nil {
		t.Fatal(err)
	}
	f := toolfactory.New(&stubConversationStore{}, nil, svc)
	tools, err := f.ToolsForInvocation("U12345678", "slack:T12345678:dm:D12345678")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want list_messages and create_canvas", len(tools))
	}
	if named, ok := tools[1].(interface{ Name() string }); !ok || named.Name() != "create_canvas" {
		t.Fatalf("second tool = %T, want create_canvas", tools[1])
	}
}

func TestFactoryNilStoreReturnsNil(t *testing.T) {
	f := toolfactory.New(nil, nil, nil)
	if f != nil {
		t.Fatal("factory with nil store should be nil")
	}
}
