package acpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	stdoutBoundBytes    = 1 * 1024 * 1024
	stderrBoundBytes    = 128 * 1024
	processKillGrace    = 5 * time.Second
	jsonRPCVersion      = "2.0"
	defaultProbeTimeout = 2 * time.Minute
	defaultRunTimeout   = 30 * time.Minute
)

type Bounds struct {
	MaxStdout int
	MaxStderr int
}

type Client struct {
	executable  string
	args        []string
	bounds      Bounds
	coordinator port.OpenCodeCoordinator
}

var _ port.ExternalAgentRuntime = (*Client)(nil)

func New(executable string, args []string) *Client {
	return &Client{
		executable: executable,
		args:       append([]string(nil), args...),
		bounds: Bounds{
			MaxStdout: stdoutBoundBytes,
			MaxStderr: stderrBoundBytes,
		},
	}
}

func NewWithCoordinator(executable string, args []string, coordinator port.OpenCodeCoordinator) *Client {
	client := New(executable, args)
	client.coordinator = coordinator
	return client
}

func (c *Client) Describe(ctx context.Context) (domain.ACPInitResult, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	proc, err := c.start(ctx, "")
	if err != nil {
		return domain.ACPInitResult{}, err
	}
	defer c.terminate(proc)
	return c.initialize(proc)
}

func (c *Client) Probe(ctx context.Context, primaryPath string, additionalPaths []string, configOptions []domain.ACPConfigOption) error {
	ctx, cancel := withDefaultTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	if err := validateWorkspacePaths(primaryPath, additionalPaths); err != nil {
		return err
	}
	proc, err := c.start(ctx, primaryPath)
	if err != nil {
		return err
	}
	defer c.terminate(proc)

	init, err := c.initialize(proc)
	if err != nil {
		return err
	}
	if len(additionalPaths) > 0 && !init.SessionCapabilities.AdditionalDirectories {
		return errors.New("ACP agent does not advertise additionalDirectories; no verified fallback is available")
	}
	sessionID, _, err := c.newSession(proc, primaryPath, additionalPaths)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	if err := c.applyConfig(proc, sessionID, configOptions); err != nil {
		return err
	}
	if init.SessionCapabilities.Close {
		if err := c.closeSession(proc, sessionID); err != nil {
			return fmt.Errorf("session/close: %w", err)
		}
	}
	return nil
}

func (c *Client) Run(ctx context.Context, req domain.AcpInvocationRequest) (domain.AcpInvocationResult, error) {
	if c.coordinator != nil {
		release, acquired := c.coordinator.TryInvocation()
		if !acquired {
			return domain.AcpInvocationResult{}, errors.New("OpenCode maintenance is in progress")
		}
		defer release()
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	ctx, cancel := withDefaultTimeout(ctx, timeout)
	defer cancel()
	if err := validateWorkspacePaths(req.PrimaryPath, req.AdditionalPaths); err != nil {
		return domain.AcpInvocationResult{}, err
	}
	proc, err := c.start(ctx, req.PrimaryPath)
	if err != nil {
		return domain.AcpInvocationResult{}, fmt.Errorf("acp client start: %w", err)
	}
	defer c.terminate(proc)

	init, err := c.initialize(proc)
	if err != nil {
		return domain.AcpInvocationResult{}, fmt.Errorf("acp initialize: %w", err)
	}
	if len(req.AdditionalPaths) > 0 && !init.SessionCapabilities.AdditionalDirectories {
		return domain.AcpInvocationResult{}, errors.New("ACP agent does not advertise additionalDirectories; no verified fallback is available")
	}

	sessionID, initialConfig, err := c.newSession(proc, req.PrimaryPath, req.AdditionalPaths)
	if err != nil {
		return domain.AcpInvocationResult{}, fmt.Errorf("acp session/new: %w", err)
	}
	if len(req.ConfigOptions) > 0 && len(initialConfig.Options) == 0 {
		return domain.AcpInvocationResult{}, errors.New("ACP session did not advertise configuration options")
	}
	if err := c.applyConfig(proc, sessionID, req.ConfigOptions); err != nil {
		return domain.AcpInvocationResult{}, err
	}

	prompt := buildPrompt(req.GlobalInstruction, req.AgentInstruction, req.Task, req.AdditionalProjects, req.AdditionalPaths)
	result, err := c.prompt(proc, sessionID, prompt, req.PermissionOptionKind, req.ConfigOptions)
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	if init.SessionCapabilities.Close {
		if err := c.closeSession(proc, sessionID); err != nil {
			return domain.AcpInvocationResult{}, fmt.Errorf("acp session/close: %w", err)
		}
	}
	return result, nil
}

func validateWorkspacePaths(primary string, additional []string) error {
	seen := make(map[string]struct{}, len(additional)+1)
	for index, path := range append([]string{primary}, additional...) {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("ACP workspace path %d must be absolute", index)
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve ACP workspace path %d: %w", index, err)
		}
		canonical = filepath.Clean(canonical)
		if canonical != filepath.Clean(path) {
			return fmt.Errorf("ACP workspace path %d must be canonical", index)
		}
		if _, duplicate := seen[canonical]; duplicate {
			return fmt.Errorf("ACP workspace path %d is duplicated", index)
		}
		seen[canonical] = struct{}{}
	}
	return nil
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, exists := ctx.Deadline(); exists && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func buildPrompt(globalInstruction, agentInstruction, task string, additionalProjects, additionalPaths []string) string {
	var b strings.Builder
	b.WriteString("<<GLOBAL INSTRUCTION (trusted)>>\n")
	b.WriteString(globalInstruction)
	b.WriteString("\n\n<<AGENT INSTRUCTION (trusted)>>\n")
	b.WriteString(agentInstruction)
	if len(additionalPaths) > 0 {
		b.WriteString("\n\n<<ADDITIONAL PROJECTS (trusted)>>\n")
		for index, path := range additionalPaths {
			name := ""
			if index < len(additionalProjects) {
				name = additionalProjects[index]
			}
			b.WriteString(name)
			b.WriteString(": ")
			b.WriteString(path)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n\n<<TASK>>\n")
	b.WriteString(task)
	return b.String()
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code int `json:"code"`
}

type process struct {
	ctx      context.Context
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	messages chan wireMessage
	fatal    chan error
	done     chan error
	writeMu  sync.Mutex
	idMu     sync.Mutex
	nextID   int64
}

func (c *Client) start(ctx context.Context, dir string) (*process, error) {
	if strings.TrimSpace(c.executable) == "" {
		return nil, errors.New("ACP executable is empty")
	}
	if dir != "" && !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("ACP working directory must be absolute")
	}
	cmd := exec.Command(c.executable, c.args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "OPENCODE_DISABLE_AUTOUPDATE=true")
	configureProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}

	proc := &process{
		ctx:      ctx,
		cmd:      cmd,
		stdin:    stdin,
		messages: make(chan wireMessage, 16),
		fatal:    make(chan error, 2),
		done:     make(chan error, 1),
		nextID:   1,
	}
	go c.readStdout(proc, stdout)
	go c.drainStderr(proc, stderr)
	go func() { proc.done <- cmd.Wait() }()
	return proc, nil
}

func (c *Client) readStdout(proc *process, stdout io.Reader) {
	limited := &io.LimitedReader{R: stdout, N: int64(c.bounds.MaxStdout) + 1}
	decoder := json.NewDecoder(limited)
	for {
		var message wireMessage
		if err := decoder.Decode(&message); err != nil {
			if limited.N == 0 {
				proc.reportFatal(errors.New("ACP stdout exceeds aggregate limit"))
				return
			}
			if errors.Is(err, io.EOF) {
				break
			}
			proc.reportFatal(fmt.Errorf("ACP stdout read failed: %w", err))
			return
		}
		if limited.N == 0 || decoder.InputOffset() > int64(c.bounds.MaxStdout) {
			proc.reportFatal(errors.New("ACP stdout exceeds aggregate limit"))
			return
		}
		select {
		case proc.messages <- message:
		case <-proc.ctx.Done():
			return
		}
	}
	proc.reportFatal(errors.New("ACP stdout closed unexpectedly"))
}

func (c *Client) drainStderr(proc *process, stderr io.Reader) {
	buffer := make([]byte, 4096)
	total := 0
	reported := false
	for {
		n, err := stderr.Read(buffer)
		total += n
		if total > c.bounds.MaxStderr && !reported {
			reported = true
			proc.reportFatal(errors.New("ACP stderr exceeds aggregate limit"))
		}
		if err != nil {
			return
		}
	}
}

func (p *process) reportFatal(err error) {
	select {
	case p.fatal <- err:
	default:
	}
}

func (c *Client) terminate(proc *process) {
	if proc == nil {
		return
	}
	_ = proc.stdin.Close()
	if proc.ctx.Err() != nil {
		_ = killProcessGroup(proc.cmd)
		<-proc.done
		return
	}
	timer := time.NewTimer(processKillGrace)
	defer timer.Stop()
	select {
	case <-proc.done:
	case <-timer.C:
		_ = killProcessGroup(proc.cmd)
		<-proc.done
	}
}

func (p *process) requestID() int64 {
	p.idMu.Lock()
	defer p.idMu.Unlock()
	id := p.nextID
	p.nextID++
	return id
}

func (p *process) write(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC message: %w", err)
	}
	payload = append(payload, '\n')
	if _, err := p.stdin.Write(payload); err != nil {
		return fmt.Errorf("write JSON-RPC message: %w", err)
	}
	return nil
}

type inboundHandler func(*process, wireMessage) error

func (c *Client) request(proc *process, method string, params any, handler inboundHandler) (json.RawMessage, error) {
	id := proc.requestID()
	if err := proc.write(map[string]any{"jsonrpc": jsonRPCVersion, "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-proc.ctx.Done():
			return nil, proc.ctx.Err()
		case err := <-proc.fatal:
			return nil, err
		case message := <-proc.messages:
			if message.JSONRPC != jsonRPCVersion {
				return nil, errors.New("ACP emitted unsupported JSON-RPC version")
			}
			if message.Method != "" {
				if handler != nil {
					if err := handler(proc, message); err != nil {
						return nil, err
					}
					continue
				}
				if len(message.ID) > 0 {
					_ = c.respondError(proc, message.ID, -32601)
					return nil, fmt.Errorf("ACP called unsupported client method %q", message.Method)
				}
				continue
			}
			responseID, err := parseNumericID(message.ID)
			if err != nil || responseID != id {
				return nil, errors.New("ACP response ID does not match request")
			}
			if message.Error != nil {
				return nil, fmt.Errorf("ACP JSON-RPC error code %d", message.Error.Code)
			}
			return message.Result, nil
		}
	}
}

func parseNumericID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("missing JSON-RPC ID")
	}
	return strconv.ParseInt(string(raw), 10, 64)
}

func (c *Client) respondResult(proc *process, id json.RawMessage, result any) error {
	return proc.write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: jsonRPCVersion, ID: id, Result: result})
}

func (c *Client) respondError(proc *process, id json.RawMessage, code int) error {
	return proc.write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   map[string]any  `json:"error"`
	}{JSONRPC: jsonRPCVersion, ID: id, Error: map[string]any{"code": code, "message": "unsupported client request"}})
}

func (c *Client) notify(proc *process, method string, params any) error {
	return proc.write(map[string]any{"jsonrpc": jsonRPCVersion, "method": method, "params": params})
}

type initializeResult struct {
	ProtocolVersion json.RawMessage            `json:"protocolVersion"`
	AgentInfo       *domain.ACPAgentInfo       `json:"agentInfo"`
	Capabilities    map[string]json.RawMessage `json:"agentCapabilities"`
}

func (c *Client) initialize(proc *process) (domain.ACPInitResult, error) {
	params := map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
			"session":  map[string]any{"configOptions": map[string]any{"boolean": map[string]any{}}},
		},
		"clientInfo": map[string]string{"name": domain.ACPClientIdentity, "version": domain.ACPClientVersion},
	}
	result, err := c.request(proc, "initialize", params, nil)
	if err != nil {
		return domain.ACPInitResult{}, err
	}
	var response initializeResult
	if err := json.Unmarshal(result, &response); err != nil {
		return domain.ACPInitResult{}, errors.New("ACP initialize result is malformed")
	}
	version, err := protocolVersion(response.ProtocolVersion)
	if err != nil || version != domain.ACPProtocolVersion {
		return domain.ACPInitResult{}, errors.New("ACP agent does not support protocol version 1")
	}
	if response.AgentInfo == nil || !boundedIdentity(response.AgentInfo.Name) || !boundedIdentity(response.AgentInfo.Version) {
		return domain.ACPInitResult{}, errors.New("ACP agent identity is missing or invalid")
	}

	serverCapabilities := make(map[string]any, len(response.Capabilities))
	for name, value := range response.Capabilities {
		serverCapabilities[name] = json.RawMessage(append([]byte(nil), value...))
	}
	sessionCaps := capabilityObject(response.Capabilities["sessionCapabilities"])
	return domain.ACPInitResult{
		ProtocolVersion:    version,
		AgentInfo:          *response.AgentInfo,
		ServerCapabilities: serverCapabilities,
		SessionCapabilities: domain.ACPSessionCapabilities{
			AdditionalDirectories: capabilityEnabled(sessionCaps["additionalDirectories"]),
			Close:                 capabilityEnabled(sessionCaps["close"]),
		},
	}, nil
}

func protocolVersion(raw json.RawMessage) (string, error) {
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.Itoa(number), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", err
	}
	return text, nil
}

func boundedIdentity(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

func capabilityObject(raw json.RawMessage) map[string]json.RawMessage {
	var result map[string]json.RawMessage
	_ = json.Unmarshal(raw, &result)
	return result
}

func capabilityEnabled(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return false
	}
	return true
}

type sessionConfigOption struct {
	ID           string `json:"id"`
	CurrentValue any    `json:"currentValue"`
}

type sessionResult struct {
	SessionID     string                `json:"sessionId"`
	ConfigOptions []sessionConfigOption `json:"configOptions"`
}

func (c *Client) newSession(proc *process, primaryPath string, additional []string) (string, domain.ACPConfigState, error) {
	params := map[string]any{"cwd": primaryPath, "mcpServers": []any{}}
	if len(additional) > 0 {
		params["additionalDirectories"] = additional
	}
	result, err := c.request(proc, "session/new", params, nil)
	if err != nil {
		return "", domain.ACPConfigState{}, err
	}
	var response sessionResult
	if err := json.Unmarshal(result, &response); err != nil || !boundedIdentity(response.SessionID) {
		return "", domain.ACPConfigState{}, errors.New("ACP session/new result is malformed")
	}
	state, err := configState(response.ConfigOptions)
	if err != nil {
		return "", domain.ACPConfigState{}, err
	}
	return response.SessionID, state, nil
}

func configState(options []sessionConfigOption) (domain.ACPConfigState, error) {
	state := domain.ACPConfigState{Options: make([]domain.ACPConfigOption, 0, len(options))}
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		if !boundedIdentity(option.ID) {
			return domain.ACPConfigState{}, errors.New("ACP config state contains invalid option ID")
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return domain.ACPConfigState{}, errors.New("ACP config state contains duplicate option IDs")
		}
		seen[option.ID] = struct{}{}
		switch option.CurrentValue.(type) {
		case string, bool:
		default:
			return domain.ACPConfigState{}, errors.New("ACP config state contains unsupported value type")
		}
		state.Options = append(state.Options, domain.ACPConfigOption{ID: option.ID, Value: option.CurrentValue})
	}
	return state, nil
}

func (c *Client) applyConfig(proc *process, sessionID string, options []domain.ACPConfigOption) error {
	for index, option := range options {
		params := map[string]any{"sessionId": sessionID, "configId": option.ID, "value": option.Value}
		if _, ok := option.Value.(bool); ok {
			params["type"] = "boolean"
		}
		result, err := c.request(proc, "session/set_config_option", params, nil)
		if err != nil {
			return fmt.Errorf("acp session/set_config_option %q: %w", option.ID, err)
		}
		var response struct {
			ConfigOptions []sessionConfigOption `json:"configOptions"`
		}
		if err := json.Unmarshal(result, &response); err != nil {
			return errors.New("ACP config response is malformed")
		}
		state, err := configState(response.ConfigOptions)
		if err != nil {
			return err
		}
		if err := verifyConfigState(options[:index+1], state); err != nil {
			return fmt.Errorf("ACP config state verification failed: %w", err)
		}
	}
	return nil
}

func verifyConfigState(expected []domain.ACPConfigOption, state domain.ACPConfigState) error {
	actual := make(map[string]any, len(state.Options))
	for _, option := range state.Options {
		actual[option.ID] = option.Value
	}
	for _, option := range expected {
		value, exists := actual[option.ID]
		if !exists || !jsonValuesEqual(option.Value, value) {
			return fmt.Errorf("selected config option %q was not retained", option.ID)
		}
	}
	return nil
}

func jsonValuesEqual(left, right any) bool {
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return string(l) == string(r)
}

type promptCollector struct {
	client         *Client
	sessionID      string
	permissionKind string
	expectedConfig []domain.ACPConfigOption
	text           strings.Builder
}

func (c *Client) prompt(proc *process, sessionID, text, permissionKind string, expectedConfig []domain.ACPConfigOption) (domain.AcpInvocationResult, error) {
	collector := &promptCollector{client: c, sessionID: sessionID, permissionKind: permissionKind, expectedConfig: expectedConfig}
	result, err := c.request(proc, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]string{"type": "text", "text": text}},
	}, collector.handle)
	if err != nil {
		if proc.ctx.Err() != nil {
			_ = c.notify(proc, "session/cancel", map[string]string{"sessionId": sessionID})
			return domain.AcpInvocationResult{}, proc.ctx.Err()
		}
		return domain.AcpInvocationResult{}, fmt.Errorf("ACP session/prompt: %w", err)
	}
	var response struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return domain.AcpInvocationResult{}, errors.New("ACP prompt result is malformed")
	}
	if response.StopReason == domain.ACPStopReasonCancelled && proc.ctx.Err() != nil {
		return domain.AcpInvocationResult{}, proc.ctx.Err()
	}
	if response.StopReason != domain.ACPStopReasonEndTurn {
		return domain.AcpInvocationResult{}, fmt.Errorf("ACP run stopped with reason %q", response.StopReason)
	}
	finalText := collector.text.String()
	if strings.TrimSpace(finalText) == "" {
		return domain.AcpInvocationResult{}, errors.New("ACP run completed without assistant text")
	}
	return domain.AcpInvocationResult{Text: finalText}, nil
}

func (c *promptCollector) handle(proc *process, message wireMessage) error {
	switch message.Method {
	case "session/update":
		return c.handleUpdate(message.Params)
	case "session/request_permission":
		if len(message.ID) == 0 {
			return errors.New("ACP permission request is missing an ID")
		}
		return c.handlePermission(proc, message.ID, message.Params)
	default:
		if len(message.ID) > 0 {
			_ = c.client.respondError(proc, message.ID, -32601)
			return fmt.Errorf("ACP called unsupported client method %q", message.Method)
		}
		return nil
	}
}

func (c *promptCollector) handleUpdate(raw json.RawMessage) error {
	var notification struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind          string                `json:"sessionUpdate"`
			Content       json.RawMessage       `json:"content"`
			ConfigOptions []sessionConfigOption `json:"configOptions"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &notification); err != nil || notification.SessionID != c.sessionID {
		return errors.New("ACP session/update is malformed or belongs to another session")
	}
	switch notification.Update.Kind {
	case "agent_message_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(notification.Update.Content, &content); err != nil || content.Type != "text" {
			return errors.New("ACP agent message chunk is malformed")
		}
		if c.text.Len()+len(content.Text) > stdoutBoundBytes {
			return errors.New("ACP assistant text exceeds limit")
		}
		c.text.WriteString(content.Text)
	case "config_option_update":
		state, err := configState(notification.Update.ConfigOptions)
		if err != nil {
			return err
		}
		if err := verifyConfigState(c.expectedConfig, state); err != nil {
			return fmt.Errorf("ACP config drift detected: %w", err)
		}
	}
	return nil
}

func (c *promptCollector) handlePermission(proc *process, id json.RawMessage, raw json.RawMessage) error {
	var request struct {
		SessionID string `json:"sessionId"`
		Options   []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || request.SessionID != c.sessionID {
		return errors.New("ACP permission request is malformed or belongs to another session")
	}
	if proc.ctx.Err() != nil {
		return c.client.respondResult(proc, id, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
	}
	selected := ""
	for _, option := range request.Options {
		if option.Kind != c.permissionKind {
			continue
		}
		if !boundedIdentity(option.ID) || selected != "" {
			return errors.New("ACP permission options are ambiguous or malformed")
		}
		selected = option.ID
	}
	if selected == "" {
		return fmt.Errorf("ACP permission request does not offer %s", c.permissionKind)
	}
	return c.client.respondResult(proc, id, map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": selected}})
}

func (c *Client) closeSession(proc *process, sessionID string) error {
	_, err := c.request(proc, "session/close", map[string]string{"sessionId": sessionID}, nil)
	return err
}
