package codexshim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/codexshim"
	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
)

// --- fake executor ---

type fakeProcess struct {
	stdout      io.Reader
	diagnostics codexshim.ProcessDiagnostics
	err         error
	terminate   func() error
}

func (p *fakeProcess) Stdout() io.Reader { return p.stdout }
func (p *fakeProcess) Terminate() error {
	if p.terminate != nil {
		return p.terminate()
	}
	return nil
}
func (p *fakeProcess) Wait() error { return p.err }
func (p *fakeProcess) Diagnostics() codexshim.ProcessDiagnostics {
	return p.diagnostics
}

type fakeExecutor struct {
	version     string
	versionErr  error
	models      []codexshim.ModelInfo
	modelsErr   error
	worktree    string
	worktreeErr error
	runStdout   string
	runStderr   string
	runErr      error
	startErr    error
	runProcess  codexshim.Process

	gotArgs        []string
	gotPrompt      string
	gotWorktreeDir string
	modelCalls     int
}

func (e *fakeExecutor) Version(context.Context) (string, error) {
	return e.version, e.versionErr
}

func (e *fakeExecutor) ListModels(context.Context) ([]codexshim.ModelInfo, error) {
	e.modelCalls++
	return e.models, e.modelsErr
}

func (e *fakeExecutor) GitWorktree(_ context.Context, dir string) (string, error) {
	e.gotWorktreeDir = dir
	if e.worktreeErr != nil {
		return "", e.worktreeErr
	}
	if e.worktree == "" {
		return "true\n", nil
	}
	return e.worktree, nil
}

func (e *fakeExecutor) StartRun(_ context.Context, args []string, prompt string) (codexshim.Process, error) {
	e.gotArgs = args
	e.gotPrompt = prompt
	if e.startErr != nil {
		return nil, e.startErr
	}
	if e.runProcess != nil {
		return e.runProcess, nil
	}
	return &fakeProcess{
		stdout: strings.NewReader(e.runStdout),
		diagnostics: codexshim.ProcessDiagnostics{
			StderrBytes: int64(len(e.runStderr)),
		},
		err: e.runErr,
	}, nil
}

func runShim(t *testing.T, exec codexshim.Executor, request cliprotocol.Request) []cliprotocol.Response {
	t.Helper()
	return runShimWithBounds(t, exec, request, codexshim.Bounds{})
}

func runShimWithBounds(t *testing.T, exec codexshim.Executor, request cliprotocol.Request, bounds codexshim.Bounds) []cliprotocol.Response {
	t.Helper()
	line, err := cliprotocol.EncodeLine(request)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	var out bytes.Buffer
	codexshim.Run(context.Background(), bytes.NewReader(line), &out, codexshim.Config{Executor: exec, Bounds: bounds})
	return decodeResponses(t, out.Bytes())
}

func decodeResponses(t *testing.T, data []byte) []cliprotocol.Response {
	t.Helper()
	var responses []cliprotocol.Response
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var resp cliprotocol.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		responses = append(responses, resp)
	}
	return responses
}

func terminalOf(responses []cliprotocol.Response) cliprotocol.Response {
	for _, resp := range responses {
		if cliprotocol.IsTerminal(resp.Type) {
			return resp
		}
	}
	return cliprotocol.Response{}
}

func lunaModels() []codexshim.ModelInfo {
	return []codexshim.ModelInfo{
		{Slug: "gpt-5.6-luna", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
		{Slug: "gpt-5.5", Efforts: []string{"low", "medium", "high", "xhigh"}},
	}
}

// --- describe ---

func TestDescribeSupportedVersion(t *testing.T) {
	exec := &fakeExecutor{version: "codex-cli 0.144.5"}
	terminal := terminalOf(runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe)))
	if terminal.Type != cliprotocol.TypeDescription {
		t.Fatalf("expected description, got %+v", terminal)
	}
	if terminal.CLIVersion != "0.144.5" || terminal.ShimVersion != codexshim.ShimVersion {
		t.Fatalf("unexpected versions: %+v", terminal)
	}
	if terminal.Name != "codex" {
		t.Fatalf("name = %q", terminal.Name)
	}
}

func TestDescribeToleratesSurroundingWhitespace(t *testing.T) {
	exec := &fakeExecutor{version: "  codex-cli 0.144.5 \t\r\n"}
	terminal := terminalOf(runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe)))
	if terminal.Type != cliprotocol.TypeDescription {
		t.Fatalf("surrounding whitespace should be accepted, got %+v", terminal)
	}
}

func TestDescribeRejectsWrongVersions(t *testing.T) {
	for _, version := range []string{
		"codex-cli 0.144.1",
		"codex-cli 0.144.4",
		"codex-cli 0.144.6",
		"codex-cli 0.145.0",
		"codex-cli 0.144.5-beta",
		"codex-cli 0.144.5 nightly",
		"codex 0.144.5",
		"0.144.5",
		"",
	} {
		exec := &fakeExecutor{version: version}
		terminal := terminalOf(runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe)))
		if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeUnsupported {
			t.Fatalf("version %q should be unsupported, got %+v", version, terminal)
		}
	}
}

func TestDescribeExecutableMissing(t *testing.T) {
	exec := &fakeExecutor{versionErr: codexshim.ErrExecutableNotFound}
	terminal := terminalOf(runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe)))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeExecutableMissing {
		t.Fatalf("expected executable_not_found, got %+v", terminal)
	}
}

func TestDescribeMakesNoModelCall(t *testing.T) {
	exec := &fakeExecutor{version: "codex-cli 0.144.5"}
	runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe))
	if exec.gotArgs != nil || exec.modelCalls != 0 {
		t.Fatalf("describe must not start a run or list models: args=%v models=%d", exec.gotArgs, exec.modelCalls)
	}
}

// --- validate ---

func validateRequest(model, agent, variant string) cliprotocol.Request {
	req := cliprotocol.NewRequest("v-1", cliprotocol.MethodValidate)
	req.Profile = &cliprotocol.Profile{Model: model, Agent: agent, Variant: variant}
	req.Workspace = &cliprotocol.Workspace{
		WorkingDirectory: "/tmp/ws",
		Projects:         []cliprotocol.Project{{Name: "workspace", Path: "/tmp/ws"}},
	}
	return req
}

func TestValidateSuccess(t *testing.T) {
	exec := &fakeExecutor{models: lunaModels()}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-5.6-luna", "", "high")))
	if terminal.Type != cliprotocol.TypeValidated {
		t.Fatalf("expected validated, got %+v", terminal)
	}
	if exec.gotWorktreeDir != "/tmp/ws" {
		t.Fatalf("git worktree checked in %q", exec.gotWorktreeDir)
	}
}

func TestValidateAcceptsOmittedVariant(t *testing.T) {
	exec := &fakeExecutor{models: lunaModels()}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-5.6-luna", "", "")))
	if terminal.Type != cliprotocol.TypeValidated {
		t.Fatalf("expected validated, got %+v", terminal)
	}
}

func TestValidateRejectsUnknownModel(t *testing.T) {
	exec := &fakeExecutor{models: lunaModels()}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-9-missing", "", "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeInvalidRequest {
		t.Fatalf("expected invalid_request for unknown model, got %+v", terminal)
	}
}

func TestValidateRejectsUnsupportedEffort(t *testing.T) {
	exec := &fakeExecutor{models: lunaModels()}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-5.6-luna", "", "ultra")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeInvalidRequest {
		t.Fatalf("expected invalid_request for unsupported effort, got %+v", terminal)
	}
}

func TestValidateRejectsNonEmptyAgent(t *testing.T) {
	exec := &fakeExecutor{models: lunaModels()}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-5.6-luna", "build", "high")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeUnsupported {
		t.Fatalf("expected unsupported for non-empty agent, got %+v", terminal)
	}
	if exec.modelCalls != 0 {
		t.Fatalf("agent rejection must happen before catalog access")
	}
}

func TestValidateRejectsNonWorktree(t *testing.T) {
	exec := &fakeExecutor{models: lunaModels(), worktree: "false\n"}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-5.6-luna", "", "high")))
	if terminal.Type != cliprotocol.TypeError || !strings.Contains(terminal.Message, "Git worktree") {
		t.Fatalf("expected Git worktree rejection, got %+v", terminal)
	}
}

func TestValidateRejectsGitFailure(t *testing.T) {
	exec := &fakeExecutor{models: lunaModels(), worktreeErr: errors.New("git -C /tmp/ws rev-parse --is-inside-work-tree exited with code 128")}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-5.6-luna", "", "high")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("expected process_failed for git failure, got %+v", terminal)
	}
}

func TestValidateRejectsCatalogFailure(t *testing.T) {
	exec := &fakeExecutor{modelsErr: errors.New("codex debug models --bundled exited with code 2")}
	terminal := terminalOf(runShim(t, exec, validateRequest("gpt-5.6-luna", "", "high")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("expected process_failed for catalog failure, got %+v", terminal)
	}
}

// --- catalog parsing ---

func TestParseModelCatalogFixture(t *testing.T) {
	models, err := codexshim.ParseModelCatalog([]byte(readFixture(t, "models_bundled.json")))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(models) != 2 || models[0].Slug != "gpt-5.6-luna" {
		t.Fatalf("unexpected models: %+v", models)
	}
	if strings.Join(models[0].Efforts, ",") != "low,medium,high,xhigh,max" {
		t.Fatalf("unexpected efforts: %v", models[0].Efforts)
	}
}

func TestParseModelCatalogRejectsMalformed(t *testing.T) {
	for name, payload := range map[string]string{
		"not JSON":     "plain text",
		"empty object": `{}`,
		"empty models": `{"models":[]}`,
		"missing slug": `{"models":[{"supported_reasoning_levels":[{"effort":"high"}]}]}`,
	} {
		if _, err := codexshim.ParseModelCatalog([]byte(payload)); err == nil {
			t.Fatalf("%s catalog should be rejected", name)
		}
	}
}

func TestParseModelCatalogErrorNeverEchoesCatalogContent(t *testing.T) {
	payload := `{"models":[{"supported_reasoning_levels":[]}], "instructions":"SECRET-INSTRUCTIONS"}`
	_, err := codexshim.ParseModelCatalog([]byte(payload))
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "SECRET-INSTRUCTIONS") {
		t.Fatalf("catalog content leaked into error: %v", err)
	}
}

// --- run ---

func runRequest(approval, variant string) cliprotocol.Request {
	req := cliprotocol.NewRequest("run-1", cliprotocol.MethodRun)
	req.Profile = &cliprotocol.Profile{Model: "gpt-5.6-luna", Approval: approval, Variant: variant}
	req.SystemInstruction = "You are Dev Agent."
	req.Messages = []cliprotocol.Message{
		{Role: cliprotocol.RoleUser, Text: "Summarize the repo."},
	}
	req.Workspace = &cliprotocol.Workspace{
		WorkingDirectory: "/tmp/ws",
		Projects: []cliprotocol.Project{
			{Name: "workspace-main", Path: "/tmp/ws"},
			{Name: "api-service", Path: "/tmp/api"},
			{Name: "zeta-lib", Path: "/tmp/zeta"},
		},
	}
	return req
}

func TestRunTextFixtureProducesResult(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "high")))
	if terminal.Type != cliprotocol.TypeResult {
		t.Fatalf("expected result, got %+v", terminal)
	}
	if terminal.Text != "OK" {
		t.Fatalf("text = %q", terminal.Text)
	}
	if terminal.FinishReason != cliprotocol.FinishReasonStop {
		t.Fatalf("finish reason = %q", terminal.FinishReason)
	}
}

func TestRunToolFixtureSelectsLastAgentMessage(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "tool_run.jsonl")}
	responses := runShim(t, exec, runRequest(cliprotocol.ApprovalAuto, ""))
	terminal := terminalOf(responses)
	if terminal.Type != cliprotocol.TypeResult || terminal.Text != "The file contains hello world." {
		t.Fatalf("unexpected terminal: %+v", terminal)
	}
	if strings.Contains(terminal.Text, "SECRET-REASONING") || strings.Contains(terminal.Text, "SECRET-TOOL-OUTPUT") {
		t.Fatalf("reasoning/tool payload leaked into final text: %q", terminal.Text)
	}
	var sawCommand, sawFileChange bool
	for _, resp := range responses {
		if resp.Type != cliprotocol.TypeActivity {
			continue
		}
		if strings.Contains(resp.Name, "SECRET") {
			t.Fatalf("activity leaked payload: %+v", resp)
		}
		if resp.Name == "command_execution" {
			sawCommand = true
		}
		if resp.Name == "file_change" {
			sawFileChange = true
		}
	}
	if !sawCommand || !sawFileChange {
		t.Fatalf("expected payload-free activities, got %+v", responses)
	}
}

func TestRunFailedTurnWinsOverText(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "failed_run.jsonl"), runErr: errors.New("exit status 1")}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("expected process_failed, got %+v", terminal)
	}
	if strings.Contains(terminal.Message, "Unexpected server error") {
		t.Fatalf("native error content leaked: %q", terminal.Message)
	}
}

func TestRunErrorEventFails(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "error_run.jsonl"), runErr: errors.New("exit status 1")}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("expected process_failed, got %+v", terminal)
	}
	if strings.Contains(terminal.Message, "stream disconnected") {
		t.Fatalf("native error content leaked: %q", terminal.Message)
	}
}

func TestRunErrorMayPrecedeTerminalTurnFailed(t *testing.T) {
	stdout := `{"type":"error","message":"stream disconnected"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"stream disconnected"}}` + "\n"
	exec := &fakeExecutor{runStdout: stdout, runErr: errors.New("exit status 1")}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("expected process_failed, got %+v", terminal)
	}
	if !strings.Contains(terminal.Message, "reported a failed turn") {
		t.Fatalf("valid Codex failure sequence was misclassified: %+v", terminal)
	}
}

func TestRunMissingTurnCompletionFails(t *testing.T) {
	stdout := `{"type":"thread.started","thread_id":"thr_1"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"partial"}}` + "\n"
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeNoResponse {
		t.Fatalf("missing turn.completed should fail, got %+v", terminal)
	}
}

func TestRunEmptyFinalMessageFails(t *testing.T) {
	stdout := `{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"  "}}` + "\n" +
		`{"type":"turn.completed"}` + "\n"
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeNoResponse {
		t.Fatalf("empty final message should fail, got %+v", terminal)
	}
}

func TestRunTrailingEventAfterTerminalTurnFails(t *testing.T) {
	stdout := `{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"OK"}}` + "\n" +
		`{"type":"turn.completed"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"trailing"}}` + "\n"
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("trailing event should fail, got %+v", terminal)
	}
}

func TestRunNonJSONLineFails(t *testing.T) {
	stdout := "plain diagnostic line\n" + readFixture(t, "text_run.jsonl")
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("non-JSON stdout should fail, got %+v", terminal)
	}
}

func TestRunMalformedItemCompletedFails(t *testing.T) {
	stdout := `{"type":"item.completed"}` + "\n" + `{"type":"turn.completed"}` + "\n"
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("malformed item.completed should fail, got %+v", terminal)
	}
}

func TestRunUnknownEventsIgnored(t *testing.T) {
	stdout := `{"type":"future.event","payload":{"huge":"but bounded"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"future_item"}}` + "\n" +
		readFixture(t, "text_run.jsonl")
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeResult || terminal.Text != "OK" {
		t.Fatalf("unknown events should be ignored, got %+v", terminal)
	}
}

func TestRunNonZeroExitWinsOverText(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl"), runErr: errors.New("exit status 2")}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("non-zero exit should override text, got %+v", terminal)
	}
}

func TestRunNativeStderrContentIsNotExposed(t *testing.T) {
	exec := &fakeExecutor{
		runStdout: readFixture(t, "error_run.jsonl"),
		runStderr: "SECRET-FILE-CONTENT",
		runErr:    errors.New("exit status 1"),
	}
	terminal := terminalOf(runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "")))
	if strings.Contains(terminal.Message, "SECRET-FILE-CONTENT") {
		t.Fatalf("native stderr leaked: %q", terminal.Message)
	}
	if !strings.Contains(terminal.Message, "omitted") {
		t.Fatalf("safe stderr metadata missing: %q", terminal.Message)
	}
}

// --- args and prompt ---

func TestRunArgsRejectMapping(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "high"))

	want := []string{
		"--model", "gpt-5.6-luna",
		"--config", `model_reasoning_effort="high"`,
		"--cd", "/tmp/ws",
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
		"exec", "--json", "--ephemeral", "--color", "never", "-",
	}
	if strings.Join(exec.gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", exec.gotArgs, want)
	}
}

func TestRunArgsAutoMapping(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(cliprotocol.ApprovalAuto, ""))
	joined := strings.Join(exec.gotArgs, " ")
	if !strings.Contains(joined, "--sandbox workspace-write") {
		t.Fatalf("auto must map to workspace-write: %v", exec.gotArgs)
	}
	if !strings.Contains(joined, "--ask-for-approval never") {
		t.Fatalf("approval policy must stay never: %v", exec.gotArgs)
	}
	if strings.Contains(joined, "--config") {
		t.Fatalf("omitted variant must not emit a reasoning override: %v", exec.gotArgs)
	}
	if !strings.Contains(joined, "--add-dir /tmp/api") || !strings.Contains(joined, "--add-dir /tmp/zeta") {
		t.Fatalf("auto must expose additional registered writable roots: %v", exec.gotArgs)
	}
}

func TestRunArgsRejectNeverAddsWritableRoots(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(cliprotocol.ApprovalReject, ""))
	if strings.Contains(strings.Join(exec.gotArgs, " "), "--add-dir") {
		t.Fatalf("reject must not grant additional writable roots: %v", exec.gotArgs)
	}
	if !strings.Contains(exec.gotPrompt, `"path":"/tmp/api"`) || !strings.Contains(exec.gotPrompt, `"path":"/tmp/zeta"`) {
		t.Fatalf("reject must retain the complete read-only project registry in the prompt: %q", exec.gotPrompt)
	}
}

func TestRunGlobalFlagsPrecedeExec(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(cliprotocol.ApprovalReject, "high"))
	execIndex := -1
	for index, arg := range exec.gotArgs {
		if arg == "exec" {
			execIndex = index
			break
		}
	}
	if execIndex == -1 {
		t.Fatalf("exec subcommand missing: %v", exec.gotArgs)
	}
	for index, arg := range exec.gotArgs {
		switch arg {
		case "--ask-for-approval", "--sandbox", "--cd", "--model", "--config", "--add-dir":
			if index > execIndex {
				t.Fatalf("global flag %s appears after exec: %v", arg, exec.gotArgs)
			}
		case "--json", "--ephemeral", "--color", "-":
			if index < execIndex {
				t.Fatalf("exec flag %s appears before exec: %v", arg, exec.gotArgs)
			}
		}
	}
	if exec.gotArgs[len(exec.gotArgs)-1] != "-" {
		t.Fatalf("stdin sentinel must be the final argument: %v", exec.gotArgs)
	}
}

func TestNeverPassesForbiddenFlags(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(cliprotocol.ApprovalAuto, "high"))
	joined := strings.Join(exec.gotArgs, " ")
	for _, forbidden := range []string{
		"--dangerously-bypass-approvals-and-sandbox", "--yolo", "danger-full-access",
		"--full-auto", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
		"--strict-config", "--output-last-message", "--output-schema", "--profile",
		"resume", "--last", "--oss", "--image",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("must not pass %s: %v", forbidden, exec.gotArgs)
		}
	}
}

func TestUserTextNeverInArgs(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(cliprotocol.ApprovalReject, ""))
	joined := strings.Join(exec.gotArgs, " ")
	for _, text := range []string{"Summarize the repo.", "You are Dev Agent.", "workspace-main", "api-service"} {
		if strings.Contains(joined, text) {
			t.Fatalf("%q leaked into args: %v", text, exec.gotArgs)
		}
	}
}

func TestRunPromptIsDeterministic(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	req := runRequest(cliprotocol.ApprovalReject, "high")
	runShim(t, exec, req)
	first := exec.gotPrompt
	runShim(t, exec, req)
	if exec.gotPrompt != first {
		t.Fatalf("prompt is not deterministic")
	}
	if !strings.Contains(first, "You are Dev Agent.") {
		t.Fatalf("prompt missing instructions: %q", first)
	}
	if !strings.Contains(first, `"name":"api-service"`) || !strings.Contains(first, `"name":"workspace-main"`) {
		t.Fatalf("prompt missing workspace registry: %q", first)
	}
	if strings.Index(first, `"name":"api-service"`) > strings.Index(first, `"name":"workspace-main"`) {
		t.Fatalf("workspace registry not sorted deterministically: %q", first)
	}
	if !strings.Contains(first, "Summarize the repo.") {
		t.Fatalf("prompt missing transcript: %q", first)
	}
	if !strings.Contains(first, "final transcript item above is the current request") {
		t.Fatalf("prompt missing final-task statement: %q", first)
	}
}

// --- oversized handling ---

func TestOversizedEventRejected(t *testing.T) {
	big := `{"type":"item.completed","item":{"id":"i","type":"command_execution","aggregated_output":"` + strings.Repeat("Z", 5000) + `"}}`
	stdout := big + "\n" + readFixture(t, "text_run.jsonl")
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShimWithBounds(t, exec, runRequest(cliprotocol.ApprovalReject, ""), codexshim.Bounds{MaxRawLineBytes: 1024}))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("oversized event should fail, got %+v", terminal)
	}
}

func TestDiscardedOversizedBytesCountTowardAggregateBound(t *testing.T) {
	big := `{"type":"item.completed","item":{"id":"i","type":"command_execution","aggregated_output":"` + strings.Repeat("Z", 5000) + `"}}`
	exec := &fakeExecutor{runStdout: big + "\n"}
	terminal := terminalOf(runShimWithBounds(t, exec, runRequest(cliprotocol.ApprovalReject, ""), codexshim.Bounds{
		MaxRawLineBytes:   1024,
		MaxRawStdoutBytes: 2048,
	}))
	if terminal.Type != cliprotocol.TypeError || !strings.Contains(terminal.Message, "stdout exceeded 2048 bytes") {
		t.Fatalf("discarded bytes bypassed aggregate bound: %+v", terminal)
	}
}

func TestParserFailureTerminatesContinuedWriter(t *testing.T) {
	reader, writer := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = io.WriteString(writer, `{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"`+strings.Repeat("Z", 5000)+`"}}`+"\n")
		for {
			if _, err := io.WriteString(writer, strings.Repeat("x", 64<<10)); err != nil {
				return
			}
		}
	}()
	var terminateOnce sync.Once
	process := &fakeProcess{
		stdout: reader,
		terminate: func() error {
			terminateOnce.Do(func() { _ = writer.Close() })
			return nil
		},
	}
	exec := &fakeExecutor{runProcess: process}
	done := make(chan []cliprotocol.Response, 1)
	go func() {
		done <- runShimWithBounds(t, exec, runRequest(cliprotocol.ApprovalReject, ""), codexshim.Bounds{MaxRawLineBytes: 1024})
	}()
	select {
	case responses := <-done:
		terminal := terminalOf(responses)
		if terminal.Type != cliprotocol.TypeError {
			t.Fatalf("expected parser failure, got %+v", terminal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parser failure deadlocked while Codex continued writing")
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("continued writer was not terminated")
	}
}

// --- helpers ---

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}
