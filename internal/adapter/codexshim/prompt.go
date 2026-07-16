package codexshim

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
)

// BuildRunArgs produces the deterministic `codex exec` argument vector. User
// text is never included; the prompt is delivered on stdin through the `-`
// sentinel. Global options are placed before `exec` because Codex 0.144.5
// rejects `--ask-for-approval` after the subcommand.
func BuildRunArgs(request cliprotocol.Request) []string {
	var args []string
	allowWrites := false
	if request.Profile != nil {
		if request.Profile.Model != "" {
			args = append(args, "--model", request.Profile.Model)
		}
		if request.Profile.Variant != "" {
			args = append(args, "--config", `model_reasoning_effort="`+request.Profile.Variant+`"`)
		}
	}
	sandbox := "read-only"
	if request.Profile != nil && request.Profile.Approval == cliprotocol.ApprovalAuto {
		sandbox = "workspace-write"
		allowWrites = true
	}
	if request.Workspace != nil && request.Workspace.WorkingDirectory != "" {
		args = append(args, "--cd", request.Workspace.WorkingDirectory)
	}
	args = append(args, "--sandbox", sandbox, "--ask-for-approval", "never")
	// Codex defines --add-dir as an additional writable root. Never emit it for
	// approval=reject; read-only runs still receive the complete registry in the
	// prompt without granting extra write authority.
	if allowWrites && request.Workspace != nil {
		projects := append([]cliprotocol.Project(nil), request.Workspace.Projects...)
		sort.Slice(projects, func(i, j int) bool {
			if projects[i].Name != projects[j].Name {
				return projects[i].Name < projects[j].Name
			}
			return projects[i].Path < projects[j].Path
		})
		seen := make(map[string]struct{}, len(projects))
		for _, project := range projects {
			if project.Path == request.Workspace.WorkingDirectory {
				continue
			}
			if _, duplicate := seen[project.Path]; duplicate {
				continue
			}
			seen[project.Path] = struct{}{}
			args = append(args, "--add-dir", project.Path)
		}
	}
	args = append(args, "exec", "--json", "--ephemeral", "--color", "never", "-")
	return args
}

// BuildPrompt flattens the trusted instructions, workspace registry, and
// bounded transcript into one Codex user input in deterministic order. Codex
// built-in instructions and loaded native configuration retain their native
// authority; this is not a native system message.
func BuildPrompt(request cliprotocol.Request) string {
	var builder strings.Builder

	instruction := strings.TrimSpace(request.SystemInstruction)
	if instruction != "" {
		builder.WriteString("<<AGENT INSTRUCTIONS (trusted)>>\n")
		builder.WriteString(instruction)
		builder.WriteString("\n\n")
	}

	if request.Workspace != nil && len(request.Workspace.Projects) > 0 {
		projects := append([]cliprotocol.Project(nil), request.Workspace.Projects...)
		sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
		registry, _ := json.Marshal(struct {
			WorkingDirectory string                `json:"working_directory"`
			Projects         []cliprotocol.Project `json:"projects"`
		}{
			WorkingDirectory: request.Workspace.WorkingDirectory,
			Projects:         projects,
		})
		builder.WriteString("<<WORKSPACE REGISTRY (trusted)>>\n")
		builder.Write(registry)
		builder.WriteString("\n\n")
	}

	builder.WriteString("<<CONVERSATION TRANSCRIPT>>\n")
	for _, message := range request.Messages {
		label := "user"
		if message.Role == cliprotocol.RoleAssistant {
			label = "assistant"
		}
		builder.WriteString("[")
		builder.WriteString(label)
		builder.WriteString("]\n")
		builder.WriteString(message.Text)
		builder.WriteString("\n\n")
	}
	builder.WriteString("The final transcript item above is the current request. Respond to it.")

	return builder.String()
}
