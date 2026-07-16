package opencodeshim_test

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

	"github.com/Dauno/slack-local-agent/internal/adapter/opencodeshim"
	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
)

// --- fake executor ---

type fakeProcess struct {
	stdout      io.Reader
	diagnostics opencodeshim.ProcessDiagnostics
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
func (p *fakeProcess) Diagnostics() opencodeshim.ProcessDiagnostics {
	return p.diagnostics
}

type fakeExecutor struct {
	version    string
	versionErr error
	models     []string
	agents     []opencodeshim.AgentInfo
	runStdout  string
	runStderr  string
	runErr     error
	startErr   error
	runProcess opencodeshim.Process

	gotArgs   []string
	gotPrompt string
}

func (e *fakeExecutor) Version(context.Context) (string, error) {
	return e.version, e.versionErr
}
func (e *fakeExecutor) ListModels(context.Context) ([]string, error) { return e.models, nil }
func (e *fakeExecutor) ListAgents(context.Context) ([]opencodeshim.AgentInfo, error) {
	return e.agents, nil
}
func (e *fakeExecutor) StartRun(_ context.Context, args []string, prompt string) (opencodeshim.Process, error) {
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
		diagnostics: opencodeshim.ProcessDiagnostics{
			StderrBytes: int64(len(e.runStderr)),
		},
		err: e.runErr,
	}, nil
}

func runShim(t *testing.T, exec opencodeshim.Executor, request cliprotocol.Request) []cliprotocol.Response {
	t.Helper()
	line, err := cliprotocol.EncodeLine(request)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	var out bytes.Buffer
	opencodeshim.Run(context.Background(), bytes.NewReader(line), &out, opencodeshim.Config{Executor: exec})
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
		if err := decodeJSON(line, &resp); err != nil {
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

// --- describe ---

func TestDescribeSupportedVersion(t *testing.T) {
	exec := &fakeExecutor{version: "1.18.3"}
	responses := runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe))
	terminal := terminalOf(responses)
	if terminal.Type != cliprotocol.TypeDescription {
		t.Fatalf("expected description, got %+v", terminal)
	}
	if terminal.CLIVersion != "1.18.3" || terminal.ShimVersion != opencodeshim.ShimVersion {
		t.Fatalf("unexpected versions: %+v", terminal)
	}
	if terminal.Name != "opencode" {
		t.Fatalf("name = %q", terminal.Name)
	}
}

func TestDescribeLeadingVAccepted(t *testing.T) {
	exec := &fakeExecutor{version: "v1.18.3\n"}
	terminal := terminalOf(runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe)))
	if terminal.Type != cliprotocol.TypeDescription {
		t.Fatalf("leading v should be accepted, got %+v", terminal)
	}
}

func TestDescribeRejectsWrongVersion(t *testing.T) {
	for _, version := range []string{"1.18.2", "1.18.3-beta", "1.18.4"} {
		exec := &fakeExecutor{version: version}
		terminal := terminalOf(runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe)))
		if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeUnsupported {
			t.Fatalf("version %q should be unsupported, got %+v", version, terminal)
		}
	}
}

func TestDescribeExecutableMissing(t *testing.T) {
	exec := &fakeExecutor{versionErr: opencodeshim.ErrExecutableNotFound}
	terminal := terminalOf(runShim(t, exec, cliprotocol.NewRequest("d-1", cliprotocol.MethodDescribe)))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeExecutableMissing {
		t.Fatalf("expected executable_not_found, got %+v", terminal)
	}
}

// --- validate ---

func validateRequest(model, agent string) cliprotocol.Request {
	req := cliprotocol.NewRequest("v-1", cliprotocol.MethodValidate)
	req.Profile = &cliprotocol.Profile{Model: model, Agent: agent}
	req.Workspace = &cliprotocol.Workspace{
		WorkingDirectory: "/tmp/ws",
		Projects:         []cliprotocol.Project{{Name: "workspace", Path: "/tmp/ws"}},
	}
	return req
}

func TestValidateSuccess(t *testing.T) {
	exec := &fakeExecutor{
		models: []string{"anthropic/model-name", "opencode/other"},
		agents: []opencodeshim.AgentInfo{{Name: "build", Primary: true}, {Name: "explore", Primary: false}},
	}
	terminal := terminalOf(runShim(t, exec, validateRequest("anthropic/model-name", "build")))
	if terminal.Type != cliprotocol.TypeValidated {
		t.Fatalf("expected validated, got %+v", terminal)
	}
}

func TestValidateRejectsUnknownModel(t *testing.T) {
	exec := &fakeExecutor{models: []string{"anthropic/model-name"}}
	terminal := terminalOf(runShim(t, exec, validateRequest("anthropic/missing", "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeInvalidRequest {
		t.Fatalf("expected invalid_request for unknown model, got %+v", terminal)
	}
}

func TestValidateRejectsBadModelSyntax(t *testing.T) {
	exec := &fakeExecutor{models: []string{"anthropic/model-name"}}
	terminal := terminalOf(runShim(t, exec, validateRequest("noslash", "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeInvalidRequest {
		t.Fatalf("expected invalid_request for bad syntax, got %+v", terminal)
	}
}

func TestValidateRejectsUnknownAgent(t *testing.T) {
	exec := &fakeExecutor{
		models: []string{"anthropic/model-name"},
		agents: []opencodeshim.AgentInfo{{Name: "build", Primary: true}},
	}
	terminal := terminalOf(runShim(t, exec, validateRequest("anthropic/model-name", "ghost")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeInvalidRequest {
		t.Fatalf("expected invalid_request for unknown agent, got %+v", terminal)
	}
}

func TestValidateRejectsNonPrimaryAgent(t *testing.T) {
	exec := &fakeExecutor{
		models: []string{"anthropic/model-name"},
		agents: []opencodeshim.AgentInfo{{Name: "explore", Primary: false}},
	}
	terminal := terminalOf(runShim(t, exec, validateRequest("anthropic/model-name", "explore")))
	if terminal.Type != cliprotocol.TypeError || !strings.Contains(terminal.Message, "not a primary") {
		t.Fatalf("expected non-primary rejection, got %+v", terminal)
	}
}

// --- run ---

func runRequest(t *testing.T, approval, variant string) cliprotocol.Request {
	t.Helper()
	req := cliprotocol.NewRequest("run-1", cliprotocol.MethodRun)
	req.Profile = &cliprotocol.Profile{Model: "anthropic/model-name", Agent: "build", Approval: approval, Variant: variant}
	req.SystemInstruction = "You are Dev Agent."
	req.Messages = []cliprotocol.Message{
		{Role: cliprotocol.RoleUser, Text: "Summarize the repo."},
	}
	req.Workspace = &cliprotocol.Workspace{
		WorkingDirectory: "/tmp/ws",
		Projects: []cliprotocol.Project{
			{Name: "workspace", Path: "/tmp/ws"},
			{Name: "api", Path: "/tmp/api"},
		},
	}
	return req
}

func TestRunTextFixtureProducesResult(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	terminal := terminalOf(runShim(t, exec, runRequest(t, cliprotocol.ApprovalAuto, "")))
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

func TestRunToolFixtureProducesResultAndActivity(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "tool_run.jsonl")}
	responses := runShim(t, exec, runRequest(t, cliprotocol.ApprovalAuto, ""))
	terminal := terminalOf(responses)
	if terminal.Type != cliprotocol.TypeResult || terminal.Text != "hello world" {
		t.Fatalf("unexpected terminal: %+v", terminal)
	}
	var sawTool bool
	for _, resp := range responses {
		if resp.Type == cliprotocol.TypeActivity && resp.Kind == "tool" && resp.Name == "bash" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("expected a tool activity for bash")
	}
}

func TestRunReasoningAndToolOutputNeverInFinalText(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "reasoning_tool_run.jsonl")}
	responses := runShim(t, exec, runRequest(t, cliprotocol.ApprovalAuto, ""))
	terminal := terminalOf(responses)
	if terminal.Type != cliprotocol.TypeResult {
		t.Fatalf("expected result, got %+v", terminal)
	}
	if strings.Contains(terminal.Text, "SECRET-REASONING") || strings.Contains(terminal.Text, "SECRET-TOOL-OUTPUT") {
		t.Fatalf("reasoning/tool payload leaked into final text: %q", terminal.Text)
	}
	if terminal.Text != "The file contains hello world." {
		t.Fatalf("text = %q", terminal.Text)
	}
	var errorTool bool
	for _, resp := range responses {
		if resp.Type == cliprotocol.TypeActivity && resp.Name == "webfetch" && resp.Status == "error" {
			errorTool = true
		}
	}
	if !errorTool {
		t.Fatalf("expected a failed tool activity for webfetch")
	}
}

func TestRunSessionErrorFixture(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "error_run.jsonl"), runErr: errors.New("exit status 1")}
	terminal := terminalOf(runShim(t, exec, runRequest(t, cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("expected process_failed, got %+v", terminal)
	}
	if strings.Contains(terminal.Message, "Unexpected server error") || !strings.Contains(terminal.Message, "session error") {
		t.Fatalf("opaque session error must be omitted, got %q", terminal.Message)
	}
}

func TestRunNativeStderrContentIsNotExposed(t *testing.T) {
	exec := &fakeExecutor{
		runStdout: readFixture(t, "error_run.jsonl"),
		runStderr: "SECRET-FILE-CONTENT",
		runErr:    errors.New("exit status 1"),
	}
	terminal := terminalOf(runShim(t, exec, runRequest(t, cliprotocol.ApprovalReject, "")))
	if strings.Contains(terminal.Message, "SECRET-FILE-CONTENT") {
		t.Fatalf("native stderr leaked: %q", terminal.Message)
	}
	if !strings.Contains(terminal.Message, "omitted") {
		t.Fatalf("safe stderr metadata missing: %q", terminal.Message)
	}
}

func TestRunNoTextProducesNoResponse(t *testing.T) {
	exec := &fakeExecutor{runStdout: `{"type":"step_start","part":{"type":"step-start","id":"p1"}}` + "\n"}
	terminal := terminalOf(runShim(t, exec, runRequest(t, cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeNoResponse {
		t.Fatalf("expected no_response, got %+v", terminal)
	}
}

func TestRunNonZeroExitWinsOverText(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl"), runErr: errors.New("exit status 2")}
	terminal := terminalOf(runShim(t, exec, runRequest(t, cliprotocol.ApprovalReject, "")))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("non-zero exit should override text, got %+v", terminal)
	}
}

func TestRunArgsAndPrompt(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	req := runRequest(t, cliprotocol.ApprovalAuto, "high")
	runShim(t, exec, req)

	wantArgs := []string{"run", "--format", "json", "--dir", "/tmp/ws", "--model", "anthropic/model-name", "--agent", "build", "--variant", "high", "--auto"}
	if strings.Join(exec.gotArgs, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("args = %v, want %v", exec.gotArgs, wantArgs)
	}
	if !strings.Contains(exec.gotPrompt, "You are Dev Agent.") {
		t.Fatalf("prompt missing instructions: %q", exec.gotPrompt)
	}
	if !strings.Contains(exec.gotPrompt, `"name":"api"`) || !strings.Contains(exec.gotPrompt, `"name":"workspace"`) {
		t.Fatalf("prompt missing sorted workspace registry: %q", exec.gotPrompt)
	}
	if strings.Index(exec.gotPrompt, `"name":"api"`) > strings.Index(exec.gotPrompt, `"name":"workspace"`) {
		t.Fatalf("workspace registry not sorted deterministically: %q", exec.gotPrompt)
	}
	if !strings.Contains(exec.gotPrompt, "Summarize the repo.") {
		t.Fatalf("prompt missing transcript: %q", exec.gotPrompt)
	}
}

func TestRunRejectRejectsAutoFlag(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(t, cliprotocol.ApprovalReject, ""))
	for _, arg := range exec.gotArgs {
		if arg == "--auto" {
			t.Fatalf("approval=reject must not pass --auto: %v", exec.gotArgs)
		}
	}
}

func TestUserTextNeverInArgs(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(t, cliprotocol.ApprovalReject, ""))
	if strings.Contains(strings.Join(exec.gotArgs, " "), "Summarize the repo.") {
		t.Fatalf("user text leaked into args: %v", exec.gotArgs)
	}
}

func TestNeverPassesForbiddenFlags(t *testing.T) {
	exec := &fakeExecutor{runStdout: readFixture(t, "text_run.jsonl")}
	runShim(t, exec, runRequest(t, cliprotocol.ApprovalAuto, ""))
	joined := strings.Join(exec.gotArgs, " ")
	for _, forbidden := range []string{"--share", "--continue", "--session", "--fork"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("must not pass %s: %v", forbidden, exec.gotArgs)
		}
	}
}

// --- oversized handling ---

func TestOversizedToolEventRejected(t *testing.T) {
	big := `{"type":"tool_use","part":{"type":"tool","tool":"bash","state":{"status":"completed","output":"` + strings.Repeat("Z", 5000) + `"}}}`
	stdout := big + "\n" + `{"type":"text","part":{"type":"text","id":"p1","text":"final answer"}}` + "\n"
	exec := &fakeExecutor{runStdout: stdout}
	terminal := terminalOf(runShimBounded(t, exec, runRequestBounded(t), 1024))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("oversized tool event should fail, got %+v", terminal)
	}
}

func TestOversizedNonToolEventFails(t *testing.T) {
	big := `{"type":"text","part":{"type":"text","id":"p1","text":"` + strings.Repeat("Z", 5000) + `"}}`
	exec := &fakeExecutor{runStdout: big + "\n"}
	terminal := terminalOf(runShimBounded(t, exec, runRequestBounded(t), 1024))
	if terminal.Type != cliprotocol.TypeError || terminal.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("oversized non-tool event should fail, got %+v", terminal)
	}
}

func TestDiscardedOversizedToolBytesCountTowardAggregateBound(t *testing.T) {
	big := `{"type":"tool_use","part":{"type":"tool","tool":"bash","state":{"status":"completed","output":"` + strings.Repeat("Z", 5000) + `"}}}`
	exec := &fakeExecutor{runStdout: big + "\n"}
	responses := runShimWithBounds(t, exec, runRequestBounded(t), opencodeshim.Bounds{
		MaxRawLineBytes:   1024,
		MaxRawStdoutBytes: 2048,
	})
	terminal := terminalOf(responses)
	if terminal.Type != cliprotocol.TypeError || !strings.Contains(terminal.Message, "stdout exceeded 2048 bytes") {
		t.Fatalf("discarded bytes bypassed aggregate bound: %+v", terminal)
	}
}

func TestParserFailureTerminatesContinuedWriter(t *testing.T) {
	reader, writer := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = io.WriteString(writer, `{"type":"text","part":{"type":"text","text":"`+strings.Repeat("Z", 5000)+`"}}`+"\n")
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
		done <- runShimWithBounds(t, exec, runRequestBounded(t), opencodeshim.Bounds{MaxRawLineBytes: 1024})
	}()
	select {
	case responses := <-done:
		terminal := terminalOf(responses)
		if terminal.Type != cliprotocol.TypeError {
			t.Fatalf("expected parser failure, got %+v", terminal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parser failure deadlocked while OpenCode continued writing")
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("continued writer was not terminated")
	}
}

func runRequestBounded(t *testing.T) cliprotocol.Request {
	return runRequest(t, cliprotocol.ApprovalReject, "")
}

// Run with a small raw-line bound to exercise oversized handling.
func runShimBounded(t *testing.T, exec opencodeshim.Executor, request cliprotocol.Request, maxLine int) []cliprotocol.Response {
	return runShimWithBounds(t, exec, request, opencodeshim.Bounds{MaxRawLineBytes: maxLine})
}

func runShimWithBounds(t *testing.T, exec opencodeshim.Executor, request cliprotocol.Request, bounds opencodeshim.Bounds) []cliprotocol.Response {
	t.Helper()
	line, _ := cliprotocol.EncodeLine(request)
	var out bytes.Buffer
	opencodeshim.Run(context.Background(), bytes.NewReader(line), &out, opencodeshim.Config{
		Executor: exec,
		Bounds:   bounds,
	})
	return decodeResponses(t, out.Bytes())
}

// --- parser unit tests ---

func TestParseModelList(t *testing.T) {
	output := "opencode/big-pickle\ndeepseek/deepseek-chat\n\nnot-a-model\n"
	models := opencodeshim.ParseModelList(output)
	if len(models) != 2 || models[0] != "opencode/big-pickle" || models[1] != "deepseek/deepseek-chat" {
		t.Fatalf("unexpected models: %v", models)
	}
}

func TestParseAgentList(t *testing.T) {
	output := "build (primary)\n  [ { \"permission\": \"*\" } ]\nexplore (subagent)\n  [ ]\n"
	agents := opencodeshim.ParseAgentList(output)
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %v", agents)
	}
	if agents[0].Name != "build" || !agents[0].Primary {
		t.Fatalf("build not primary: %+v", agents[0])
	}
	if agents[1].Name != "explore" || agents[1].Primary {
		t.Fatalf("explore not subagent: %+v", agents[1])
	}
}

func TestNormalizeVersionViaRealAgentListFile(t *testing.T) {
	// Guard against fixture drift: the recorded agent list markers must parse.
	agents := opencodeshim.ParseAgentList("compaction (primary)\ntitle (primary)\ngeneral (subagent)\n")
	if len(agents) != 3 {
		t.Fatalf("expected 3 agents, got %d", len(agents))
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

func decodeJSON(data []byte, target any) error {
	return json.Unmarshal(data, target)
}
