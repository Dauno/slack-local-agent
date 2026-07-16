// Package codexshim maps the stable cli-v1 protocol to the Codex CLI. It is
// packaged behind the hidden command `local-agent shim codex`. It never loads
// local-agent configuration, opens SQLite, initializes Slack, or bootstraps
// state.
package codexshim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
)

const (
	// ProviderName identifies this mapper in describe responses.
	ProviderName = "codex"
	// ShimVersion identifies the mapper build.
	ShimVersion = "v0.1.1"
	// SupportedCLIVersion is the only Codex CLI version covered by the
	// recorded fixtures of this mapper build. The experimental
	// `codex debug models` schema and the JSONL event subset are pinned to
	// it; accepting another version requires new fixtures and tests.
	SupportedCLIVersion = "0.144.5"
	// DefaultExecutable is the Codex executable resolved through PATH.
	DefaultExecutable = "codex"
)

// versionPrefix is the exact program identifier emitted by `codex --version`.
const versionPrefix = "codex-cli"

// Capabilities reports only what this mapper version implements. The list is
// diagnostic and does not expand the ADK content subset accepted by the
// generic adapter.
func Capabilities() []string {
	return []string{"text", "native_tools", "multi_project", "ephemeral"}
}

// Bounds are the mapper's inner Codex process limits. They are independent
// from the generic cli-v1 output limits enforced by the provider adapter.
type Bounds struct {
	MaxRequestLineBytes int
	MaxRawLineBytes     int
	MaxRawStdoutBytes   int
	MaxRawStderrBytes   int
	MaxCatalogBytes     int
}

func (b Bounds) withDefaults() Bounds {
	if b.MaxRequestLineBytes <= 0 {
		b.MaxRequestLineBytes = 8 << 20
	}
	if b.MaxRawLineBytes <= 0 {
		b.MaxRawLineBytes = 1 << 20
	}
	if b.MaxRawStdoutBytes <= 0 {
		b.MaxRawStdoutBytes = 16 << 20
	}
	if b.MaxRawStderrBytes <= 0 {
		b.MaxRawStderrBytes = 8 << 10
	}
	if b.MaxCatalogBytes <= 0 {
		b.MaxCatalogBytes = 8 << 20
	}
	return b
}

// Config wires the mapper's executor and bounds.
type Config struct {
	// Executor abstracts Codex invocations. Nil selects the real
	// PATH-resolved Codex executable.
	Executor Executor
	Bounds   Bounds
}

// Run reads exactly one cli-v1 request from in, performs it, writes protocol
// responses to out, and returns the process exit code.
func Run(ctx context.Context, in io.Reader, out io.Writer, cfg Config) int {
	bounds := cfg.Bounds.withDefaults()
	executor := cfg.Executor
	if executor == nil {
		executor = NewExecutor(DefaultExecutable, bounds)
	}

	requestLine, _, truncated, err := readBoundedLine(bufio.NewReaderSize(in, bounds.MaxRequestLineBytes), bounds.MaxRequestLineBytes)
	if err != nil {
		writeResponse(out, cliprotocol.NewError("unknown", cliprotocol.CodeInvalidRequest, fmt.Sprintf("read cli-v1 request: %v", err), false))
		return 2
	}
	if truncated {
		writeResponse(out, cliprotocol.NewError("unknown", cliprotocol.CodeInvalidRequest, "cli-v1 request exceeds the request size bound", false))
		return 2
	}
	request, err := cliprotocol.DecodeRequest(requestLine)
	if err != nil {
		writeResponse(out, cliprotocol.NewError(requestID(requestLine), cliprotocol.CodeInvalidRequest, err.Error(), false))
		return 2
	}

	switch request.Method {
	case cliprotocol.MethodDescribe:
		writeResponse(out, describe(ctx, executor, request.ID))
	case cliprotocol.MethodValidate:
		writeResponse(out, validate(ctx, executor, request))
	case cliprotocol.MethodRun:
		runModel(ctx, executor, request, bounds, out)
	default:
		writeResponse(out, cliprotocol.NewError(request.ID, cliprotocol.CodeUnsupported, fmt.Sprintf("unsupported method %q", request.Method), false))
	}
	return 0
}

// requestID best-effort extracts the id from a malformed request for error
// correlation.
func requestID(line []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil || strings.TrimSpace(envelope.ID) == "" {
		return "unknown"
	}
	return envelope.ID
}

func writeResponse(out io.Writer, resp cliprotocol.Response) {
	line, err := cliprotocol.EncodeLine(resp)
	if err != nil {
		return
	}
	_, _ = out.Write(line)
}

// describe checks the Codex executable and its exact supported version. It
// performs no model call.
func describe(ctx context.Context, executor Executor, id string) cliprotocol.Response {
	rawVersion, err := executor.Version(ctx)
	if err != nil {
		return cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("discover Codex executable", err), false)
	}
	version, ok := normalizeVersion(rawVersion)
	if !ok || version != SupportedCLIVersion {
		return cliprotocol.NewError(id, cliprotocol.CodeUnsupported,
			fmt.Sprintf("installed Codex CLI version is not supported by this shim; install exactly %s (upgrade or downgrade Codex)", SupportedCLIVersion), false)
	}
	return cliprotocol.NewDescription(id, ProviderName, ShimVersion, version, Capabilities())
}

// normalizeVersion accepts exactly `codex-cli <version>` with surrounding
// ASCII whitespace ignored. Prerelease labels, suffixes, and any other output
// intentionally fail the comparison.
func normalizeVersion(raw string) (string, bool) {
	trimmed := strings.Trim(raw, " \t\r\n")
	rest, found := strings.CutPrefix(trimmed, versionPrefix+" ")
	if !found {
		return "", false
	}
	version := strings.Trim(rest, " \t")
	if version == "" || strings.ContainsAny(version, " \t\r\n") {
		return "", false
	}
	return version, true
}

// validate verifies Codex-specific profile semantics from the bundled model
// catalog and the Git worktree requirement. It performs no model call and no
// remote catalog refresh.
func validate(ctx context.Context, executor Executor, request cliprotocol.Request) cliprotocol.Response {
	id := request.ID
	profile := request.Profile

	if profile.Agent != "" {
		return cliprotocol.NewError(id, cliprotocol.CodeUnsupported,
			"profile agent is not supported by the Codex mapper; Codex --profile selects a configuration layer, not a primary agent", false)
	}

	models, err := executor.ListModels(ctx)
	if err != nil {
		return cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("list bundled Codex models", err), false)
	}
	var selected *ModelInfo
	for index := range models {
		if models[index].Slug == profile.Model {
			selected = &models[index]
			break
		}
	}
	if selected == nil {
		return cliprotocol.NewError(id, cliprotocol.CodeInvalidRequest,
			fmt.Sprintf("model %q is not available in the bundled Codex model catalog", profile.Model), false)
	}
	if profile.Variant != "" && !containsString(selected.Efforts, profile.Variant) {
		return cliprotocol.NewError(id, cliprotocol.CodeInvalidRequest,
			fmt.Sprintf("reasoning effort %q is not supported by Codex model %q", profile.Variant, profile.Model), false)
	}

	worktree, err := executor.GitWorktree(ctx, request.Workspace.WorkingDirectory)
	if err != nil {
		return cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("check Git worktree", err), false)
	}
	if strings.Trim(worktree, " \t\r\n") != "true" {
		return cliprotocol.NewError(id, cliprotocol.CodeInvalidRequest,
			"the working directory must be inside a Git worktree for a Codex profile", false)
	}

	return cliprotocol.NewValidated(id)
}

// runModel launches one `codex exec --json --ephemeral` invocation,
// normalizes its JSONL events, and writes activity plus exactly one terminal
// message.
func runModel(ctx context.Context, executor Executor, request cliprotocol.Request, bounds Bounds, out io.Writer) {
	id := request.ID
	args := BuildRunArgs(request)
	prompt := BuildPrompt(request)

	process, err := executor.StartRun(ctx, args, prompt)
	if err != nil {
		writeResponse(out, cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("start codex exec", err), false))
		return
	}

	parsed, parseErr := ParseRunEvents(process.Stdout(), bounds, func(name, status string) {
		activity := cliprotocol.NewActivity(id, cliprotocol.ActivityKindTool, name, status)
		if err := cliprotocol.ValidateResponse(activity); err != nil {
			// Native item types are diagnostic only. Normalize unexpected
			// names rather than emitting arbitrary child-controlled fields.
			activity.Name = "unknown"
		}
		writeResponse(out, activity)
	})
	if parseErr != nil {
		// Parsing stopped before EOF. Terminate and drain before Wait so a
		// continued writer cannot deadlock the shim on a full stdout pipe.
		_ = process.Terminate()
		_, _ = io.Copy(io.Discard, process.Stdout())
	}
	waitErr := process.Wait()
	diagnostics := process.Diagnostics()

	switch {
	case ctx.Err() != nil:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeTimeout, "codex exec cancelled"+diagnosticDetail(diagnostics), errors.Is(ctx.Err(), context.DeadlineExceeded)))
	case parseErr != nil:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeProcessFailed,
			fmt.Sprintf("parse codex output: %v%s", parseErr, diagnosticDetail(diagnostics)), false))
	case parsed.Failed:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeProcessFailed,
			"codex reported a failed turn"+diagnosticDetail(diagnostics), false))
	case waitErr != nil:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeProcessFailed,
			"codex "+processFailure(waitErr)+diagnosticDetail(diagnostics), false))
	case !parsed.Completed:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeNoResponse,
			"codex exited without completing a turn"+diagnosticDetail(diagnostics), false))
	case strings.TrimSpace(parsed.Text) == "":
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeNoResponse,
			"codex completed without producing a final response"+diagnosticDetail(diagnostics), false))
	default:
		writeResponse(out, cliprotocol.NewResult(id, strings.TrimSpace(parsed.Text)))
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func classifyExecutorError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cliprotocol.CodeTimeout
	}
	if errors.Is(err, ErrExecutableNotFound) {
		return cliprotocol.CodeExecutableMissing
	}
	return cliprotocol.CodeProcessFailed
}

func executorErrorMessage(action string, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return action + " cancelled"
	}
	if errors.Is(err, ErrExecutableNotFound) {
		return "Codex executable was not found"
	}
	return action + " failed"
}
