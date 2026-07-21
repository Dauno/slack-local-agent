// Package secure provides output-safe representations of sensitive values.
package secure

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maskedUnset = "<not set>"
	maskMarker  = "****"
)

var credentialPattern = regexp.MustCompile(`(?:xox[a-z]-[A-Za-z0-9-]{8,}|xapp-[A-Za-z0-9-]{8,}|sk-[A-Za-z0-9_-]{8,})`)

// Mask returns a recognizable representation of a secret without revealing it.
// Known token prefixes and, for sufficiently long values, the last four
// characters are retained to help users identify which credential is active.
func Mask(value string) string {
	if value == "" {
		return maskedUnset
	}

	prefix := credentialPrefix(value)
	if len(value) <= len(prefix)+8 {
		return prefix + maskMarker
	}

	return prefix + maskMarker + value[len(value)-4:]
}

// StreamingCarryRunes returns the minimum suffix a streaming transport must
// retain so a registered secret split across deltas is never emitted early.
func (r Redactor) StreamingCarryRunes() int {
	carry := 128
	for _, secret := range r.secrets {
		carry = max(carry, utf8.RuneCountInString(secret))
	}
	return carry
}

// Redactor replaces registered credentials and commonly recognizable token
// shapes in text intended for logs, errors, or terminal output.
type Redactor struct {
	secrets []string
}

// NewRedactor constructs a Redactor. Empty values are ignored and longer
// values are replaced first so overlapping credentials cannot leak suffixes.
func NewRedactor(secrets ...string) Redactor {
	unique := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if secret != "" {
			unique[secret] = struct{}{}
		}
	}

	ordered := make([]string, 0, len(unique))
	for secret := range unique {
		ordered = append(ordered, secret)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return len(ordered[i]) > len(ordered[j])
	})

	return Redactor{secrets: ordered}
}

// String redacts sensitive values found in text.
func (r Redactor) String(text string) string {
	for _, secret := range r.secrets {
		text = strings.ReplaceAll(text, secret, Mask(secret))
	}

	return credentialPattern.ReplaceAllStringFunc(text, Mask)
}

// Error returns an error with a redacted message while preserving errors.Is
// and errors.As behavior through Unwrap.
func (r Redactor) Error(err error) error {
	if err == nil {
		return nil
	}

	return redactedError{
		message: r.String(err.Error()),
		cause:   err,
	}
}

type redactedError struct {
	message string
	cause   error
}

func (e redactedError) Error() string { return e.message }
func (e redactedError) Unwrap() error { return e.cause }

func credentialPrefix(value string) string {
	prefixes := [...]string{
		"xoxb-",
		"xoxa-",
		"xoxp-",
		"xoxr-",
		"xoxs-",
		"xapp-",
		"sk-",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return prefix
		}
	}

	return ""
}
