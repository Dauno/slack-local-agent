package codexshim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrExecutableNotFound indicates the Codex executable cannot be resolved.
var ErrExecutableNotFound = errors.New("codex executable not found")

// ModelInfo is one bundled Codex model with its supported reasoning efforts.
// It carries only the version-pinned fields required for validation; catalog
// instructions and unrelated metadata are never decoded or retained.
type ModelInfo struct {
	Slug    string
	Efforts []string
}

// Process is one started `codex exec` invocation.
type Process interface {
	Stdout() io.Reader
	// Terminate stops the full Codex subprocess group. It is safe to call
	// after the process has already exited.
	Terminate() error
	// Wait reaps the child and returns its exit error, if any.
	Wait() error
	// Diagnostics returns content-free metadata for bounded native stderr.
	Diagnostics() ProcessDiagnostics
}

// ProcessDiagnostics is safe to include in errors because it never contains
// child-controlled stderr content.
type ProcessDiagnostics struct {
	StderrBytes     int64
	StderrTruncated bool
}

// Executor abstracts Codex and Git invocations so tests can stay hermetic.
type Executor interface {
	Version(ctx context.Context) (string, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
	GitWorktree(ctx context.Context, dir string) (string, error)
	StartRun(ctx context.Context, args []string, prompt string) (Process, error)
}

// NewExecutor creates the real PATH-resolved Codex executor.
func NewExecutor(executable string, bounds Bounds) Executor {
	return &realExecutor{executable: executable, bounds: bounds.withDefaults()}
}

type realExecutor struct {
	executable string
	bounds     Bounds
}

func (e *realExecutor) lookup() (string, error) {
	resolved, err := exec.LookPath(e.executable)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrExecutableNotFound, err)
	}
	return resolved, nil
}

// capture runs one bounded non-model command and returns its stdout. Native
// stderr content never leaves the bounded capture; errors carry only
// content-free metadata.
func (e *realExecutor) capture(ctx context.Context, executable string, maxStdout int, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	stdout := newBoundedCapture(maxStdout)
	stderr := newBoundedCapture(e.bounds.MaxRawStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	runErr := cmd.Run()
	name := cmd.Args[0]
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("%s %s cancelled: %w", name, strings.Join(args, " "), ctxErr)
	}
	if stdout.Truncated() {
		return "", fmt.Errorf("%s %s output exceeded %d bytes%s", name, strings.Join(args, " "), maxStdout, diagnosticDetail(stderr.Summary()))
	}
	if runErr != nil {
		return "", fmt.Errorf("%s %s %s%s", name, strings.Join(args, " "), processFailure(runErr), diagnosticDetail(stderr.Summary()))
	}
	return stdout.String(), nil
}

func (e *realExecutor) captureCodex(ctx context.Context, maxStdout int, args ...string) (string, error) {
	resolved, err := e.lookup()
	if err != nil {
		return "", err
	}
	return e.capture(ctx, resolved, maxStdout, args...)
}

func (e *realExecutor) Version(ctx context.Context) (string, error) {
	output, err := e.captureCodex(ctx, e.bounds.MaxRawStdoutBytes, "--version")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed, nil
		}
	}
	return "", errors.New("codex --version produced no output")
}

func (e *realExecutor) ListModels(ctx context.Context) ([]ModelInfo, error) {
	output, err := e.captureCodex(ctx, e.bounds.MaxCatalogBytes, "debug", "models", "--bundled")
	if err != nil {
		return nil, err
	}
	return ParseModelCatalog([]byte(output))
}

// GitWorktree runs `git -C <dir> rev-parse --is-inside-work-tree` with fixed
// arguments, no shell, and bounded output. The caller compares the normalized
// result against exactly "true".
func (e *realExecutor) GitWorktree(ctx context.Context, dir string) (string, error) {
	resolved, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git executable not found: %v", err)
	}
	return e.capture(ctx, resolved, 1<<10, "-C", dir, "rev-parse", "--is-inside-work-tree")
}

// ParseModelCatalog decodes only the version-pinned fields the mapper needs
// from `codex debug models --bundled`. The command is experimental in Codex
// 0.144.5; a schema change is a compatibility failure.
func ParseModelCatalog(data []byte) ([]ModelInfo, error) {
	var catalog struct {
		Models []struct {
			Slug                     string `json:"slug"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode bundled Codex model catalog: schema is not compatible with this shim")
	}
	if len(catalog.Models) == 0 {
		return nil, errors.New("bundled Codex model catalog contains no models")
	}
	models := make([]ModelInfo, 0, len(catalog.Models))
	for index, model := range catalog.Models {
		if strings.TrimSpace(model.Slug) == "" {
			return nil, fmt.Errorf("bundled Codex model catalog entry %d has no slug", index)
		}
		info := ModelInfo{Slug: model.Slug}
		for _, level := range model.SupportedReasoningLevels {
			if level.Effort != "" {
				info.Efforts = append(info.Efforts, level.Effort)
			}
		}
		models = append(models, info)
	}
	return models, nil
}

func (e *realExecutor) StartRun(ctx context.Context, args []string, prompt string) (Process, error) {
	resolved, err := e.lookup()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex stdout: %w", err)
	}
	stderr := newBoundedCapture(e.bounds.MaxRawStderrBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}

	return &realProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type realProcess struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr *boundedCapture
}

func (p *realProcess) Stdout() io.Reader { return p.stdout }

func (p *realProcess) Terminate() error { return killProcessGroup(p.cmd) }

func (p *realProcess) Wait() error { return p.cmd.Wait() }

func (p *realProcess) Diagnostics() ProcessDiagnostics {
	if p == nil || p.stderr == nil {
		return ProcessDiagnostics{}
	}
	return p.stderr.Summary()
}

type boundedCapture struct {
	limit int
	data  []byte
	total int64
}

func newBoundedCapture(limit int) *boundedCapture {
	return &boundedCapture{limit: limit}
}

func (b *boundedCapture) Write(data []byte) (int, error) {
	b.total += int64(len(data))
	remaining := b.limit - len(b.data)
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		b.data = append(b.data, data[:remaining]...)
	}
	return len(data), nil
}

func (b *boundedCapture) String() string { return string(b.data) }

func (b *boundedCapture) Truncated() bool { return b.total > int64(b.limit) }

func (b *boundedCapture) Summary() ProcessDiagnostics {
	return ProcessDiagnostics{StderrBytes: b.total, StderrTruncated: b.Truncated()}
}

func processFailure(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return fmt.Sprintf("exited with code %d", code)
		}
		return "was terminated"
	}
	return "failed"
}

func diagnosticDetail(diagnostic ProcessDiagnostics) string {
	if diagnostic.StderrBytes == 0 {
		return ""
	}
	if diagnostic.StderrTruncated {
		return " (native stderr omitted; bounded capture was truncated)"
	}
	return fmt.Sprintf(" (native stderr omitted; %d bytes)", diagnostic.StderrBytes)
}
