// Package fssandbox provides a local filesystem-backed sandbox executor
// for read-only repository operations within pre-registered project roots.
package fssandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

var _ sandbox.SandboxExecutor = (*Executor)(nil)

// Executor runs sandbox operations against the local filesystem. It restricts
// all operations to pre-registered project roots.
type Executor struct {
	projects       map[string]string // name → resolved absolute path
	maxOutputBytes int
}

// New creates a filesystem sandbox executor. projects maps human-readable project
// names to their absolute filesystem paths.
func New(projects map[string]string, maxOutputBytes int) (*Executor, error) {
	if len(projects) == 0 {
		return nil, errors.New("at least one project is required")
	}
	clean := make(map[string]string, len(projects))
	for name, path := range projects {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve project %q: %w", name, err)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("resolve project %q symlinks: %w", name, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("stat project %q: %w", name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("project %q is not a directory", name)
		}
		clean[name] = resolved
	}
	if maxOutputBytes <= 0 {
		return nil, errors.New("maximum output bytes must be positive")
	}
	return &Executor{projects: clean, maxOutputBytes: maxOutputBytes}, nil
}

func (e *Executor) Execute(ctx context.Context, op sandbox.SandboxOperation) (sandbox.SandboxResult, error) {
	select {
	case <-ctx.Done():
		return sandbox.SandboxResult{}, ctx.Err()
	default:
	}

	switch op.Capability {
	case domain.CapListRepos:
		return e.listRepos()
	case domain.CapListDirectory:
		return e.listDirectory(op.Args)
	case domain.CapReadFile:
		return e.readFile(op.Args)
	case domain.CapListWorktrees:
		return e.listWorktrees(op.Args)
	default:
		return sandbox.SandboxResult{}, fmt.Errorf("executor does not support %s", op.Capability)
	}
}

func (e *Executor) listRepos() (sandbox.SandboxResult, error) {
	names := make([]string, 0, len(e.projects))
	for name := range e.projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return sandbox.SandboxResult{
		Output: strings.Join(names, "\n"),
	}, nil
}

func (e *Executor) readFile(args map[string]any) (sandbox.SandboxResult, error) {
	projectName, _ := args["project"].(string)
	path, _ := args["path"].(string)

	root, ok := e.projects[projectName]
	if !ok {
		return sandbox.SandboxResult{}, fmt.Errorf("unknown project %q", projectName)
	}

	resolved := filepath.Clean(filepath.Join(root, path))
	if !withinRoot(root, resolved) {
		return sandbox.SandboxResult{}, fmt.Errorf("path %q is outside project root", path)
	}
	target, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(path)
	}
	if !withinRoot(root, target) {
		return sandbox.SandboxResult{}, pathUnavailable(path)
	}

	relPath, err := filepath.Rel(root, target)
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(path)
	}
	if anyRestrictedSegment(relPath) {
		return sandbox.SandboxResult{}, pathUnavailable(path)
	}

	info, err := os.Stat(target)
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(path)
	}
	if !info.Mode().IsRegular() {
		return sandbox.SandboxResult{}, unsupportedFile(path)
	}
	file, err := os.Open(target)
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(path)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(e.maxOutputBytes)+int64(utf8.UTFMax)))
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(path)
	}
	data, truncated, valid := textPrefix(data, e.maxOutputBytes)
	if !valid {
		return sandbox.SandboxResult{}, unsupportedFile(path)
	}

	return sandbox.SandboxResult{
		Output:      string(data),
		OutputBytes: len(data),
		Truncated:   truncated,
	}, nil
}

func (e *Executor) listDirectory(args map[string]any) (sandbox.SandboxResult, error) {
	projectName, _ := args["project"].(string)
	dirPath, _ := args["path"].(string)
	if dirPath == "" {
		dirPath = "."
	}

	root, ok := e.projects[projectName]
	if !ok {
		return sandbox.SandboxResult{}, fmt.Errorf("unknown project %q", projectName)
	}

	resolved := filepath.Clean(filepath.Join(root, dirPath))
	if !withinRoot(root, resolved) {
		return sandbox.SandboxResult{}, fmt.Errorf("path %q is outside project root", dirPath)
	}
	target, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(dirPath)
	}
	if !withinRoot(root, target) {
		return sandbox.SandboxResult{}, pathUnavailable(dirPath)
	}
	relPath, err := filepath.Rel(root, target)
	if err != nil || anyRestrictedSegment(relPath) {
		return sandbox.SandboxResult{}, pathUnavailable(dirPath)
	}

	info, err := os.Stat(target)
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(dirPath)
	}
	if !info.IsDir() {
		return sandbox.SandboxResult{}, fmt.Errorf("path %q is not a directory", dirPath)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		return sandbox.SandboxResult{}, pathUnavailable(dirPath)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if isRestrictedSegment(name) {
			continue
		}
		entryPath := filepath.Join(target, name)
		evalPath, evalErr := filepath.EvalSymlinks(entryPath)
		if evalErr != nil {
			continue
		}
		if !withinRoot(root, evalPath) {
			continue
		}
		relPath, relErr := filepath.Rel(root, evalPath)
		if relErr != nil {
			continue
		}
		if anyRestrictedSegment(relPath) {
			continue
		}
		evalInfo, statErr := os.Stat(evalPath)
		if statErr != nil {
			continue
		}
		if evalInfo.IsDir() {
			names = append(names, name+"/")
		} else if evalInfo.Mode().IsRegular() {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	output := strings.Join(names, "\n")
	truncated := false
	if len(output) > e.maxOutputBytes {
		last := 0
		for i, n := range names {
			end := last + len(n)
			if i > 0 {
				end++ // newline
			}
			if end > e.maxOutputBytes {
				names = names[:i]
				truncated = true
				break
			}
			last = end
		}
		output = strings.Join(names, "\n")
		truncated = true
	}

	return sandbox.SandboxResult{
		Output:    output,
		Truncated: truncated,
	}, nil
}

func anyRestrictedSegment(path string) bool {
	segs := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for _, seg := range segs {
		if seg == "." {
			continue
		}
		if isRestrictedSegment(seg) {
			return true
		}
	}
	return false
}

func isRestrictedSegment(seg string) bool {
	return seg == ".env" || seg == ".local-agent" || seg == ".git"
}

func textPrefix(data []byte, maxBytes int) ([]byte, bool, bool) {
	truncated := len(data) > maxBytes
	limit := min(len(data), maxBytes)
	for offset := 0; offset < limit; {
		if data[offset] == 0 {
			return nil, false, false
		}
		r, size := utf8.DecodeRune(data[offset:])
		if r == utf8.RuneError && size == 1 {
			return nil, false, false
		}
		if offset+size > maxBytes {
			return data[:offset], true, true
		}
		offset += size
	}
	return data[:limit], truncated, true
}

func pathUnavailable(path string) error {
	return fmt.Errorf("path %q is unavailable", path)
}

func unsupportedFile(path string) error {
	return fmt.Errorf("path %q is not a supported text file", path)
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func (e *Executor) listWorktrees(args map[string]any) (sandbox.SandboxResult, error) {
	projectName, _ := args["project"].(string)
	root, ok := e.projects[projectName]
	if !ok {
		return sandbox.SandboxResult{}, fmt.Errorf("unknown project %q", projectName)
	}

	worktreeDir := filepath.Join(root, ".git", "worktrees")
	entries, err := os.ReadDir(worktreeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sandbox.SandboxResult{Output: "(no worktrees)"}, nil
		}
		return sandbox.SandboxResult{}, errors.New("worktrees are unavailable")
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return sandbox.SandboxResult{Output: "(no worktrees)"}, nil
	}
	return sandbox.SandboxResult{
		Output: strings.Join(names, "\n"),
	}, nil
}
