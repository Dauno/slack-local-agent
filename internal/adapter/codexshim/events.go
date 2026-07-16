package codexshim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ParsedRun is the normalized outcome of one `codex exec --json` invocation.
// Reasoning, command output, diffs, file contents, paths, MCP payloads, and
// thread IDs never appear here.
type ParsedRun struct {
	Text      string
	Completed bool
	Failed    bool
}

// codexEvent decodes only the version-pinned fields the mapper needs. Native
// command text, paths, diffs, and tool payloads are intentionally absent so
// an oversized payload is never materialized as an unbounded object.
type codexEvent struct {
	Type string `json:"type"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

// activityItemTypes are the known non-message item types reported as bounded
// payload-free activity. Names must remain safe diagnostic tokens.
var activityItemTypes = map[string]struct{}{
	"command_execution": {},
	"file_change":       {},
	"mcp_tool_call":     {},
	"web_search":        {},
	"todo_list":         {},
}

// ParseRunEvents drains Codex's JSONL event stream to EOF under the mapper's
// raw line and aggregate stdout bounds. The last non-empty completed
// agent_message before turn.completed is the final result. turn.failed and
// error events fail the run even if text was already observed. A top-level
// error may precede Codex's terminal turn.failed event, so only turn events
// close the stream. Any event after a terminal turn fails the run.
func ParseRunEvents(reader io.Reader, bounds Bounds, onActivity func(name, status string)) (ParsedRun, error) {
	bounds = bounds.withDefaults()
	buffered := bufio.NewReaderSize(reader, bounds.MaxRawLineBytes)

	var (
		parsed     ParsedRun
		candidate  string
		terminal   bool
		totalBytes int64
	)

	for {
		line, consumed, truncated, err := readBoundedLine(buffered, bounds.MaxRawLineBytes)
		if err == io.EOF {
			break
		}
		if err != nil {
			return ParsedRun{}, fmt.Errorf("read codex stdout: %w", err)
		}
		totalBytes += int64(consumed)
		if totalBytes > int64(bounds.MaxRawStdoutBytes) {
			return ParsedRun{}, fmt.Errorf("codex stdout exceeded %d bytes", bounds.MaxRawStdoutBytes)
		}
		if truncated {
			// Reject before unmarshalling. Native payloads are opaque and
			// must not bypass the mapper's raw line bound.
			return ParsedRun{}, fmt.Errorf("codex emitted an event longer than %d bytes", bounds.MaxRawLineBytes)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var event codexEvent
		if err := json.Unmarshal(line, &event); err != nil {
			// With --json, stdout is reserved for JSONL. A non-JSON line is a
			// framing failure, not a diagnostic to skip.
			return ParsedRun{}, fmt.Errorf("codex emitted a non-JSON stdout line")
		}
		if terminal {
			return ParsedRun{}, fmt.Errorf("codex emitted an event after a terminal turn")
		}

		switch event.Type {
		case "thread.started", "turn.started", "item.started", "item.updated":
			// Counted diagnostically at most; payloads are never inspected
			// and thread IDs are never persisted or resumed.
		case "item.completed":
			if event.Item == nil {
				return ParsedRun{}, fmt.Errorf("codex emitted a malformed item.completed event")
			}
			switch {
			case event.Item.Type == "agent_message":
				if strings.TrimSpace(event.Item.Text) != "" {
					candidate = event.Item.Text
				}
			case event.Item.Type == "reasoning":
				// Reasoning must never become final ADK text.
			default:
				if _, known := activityItemTypes[event.Item.Type]; known && onActivity != nil {
					onActivity(event.Item.Type, "completed")
				}
				// Unknown item types are ignored after bounded envelope
				// validation.
			}
		case "turn.completed":
			parsed.Completed = true
			parsed.Text = candidate
			terminal = true
		case "turn.failed":
			parsed.Failed = true
			terminal = true
		case "error":
			// Codex 0.144.5 can emit an unrecoverable error notification and
			// then the terminal turn.failed event. Preserve failure state but
			// keep parsing that valid sequence.
			parsed.Failed = true
		default:
			// Unknown event types are ignored after bounded envelope
			// validation. This is forward-compatible only within the pinned
			// Codex version.
		}
	}

	return parsed, nil
}

// readBoundedLine returns one bounded prefix and the exact number of consumed
// bytes, including discarded oversized content and a trailing newline.
func readBoundedLine(reader *bufio.Reader, maxLineBytes int) ([]byte, int, bool, error) {
	var (
		prefix       []byte
		consumed     int
		contentBytes int
	)
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += len(fragment)
		fragmentContent := len(fragment)
		if fragmentContent > 0 && fragment[fragmentContent-1] == '\n' {
			fragmentContent--
		}
		contentBytes += fragmentContent
		if remaining := maxLineBytes - len(prefix); remaining > 0 {
			if remaining > len(fragment) {
				remaining = len(fragment)
			}
			prefix = append(prefix, fragment[:remaining]...)
		}

		switch err {
		case nil:
			return bytes.TrimRight(prefix, "\r\n"), consumed, contentBytes > maxLineBytes, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if consumed == 0 {
				return nil, 0, false, io.EOF
			}
			return bytes.TrimRight(prefix, "\r\n"), consumed, contentBytes > maxLineBytes, nil
		default:
			return bytes.TrimRight(prefix, "\r\n"), consumed, contentBytes > maxLineBytes, err
		}
	}
}
