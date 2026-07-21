// Package toolfactory creates ADK function tools scoped to an actor and
// conversation. Read-only tools are registered unconditionally; mutable
// tools carry RequireConfirmation and delegate authorization to the sandbox.
package toolfactory

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

var _ port.AgentToolFactory = (*Factory)(nil)

// Factory implements port.AgentToolFactory by producing typed ADK function
// tools for the invoking actor and conversation.
type Factory struct {
	store   port.ConversationStore
	sandbox *sandboxusecase.Service
	canvas  *canvasusecase.Service
}

// New creates a tool factory. Sandbox and canvas services may be nil — when
// absent, only the conversation list_messages tool is registered.
func New(store port.ConversationStore, sb *sandboxusecase.Service, cv *canvasusecase.Service) *Factory {
	if store == nil {
		return nil
	}
	return &Factory{store: store, sandbox: sb, canvas: cv}
}

// ToolsForInvocation implements port.AgentToolFactory. A tool construction
// failure returns an error instead of a partial tool list.
func (f *Factory) ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error) {
	if f == nil || f.store == nil {
		return nil, nil
	}

	tools := make([]any, 0, 8)

	// Conversation tool.
	ro, err := f.listMessagesTool(key)
	if err != nil {
		return nil, fmt.Errorf("build list_messages tool: %w", err)
	}
	tools = append(tools, ro)

	if f.sandbox != nil {
		// Read-only sandbox tools.
		listRepos, err := f.listReposTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build list_repos tool: %w", err)
		}
		tools = append(tools, listRepos)

		listDirectory, err := f.listDirectoryTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build list_directory tool: %w", err)
		}
		tools = append(tools, listDirectory)

		readFile, err := f.readFileTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build read_file tool: %w", err)
		}
		tools = append(tools, readFile)

		listWorktrees, err := f.listWorktreesTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build list_worktrees tool: %w", err)
		}
		tools = append(tools, listWorktrees)
	}

	if f.canvas != nil {
		createCanvas, err := f.createCanvasTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build create_canvas tool: %w", err)
		}
		tools = append(tools, createCanvas)
	}

	return tools, nil
}

// --- read-only: conversation ---

type listMessagesArgs struct {
	Limit int `json:"limit,omitzero" jsonschema:"maximum number of messages to retrieve (default 5, max 20)"`
}

type listMessagesResult struct {
	Messages []messageItem `json:"messages"`
	Count    int           `json:"count"`
}

type messageItem struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

func (f *Factory) listMessagesTool(key domain.ConversationKey) (tool.Tool, error) {
	store := f.store
	conversationKey := key
	return functiontool.New(
		functiontool.Config{
			Name:        "list_messages",
			Description: "Lists recent messages from the current conversation. Read-only — no mutations.",
		},
		func(ctx agent.Context, args listMessagesArgs) (listMessagesResult, error) {
			limit := args.Limit
			if limit <= 0 || limit > 20 {
				limit = 5
			}
			msgs, err := store.RecentMessages(ctx, conversationKey, limit)
			if err != nil {
				return listMessagesResult{}, fmt.Errorf("read messages: %w", err)
			}
			result := listMessagesResult{
				Messages: make([]messageItem, 0, len(msgs)),
				Count:    len(msgs),
			}
			for _, m := range msgs {
				result.Messages = append(result.Messages, messageItem{
					Role: string(m.Role), Content: m.Content,
					Timestamp: m.CreatedAt.Format(time.RFC3339),
				})
			}
			return result, nil
		},
	)
}

// --- read-only: sandbox ---

type listReposResult struct {
	Repos []string `json:"repos"`
}

func (f *Factory) listReposTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "list_repos",
			Description: "Lists pre-registered project repositories available for read-only inspection. Returned names are the only valid project names for filesystem tools.",
		},
		func(ctx agent.Context, _ struct{}) (listReposResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapListRepos, nil, actor)
			if err != nil {
				return listReposResult{}, err
			}
			return listReposResult{Repos: splitNonEmpty(result.Output)}, nil
		},
	)
}

type listDirectoryArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Path    string `json:"path,omitzero" jsonschema:"project-relative directory path (defaults to '.')"`
}

type listDirectoryResult struct {
	Entries   []string `json:"entries"`
	Truncated bool     `json:"truncated"`
}

func (f *Factory) listDirectoryTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "list_directory",
			Description: "Lists directory contents non-recursively within a pre-registered project. Directory names end with '/'. Start with path '.' for the project root, then traverse subdirectories. Read-only -- no mutations.",
		},
		func(ctx agent.Context, args listDirectoryArgs) (listDirectoryResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapListDirectory,
				map[string]any{"project": args.Project, "path": args.Path}, actor)
			if err != nil {
				return listDirectoryResult{}, err
			}
			return listDirectoryResult{Entries: splitNonEmpty(result.Output), Truncated: result.Truncated}, nil
		},
	)
}

type readFileArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Path    string `json:"path" jsonschema:"path to the file within the project"`
}

type readFileResult struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

func (f *Factory) readFileTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "read_file",
			Description: "Reads a file from a pre-registered project. Read-only -- no mutations.",
		},
		func(ctx agent.Context, args readFileArgs) (readFileResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapReadFile,
				map[string]any{"project": args.Project, "path": args.Path}, actor)
			if err != nil {
				return readFileResult{}, err
			}
			return readFileResult{Content: result.Output, Truncated: result.Truncated}, nil
		},
	)
}

type listWorktreesArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
}

type listWorktreesResult struct {
	Worktrees []string `json:"worktrees"`
}

func (f *Factory) listWorktreesTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "list_worktrees",
			Description: "Lists git worktrees for a project. Read-only — no mutations.",
		},
		func(ctx agent.Context, args listWorktreesArgs) (listWorktreesResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapListWorktrees,
				map[string]any{"project": args.Project}, actor)
			if err != nil {
				return listWorktreesResult{}, err
			}
			return listWorktreesResult{Worktrees: splitNonEmpty(result.Output)}, nil
		},
	)
}

// --- mutable: sandbox (native ADK confirmation) ---

type createWorktreeArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Name    string `json:"name" jsonschema:"name for the new worktree"`
}

type createWorktreeResult struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

func (f *Factory) createWorktreeTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:                "create_worktree",
			Description:         "Creates a new git worktree in a project. Requires user confirmation.",
			RequireConfirmation: true,
		},
		func(ctx agent.Context, args createWorktreeArgs) (createWorktreeResult, error) {
			callID := ctx.FunctionCallID()
			_, err := sb.Run(ctx, callID, domain.CapCreateWorktree,
				map[string]any{"project": args.Project, "name": args.Name}, actor)
			if err != nil {
				return createWorktreeResult{Status: "failed"}, err
			}
			return createWorktreeResult{Status: "created", Name: args.Name}, nil
		},
	)
}

type removeWorktreeArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Name    string `json:"name" jsonschema:"name of the worktree to remove"`
}

type removeWorktreeResult struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

func (f *Factory) removeWorktreeTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:                "remove_worktree",
			Description:         "Removes a git worktree from a project. Requires user confirmation.",
			RequireConfirmation: true,
		},
		func(ctx agent.Context, args removeWorktreeArgs) (removeWorktreeResult, error) {
			callID := ctx.FunctionCallID()
			_, err := sb.Run(ctx, callID, domain.CapRemoveWorktree,
				map[string]any{"project": args.Project, "name": args.Name}, actor)
			if err != nil {
				return removeWorktreeResult{Status: "failed"}, err
			}
			return removeWorktreeResult{Status: "removed", Name: args.Name}, nil
		},
	)
}

func splitNonEmpty(s string) []string {
	if s == "" || s == "(no worktrees)" {
		return nil
	}
	var out []string
	for _, line := range splitLines(s) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

type createCanvasArgs struct {
	Title   string `json:"title" jsonschema:"Canvas title (required, max 150 characters)"`
	Content string `json:"content" jsonschema:"Canvas body in standard Markdown (required, max 50,000 characters)"`
}

type createCanvasResult struct {
	CanvasID string `json:"canvas_id"`
	Message  string `json:"message"`
}

func (f *Factory) createCanvasTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	svc := f.canvas

	return functiontool.New(functiontool.Config{
		Name:        "create_canvas",
		Description: "Creates a persistent Slack Canvas document with the given title and Markdown content. Requires explicit user confirmation before creation.",
		RequireConfirmationProvider: func(args createCanvasArgs) bool {
			return svc.ValidateCanvas(args.Title, args.Content) == nil
		},
	}, func(ctx agent.Context, args createCanvasArgs) (createCanvasResult, error) {
		callID := ctx.FunctionCallID()
		if callID == "" {
			return createCanvasResult{}, fmt.Errorf("create canvas: function call ID is required")
		}
		operationDigest := sha256.Sum256([]byte(string(key) + "\x00" + callID))
		callID = fmt.Sprintf("canvas:%x", operationDigest)
		result, err := svc.CreateCanvas(ctx, callID, key, actor, args.Title, args.Content)
		if err != nil {
			return createCanvasResult{Message: fmt.Sprintf("Failed to create Canvas: %v", err)}, err
		}
		return createCanvasResult{
			CanvasID: result.CanvasID,
			Message:  fmt.Sprintf("Canvas created: %s", canvasURL(key, result.CanvasID)),
		}, nil
	})
}

func canvasURL(key domain.ConversationKey, canvasID string) string {
	parts := strings.SplitN(string(key), ":", 4)
	if len(parts) >= 2 && parts[0] == "slack" {
		return fmt.Sprintf("https://app.slack.com/docs/%s/%s", url.PathEscape(parts[1]), url.PathEscape(canvasID))
	}
	return canvasID
}
