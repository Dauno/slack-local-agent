package bot

import (
	"strings"
	"testing"
)

func TestIncrementalSanitizerWithholdsSplitSecretsAndSlackConstructs(t *testing.T) {
	secret := "sk-1234567890-secret"
	sanitizer := newIncrementalSanitizer(func(value string) string {
		return strings.ReplaceAll(value, secret, "[redacted]")
	}, len([]rune(secret)))
	sanitizer.Add(strings.Repeat("safe ", 40) + "sk-123")
	if snapshot := sanitizer.Snapshot(false); strings.Contains(snapshot, "sk-123") {
		t.Fatalf("partial secret escaped: %q", snapshot)
	}
	sanitizer.Add("4567890-secret and <@U123")
	if snapshot := sanitizer.Snapshot(false); strings.Contains(snapshot, secret) || strings.Contains(snapshot, "<@U123") {
		t.Fatalf("unsafe suffix escaped: %q", snapshot)
	}
	sanitizer.Add("45678> with `unfinished")
	if snapshot := sanitizer.Snapshot(false); strings.Contains(snapshot, "`unfinished") {
		t.Fatalf("unfinished code escaped: %q", snapshot)
	}
	final := sanitizer.Snapshot(true)
	if strings.Contains(final, secret) || !strings.Contains(final, "[redacted]") {
		t.Fatalf("final snapshot=%q", final)
	}
}
