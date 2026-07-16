package fssandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

func TestReadFileRejectsSymlinkOutsideRegisteredProject(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Fatal(err)
	}
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "outside-link"},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("read symlink error = %v", err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), outside) {
		t.Fatalf("read symlink error disclosed a host path: %v", err)
	}
}

func TestReadFileRespectsConfiguredOutputLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := New(map[string]string{"project": root}, 4)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "large.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "1234" || result.OutputBytes != 4 || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
}

func TestReadFileTruncatesAtUTF8Boundary(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "two-byte rune", data: "abcé", want: "abc"},
		{name: "three-byte rune", data: "ab€", want: "ab"},
		{name: "four-byte rune", data: "a🙂", want: "a"},
		{name: "rune after boundary", data: "abcdé", want: "abcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "text.txt"), []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			executor, err := New(map[string]string{"project": root}, 4)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
				Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "text.txt"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Output != tt.want || result.OutputBytes != len(tt.want) || !result.Truncated {
				t.Fatalf("result = %#v, want output %q", result, tt.want)
			}
		})
	}
}

func TestReadFileUnavailableErrorDoesNotDiscloseHostPath(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "missing", "secret.txt")
	if err := os.Symlink(external, filepath.Join(root, "broken-link")); err != nil {
		t.Fatal(err)
	}
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "broken-link"},
	})
	if err == nil {
		t.Fatal("expected unavailable path error")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), external) {
		t.Fatalf("error disclosed a host path: %v", err)
	}
}

func TestListDirectorySortsEntries(t *testing.T) {
	root := t.TempDir()
	dirs := []string{"zzz-dir", "aaa-dir", "bbb-dir"}
	for _, d := range dirs {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	files := []string{"zzz.txt", "aaa.txt", "bbb.txt"}
	for _, f := range files {
		os.WriteFile(filepath.Join(root, f), []byte("content"), 0o600)
	}
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(result.Output)
	if len(lines) < 6 {
		t.Fatalf("expected at least 6 entries, got %d: %s", len(lines), result.Output)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			t.Fatalf("entries not sorted: %q > %q", lines[i-1], lines[i])
		}
	}
}

func TestListDirectoryTrailingSlashForDirs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "mydir"), 0o755)
	os.WriteFile(filepath.Join(root, "myfile.txt"), []byte("hi"), 0o600)
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(result.Output)
	hasDir := false
	hasFile := false
	for _, l := range lines {
		if l == "mydir/" {
			hasDir = true
		}
		if l == "myfile.txt" {
			hasFile = true
		}
	}
	if !hasDir || !hasFile {
		t.Fatalf("result = %q", result.Output)
	}
}

func TestListDirectoryHiddenRestrictedEntries(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".env"), []byte("secret"), 0o600)
	os.MkdirAll(filepath.Join(root, ".local-agent"), 0o755)
	os.WriteFile(filepath.Join(root, "visible.txt"), []byte("ok"), 0o600)
	os.WriteFile(filepath.Join(root, ".env.example"), []byte("template"), 0o600)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored"), 0o600)
	os.MkdirAll(filepath.Join(root, ".github"), 0o755)
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(result.Output)
	forbidden := map[string]bool{".git": true, ".env": true, ".local-agent": true}
	allowed := map[string]bool{"visible.txt": true, ".env.example": true, ".gitignore": true, ".github/": true}
	for _, line := range lines {
		if forbidden[line] {
			t.Fatalf("restricted entry %q should not appear in listing", line)
		}
		delete(allowed, line)
	}
	if len(allowed) > 0 {
		t.Fatalf("allowed entries missing from listing: %v", allowed)
	}
}

func TestReadFileRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "mydir"), 0o755)
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "mydir"},
	})
	if err == nil || !strings.Contains(err.Error(), "supported text file") {
		t.Fatalf("read directory error = %v", err)
	}
}

func TestReadFileRejectsRestrictedPath(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".env"), []byte("secret"), 0o600)
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": ".env"},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("read .env error = %v", err)
	}
}

func TestReadFileRejectsNestedRestrictedPath(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "services", "api"), 0o755)
	os.WriteFile(filepath.Join(root, "services", "api", ".env"), []byte("secret"), 0o600)
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "services/api/.env"},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("read nested .env error = %v", err)
	}
}

func TestReadFileRejectsBinaryData(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0x00, 0xFF, 0xFE, 0xFD}, 0o600)
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "binary.bin"},
	})
	if err == nil || !strings.Contains(err.Error(), "supported text file") {
		t.Fatalf("read binary error = %v", err)
	}
}

func TestReadFileRejectsInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	invalid := []byte{'h', 'e', 'l', 'l', 'o', 0xFF, 0xFE}
	os.WriteFile(filepath.Join(root, "bad.txt"), invalid, 0o600)
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "bad.txt"},
	})
	if err == nil || !strings.Contains(err.Error(), "supported text file") {
		t.Fatalf("read invalid UTF-8 error = %v", err)
	}
}

func TestReadFileRejectsNULByte(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "nul.txt"), []byte{0x00}, 0o600)
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "nul.txt"},
	})
	if err == nil || !strings.Contains(err.Error(), "supported text file") {
		t.Fatalf("read NUL error = %v", err)
	}
}

func TestListDirectoryRejectsSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	os.MkdirAll(outside, 0o755)
	os.Symlink(outside, filepath.Join(root, "escape-link"))
	os.WriteFile(filepath.Join(root, "safe.txt"), []byte("ok"), 0o600)
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Output, "escape-link") {
		t.Fatalf("outside symlink should not appear in listing: %s", result.Output)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "escape-link"},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("outside symlink error = %v", err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), outside) {
		t.Fatalf("outside symlink error disclosed a host path: %v", err)
	}
}

func TestReadFileRejectsSymlinkToRestricted(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".env"), []byte("secret"), 0o600)
	os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, "alias.env"))
	os.WriteFile(filepath.Join(root, "safe.txt"), []byte("ok"), 0o600)
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "alias.env"},
	})
	if err == nil {
		t.Fatal("symlink to restricted file should be denied")
	}
}

func TestListDirectoryRejectsSymlinkToRestrictedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".git"), filepath.Join(root, "metadata")); err != nil {
		t.Fatal(err)
	}
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}

	rootResult, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rootResult.Output, "metadata") {
		t.Fatalf("restricted directory alias appeared in listing: %q", rootResult.Output)
	}

	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "metadata"},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("restricted directory alias error = %v", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("restricted directory alias error disclosed root: %v", err)
	}
}

func TestListDirectoryAcceptsSymlinkWithinRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "target.txt"), []byte("hello"), 0o600)
	os.Symlink(filepath.Join(root, "sub", "target.txt"), filepath.Join(root, "link.txt"))
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "link.txt") {
		t.Fatalf("safe symlink should appear in listing: %s", result.Output)
	}
	readResult, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "link.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if readResult.Output != "hello" {
		t.Fatalf("read symlink = %q", readResult.Output)
	}
}

func TestListDirectoryBoundaryNoTruncation(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "bb", "ccc", "d"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executor, err := New(map[string]string{"project": root}, 8)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatal("expected truncation because the complete listing exceeds the limit")
	}
	if result.Output != "a\nbb\nccc" {
		t.Fatalf("expected exact-fitting prefix %q, got %q", "a\nbb\nccc", result.Output)
	}
}

func TestListDirectoryTruncatesOutput(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("file-with-long-name-%d.txt", i)), []byte("x"), 0o600)
	}
	executor, err := New(map[string]string{"project": root}, 50)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatal("expected truncated listing")
	}
}

func TestListDirectoryNonRecursive(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "dir", "nested"), 0o755)
	os.WriteFile(filepath.Join(root, "dir", "nested", "deep.txt"), []byte("deep"), 0o600)
	executor, err := New(map[string]string{"project": root}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "project", "path": "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Output, "deep.txt") {
		t.Fatalf("nested file should not appear in root listing: %s", result.Output)
	}
	if !strings.Contains(result.Output, "dir/") {
		t.Fatalf("subdir should appear: %s", result.Output)
	}
}

func TestListDirectoryUnknownProject(t *testing.T) {
	root := t.TempDir()
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListDirectory, Args: map[string]any{"project": "nonexistent", "path": "."},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Fatalf("unknown project error = %v", err)
	}
}

func TestListReposReturnsDeterministicOrder(t *testing.T) {
	projects := map[string]string{
		"zzz": t.TempDir(),
		"aaa": t.TempDir(),
	}
	executor, err := New(projects, 1024)
	if err != nil {
		t.Fatal(err)
	}
	result1, _ := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListRepos,
	})
	result2, _ := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapListRepos,
	})
	if result1.Output != result2.Output {
		t.Fatalf("list_repos not deterministic: %q vs %q", result1.Output, result2.Output)
	}
	lines := splitLines(result1.Output)
	if len(lines) != 2 || lines[0] != "aaa" || lines[1] != "zzz" {
		t.Fatalf("unexpected order: %v", lines)
	}
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
