package bot

import "strings"

type incrementalSanitizer struct {
	raw      strings.Builder
	sanitize func(string) string
	carry    int
}

func newIncrementalSanitizer(sanitize func(string) string, carry int) *incrementalSanitizer {
	if sanitize == nil {
		sanitize = func(value string) string { return value }
	}
	if carry < 128 {
		carry = 128
	}
	return &incrementalSanitizer{sanitize: sanitize, carry: carry}
}

func (s *incrementalSanitizer) Add(delta string) {
	s.raw.WriteString(delta)
}

func (s *incrementalSanitizer) Snapshot(final bool) string {
	raw := s.raw.String()
	if final {
		return s.sanitize(raw)
	}
	runes := []rune(raw)
	boundary := len(runes) - s.carry
	if boundary <= 0 {
		return ""
	}
	boundary = min(boundary, unfinishedAngle(runes), unfinishedBackticks(runes), unfinishedLink(runes), unfinishedCredential(runes))
	if boundary <= 0 {
		return ""
	}
	return s.sanitize(string(runes[:boundary]))
}

func unfinishedAngle(runes []rune) int {
	start := len(runes)
	for index, r := range runes {
		switch r {
		case '<':
			start = index
		case '>':
			start = len(runes)
		}
	}
	return start
}

func unfinishedBackticks(runes []rune) int {
	start, marker := len(runes), 0
	for index := 0; index < len(runes); {
		if runes[index] != '`' {
			index++
			continue
		}
		run := 1
		for index+run < len(runes) && runes[index+run] == '`' {
			run++
		}
		if marker == 0 {
			marker, start = run, index
		} else if run == marker {
			marker, start = 0, len(runes)
		}
		index += run
	}
	return start
}

func unfinishedLink(runes []rune) int {
	start, brackets, parens := len(runes), 0, 0
	for index, r := range runes {
		switch r {
		case '[':
			if brackets == 0 && parens == 0 {
				start = index
			}
			brackets++
		case ']':
			if brackets > 0 {
				brackets--
				if brackets == 0 && (index+1 >= len(runes) || runes[index+1] != '(') {
					start = len(runes)
				}
			}
		case '(':
			if index > 0 && runes[index-1] == ']' {
				parens = 1
			}
		case ')':
			if parens > 0 {
				parens--
				if parens == 0 {
					start = len(runes)
				}
			}
		}
	}
	return start
}

func unfinishedCredential(runes []rune) int {
	text := string(runes)
	start := len(runes)
	for _, prefix := range []string{"xoxb-", "xoxa-", "xoxp-", "xoxr-", "xoxs-", "xapp-", "sk-"} {
		index := strings.LastIndex(text, prefix)
		if index < 0 {
			continue
		}
		suffix := text[index+len(prefix):]
		valid := true
		for _, r := range suffix {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				valid = false
				break
			}
		}
		if valid {
			start = min(start, len([]rune(text[:index])))
		}
	}
	return start
}
