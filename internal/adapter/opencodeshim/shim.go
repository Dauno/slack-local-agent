// Package opencodeshim maps the stable cli-v1 protocol to the OpenCode CLI.
// It is the reference mapper packaged behind the hidden command
// `local-agent shim opencode`. It never loads local-agent configuration,
// opens SQLite, initializes Slack, or bootstraps state.
package opencodeshim

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
	ProviderName = "opencode"
	// ShimVersion identifies the mapper build.
	ShimVersion = "v0.1.1"
	// SupportedCLIVersion is the only OpenCode version covered by the recorded
	// fixtures of this mapper build. Expanding the range requires fixtures and
	// acceptance tests for every newly accepted version.
	SupportedCLIVersion = "1.18.3"
	// DefaultExecutable is the OpenCode executable resolved through PATH.
	DefaultExecutable = "opencode"
)

// Capabilities reports only what this mapper version implements.
func Capabilities() []string {
	return []string{"text", "native_tools", "multi_project"}
}

// Bounds are the mapper's inner OpenCode process limits. They are independent
// from the generic cli-v1 output limits enforced by the provider adapter.
type Bounds struct {
	MaxRequestLineBytes int
	MaxRawLineBytes     int
	MaxRawStdoutBytes   int
	MaxRawStderrBytes   int
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
	return b
}

// Config wires the mapper's executor and bounds.
type Config struct {
	// Executor abstracts OpenCode invocations. Nil selects the real
	// PATH-resolved OpenCode executable.
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

// describe checks the OpenCode executable and its exact supported version. It
// performs no model call.
func describe(ctx context.Context, executor Executor, id string) cliprotocol.Response {
	rawVersion, err := executor.Version(ctx)
	if err != nil {
		return cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("discover OpenCode executable", err), false)
	}
	version := normalizeVersion(rawVersion)
	if version != SupportedCLIVersion {
		return cliprotocol.NewError(id, cliprotocol.CodeUnsupported,
			fmt.Sprintf("installed OpenCode version is not supported by this shim; install exactly %s (upgrade or downgrade OpenCode)", SupportedCLIVersion), false)
	}
	return cliprotocol.NewDescription(id, ProviderName, ShimVersion, version, Capabilities())
}

// normalizeVersion trims ASCII whitespace and one optional leading "v".
// Prerelease labels and any other suffix intentionally fail the comparison.
func normalizeVersion(raw string) string {
	version := strings.Trim(raw, " \t\r\n")
	version = strings.TrimPrefix(version, "v")
	return version
}

// validate verifies model syntax, model availability from the local catalog,
// and that a configured agent exists and is primary. It performs no model
// call and prevents `opencode run` from silently falling back.
func validate(ctx context.Context, executor Executor, request cliprotocol.Request) cliprotocol.Response {
	id := request.ID
	profile := request.Profile

	provider, rest, found := strings.Cut(profile.Model, "/")
	if !found || strings.TrimSpace(provider) == "" || strings.TrimSpace(rest) == "" {
		return cliprotocol.NewError(id, cliprotocol.CodeInvalidRequest,
			fmt.Sprintf("model %q must use OpenCode provider/model syntax", profile.Model), false)
	}

	models, err := executor.ListModels(ctx)
	if err != nil {
		return cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("list OpenCode models", err), false)
	}
	if !containsString(models, profile.Model) {
		return cliprotocol.NewError(id, cliprotocol.CodeInvalidRequest,
			fmt.Sprintf("model %q is not available in the local OpenCode model catalog", profile.Model), false)
	}

	if profile.Agent != "" {
		agents, err := executor.ListAgents(ctx)
		if err != nil {
			return cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("list OpenCode agents", err), false)
		}
		var match *AgentInfo
		for index := range agents {
			if agents[index].Name == profile.Agent {
				match = &agents[index]
				break
			}
		}
		if match == nil {
			return cliprotocol.NewError(id, cliprotocol.CodeInvalidRequest,
				fmt.Sprintf("OpenCode agent %q does not exist", profile.Agent), false)
		}
		if !match.Primary {
			return cliprotocol.NewError(id, cliprotocol.CodeInvalidRequest,
				fmt.Sprintf("OpenCode agent %q is not a primary agent", profile.Agent), false)
		}
	}

	return cliprotocol.NewValidated(id)
}

// runModel launches one `opencode run --format json` invocation, normalizes
// its events, and writes activity plus exactly one terminal message.
func runModel(ctx context.Context, executor Executor, request cliprotocol.Request, bounds Bounds, out io.Writer) {
	id := request.ID
	args := BuildRunArgs(request)
	prompt := BuildPrompt(request)

	process, err := executor.StartRun(ctx, args, prompt)
	if err != nil {
		writeResponse(out, cliprotocol.NewError(id, classifyExecutorError(err), executorErrorMessage("start opencode run", err), false))
		return
	}

	parsed, parseErr := ParseRunEvents(process.Stdout(), bounds, func(name, status string) {
		activity := cliprotocol.NewActivity(id, cliprotocol.ActivityKindTool, name, status)
		if err := cliprotocol.ValidateResponse(activity); err != nil {
			// Native tool names are diagnostic only. Normalize unexpected names
			// rather than emitting arbitrary child-controlled protocol fields.
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
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeTimeout, "opencode run cancelled"+diagnosticDetail(diagnostics), errors.Is(ctx.Err(), context.DeadlineExceeded)))
	case parseErr != nil:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeProcessFailed,
			fmt.Sprintf("parse opencode output: %v%s", parseErr, diagnosticDetail(diagnostics)), false))
	case parsed.SessionFailed:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeProcessFailed,
			"opencode reported a session error"+diagnosticDetail(diagnostics), false))
	case waitErr != nil:
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeProcessFailed,
			"opencode "+processFailure(waitErr)+diagnosticDetail(diagnostics), false))
	case strings.TrimSpace(parsed.Text) == "":
		writeResponse(out, cliprotocol.NewError(id, cliprotocol.CodeNoResponse,
			"opencode exited before producing a final response"+diagnosticDetail(diagnostics), false))
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
		return "OpenCode executable was not found"
	}
	return action + " failed"
}
