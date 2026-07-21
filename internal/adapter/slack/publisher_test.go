package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestSlackMetadataEventTypesUseObservedAcceptedAlphabet(t *testing.T) {
	for _, eventType := range []string{assistantMetadataEventType, confirmationMetadataEventType} {
		if eventType == "" || strings.Contains(eventType, ".") {
			t.Fatalf("metadata event type %q is rejected by Slack", eventType)
		}
		for _, r := range eventType {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
				t.Fatalf("metadata event type %q contains unsupported character %q", eventType, r)
			}
		}
	}
}

func TestSplitMarkdownSinglePartBelowLimit(t *testing.T) {
	t.Parallel()
	short := "Hello world"
	chunks := SplitMarkdown(short, SlackMarkdownChunkRunes, true)
	if len(chunks) != 1 || chunks[0] != short {
		t.Fatalf("SplitMarkdown(short) = %#v", chunks)
	}
}

func TestSplitMarkdownMultipartStaysBelowLimit(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("界", SlackMarkdownChunkRunes+100)
	chunks := SplitMarkdown(long, SlackMarkdownChunkRunes, true)
	if len(chunks) < 2 {
		t.Fatalf("SplitMarkdown(long) returned %d chunks, want >= 2", len(chunks))
	}
	for i, chunk := range chunks {
		if got := utf8.RuneCountInString(chunk); got > SlackMarkdownChunkRunes {
			t.Fatalf("chunk %d has %d Unicode code points, exceeding %d", i+1, got, SlackMarkdownChunkRunes)
		}
	}
}

func TestSplitMarkdownPreservesBlankLineBoundaries(t *testing.T) {
	t.Parallel()
	// Build multi-paragraph text exceeding the limit. The splitter
	// should produce multiple parts, splitting at blank lines.
	paragraph := strings.Repeat("x", 100)
	text := paragraph + "\n\n" + paragraph + "\n\n" + paragraph
	chunks := SplitMarkdown(text, 150, true)
	if len(chunks) < 2 {
		t.Fatalf("SplitMarkdown() = %d chunks, want >= 2", len(chunks))
	}
}

func TestSplitMarkdownPreservesFencedCodeBlocks(t *testing.T) {
	t.Parallel()
	text := "```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```"
	chunks := SplitMarkdown(text, SlackMarkdownChunkRunes, true)
	if len(chunks) != 1 {
		t.Fatalf("SplitMarkdown(fenced) = %d chunks, want 1", len(chunks))
	}
	if !strings.Contains(chunks[0], "```go") || !strings.Contains(chunks[0], "func main") {
		t.Fatalf("SplitMarkdown(fenced) altered fence: %q", chunks[0])
	}
}

func TestSplitMarkdownBoundsEveryIntermediateLine(t *testing.T) {
	t.Parallel()
	const limit = 80
	text := "prefix\n" + strings.Repeat("x", limit+25) + "\nsuffix"
	for index, chunk := range SplitMarkdown(text, limit, true) {
		if got := utf8.RuneCountInString(chunk); got > limit {
			t.Fatalf("chunk %d has %d runes, want <= %d", index+1, got, limit)
		}
	}
}

func TestSplitMarkdownDoesNotSplitInlineLinkWhenSafeBoundaryExists(t *testing.T) {
	t.Parallel()
	link := "[label with space](https://example.test/path)"
	chunks := SplitMarkdown("prefix words "+link+" trailing words", 65, true)
	for _, chunk := range chunks {
		if strings.Contains(chunk, "[label") && !strings.Contains(chunk, link) {
			t.Fatalf("inline link was split: %#v", chunks)
		}
	}
}

func TestSplitMarkdownClosesAndReopensOversizedFence(t *testing.T) {
	t.Parallel()
	const limit = 90
	text := "~~~go\n" + strings.Repeat("fmt.Println(\"hello\")\n", 12) + "~~~"
	chunks := SplitMarkdown(text, limit, true)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want multipart fence", len(chunks))
	}
	for index, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > limit || !strings.Contains(chunk, "~~~go\n") || !strings.HasSuffix(chunk, "~~~") {
			t.Fatalf("chunk %d is not a bounded complete fence: %q", index+1, chunk)
		}
	}
}

func TestSplitMarkdownRepeatsOversizedTableHeader(t *testing.T) {
	t.Parallel()
	const limit = 100
	header := "| Name | Value |\n| --- | --- |\n"
	text := header + strings.Repeat("| item | some moderately long value |\n", 8)
	chunks := SplitMarkdown(text, limit, true)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want multipart table", len(chunks))
	}
	for index, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > limit || !strings.Contains(chunk, header) {
			t.Fatalf("chunk %d missing bounded repeated header: %q", index+1, chunk)
		}
	}
}

func TestSplitMarkdownRepeatsOversizedListMarker(t *testing.T) {
	t.Parallel()
	chunks := SplitMarkdown("- "+strings.Repeat("word ", 30), 60, true)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want multipart list item", len(chunks))
	}
	for index, chunk := range chunks {
		content := chunk[strings.Index(chunk, "\n\n")+2:]
		if !strings.HasPrefix(content, "- ") {
			t.Fatalf("chunk %d did not repeat list marker: %q", index+1, chunk)
		}
	}
}

func TestNeutralizeUnsafeControls(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"user mention", "<@U12345678>", "&lt;@U12345678>"},
		{"user mention with label", "<@U12345678|john>", "&lt;@U12345678|john>"},
		{"subteam", "<!subteam^SAZ123>", "&lt;!subteam^SAZ123>"},
		{"here", "<!here>", "&lt;!here>"},
		{"channel", "<!channel>", "&lt;!channel>"},
		{"everyone", "<!everyone>", "&lt;!everyone>"},
		{"channel ref", "<#C12345678>", "&lt;#C12345678>"},
		{"channel ref with label", "<#C12345678|general>", "&lt;#C12345678|general>"},
		{"date", "<!date^1392734382^{date}|fallback>", "&lt;!date^1392734382^{date}|fallback>"},
		{"normal text untouched", "Hello <world>", "Hello <world>"},
		{"markdown link untouched", "[link](https://example.com)", "[link](https://example.com)"},
		{"code block preserved", "```\n<@U12345678>\n```", "```\n<@U12345678>\n```"},
		{"unclosed code block preserved", "```\n<@U12345678>\n", "```\n<@U12345678>\n"},
		{"inline code preserved", "`<@U12345678>` <!here>", "`<@U12345678>` &lt;!here>"},
		{"unclosed inline code does not shield controls", "`broken <!channel>", "`broken &lt;!channel>"},
		{"tilde fence requires tilde close", "~~~\n<@U12345678>\n```\n<!channel>\n~~~\n<!here>", "~~~\n<@U12345678>\n```\n<!channel>\n~~~\n&lt;!here>"},
		{"mixed", "Hi <!channel> and <@U123>", "Hi &lt;!channel> and &lt;@U123>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := neutralizeUnsafeControls(tt.input)
			if got != tt.want {
				t.Fatalf("neutralizeUnsafeControls(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPublisherPostsChunksInOrderAndReturnsLastTimestamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target domain.ReplyTarget
	}{
		{name: "direct message", target: domain.ReplyTarget{ChannelID: testDM}},
		{name: "channel thread", target: domain.ReplyTarget{ChannelID: testChannel, ThreadTS: testThread}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakePostClient{responses: []postResponse{{timestamp: "1.1"}, {timestamp: "1.2"}}}
			publisher := newPublisher(client, 2*time.Second, nil, true)
			publisher.pace = time.Second
			now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
			publisher.now = func() time.Time { return now }
			var sleeps []time.Duration
			publisher.sleep = func(_ context.Context, duration time.Duration) error {
				sleeps = append(sleeps, duration)
				now = now.Add(duration)
				return nil
			}

			text := strings.Repeat("界", SlackMarkdownChunkRunes+100)
			tt.target.CorrelationID = "intent-correlation"
			got, err := publisher.Publish(context.Background(), tt.target, text)
			if err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			if got.LastMessageTS != "1.2" {
				t.Fatalf("last timestamp = %q, want 1.2", got.LastMessageTS)
			}
			calls := client.callsSnapshot()
			if len(calls) != 2 || len(sleeps) != 1 || sleeps[0] != time.Second {
				t.Fatalf("calls = %d, sleeps = %v", len(calls), sleeps)
			}
			for index, call := range calls {
				if call.channelID != tt.target.ChannelID || call.threadTS != tt.target.ThreadTS {
					t.Fatalf("call %d target = %q/%q, want %q/%q", index+1, call.channelID, call.threadTS, tt.target.ChannelID, tt.target.ThreadTS)
				}
				if call.correlationID != tt.target.CorrelationID {
					t.Fatalf("call %d correlation = %q, want %q", index+1, call.correlationID, tt.target.CorrelationID)
				}
				if !call.hadDeadline {
					t.Fatalf("call %d did not receive an API deadline", index+1)
				}
				if call.renderMode != "markdown_v1" {
					t.Fatalf("call %d render_mode = %q, want markdown_v1", index+1, call.renderMode)
				}
				if call.partIndex != index+1 || call.partCount != len(calls) {
					t.Fatalf("call %d part = %d/%d", index+1, call.partIndex, call.partCount)
				}
				if call.contentSHA256 == "" {
					t.Fatalf("call %d missing content SHA-256 digest", index+1)
				}
			}
		})
	}
}

func TestSDKPostClientUsesMarkdownTextAndMetadata(t *testing.T) {
	t.Parallel()
	var metadata slackapi.SlackMetadata
	var formValues url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		formValues = r.Form
		if err := json.Unmarshal([]byte(r.Form.Get("metadata")), &metadata); err != nil {
			t.Errorf("metadata error = %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"D12345678","ts":"1.1"}`))
	}))
	defer server.Close()

	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	req := postRequest{
		channelID:     testDM,
		markdown:      "**bold** reply",
		threadTS:      testThread,
		correlationID: "intent-correlation",
		renderMode:    "markdown_v1",
		partIndex:     1,
		partCount:     1,
		contentSHA256: contentSHA256("**bold** reply"),
	}
	timestamp, err := (sdkPostClient{client: client}).PostMessage(t.Context(), req)
	if err != nil || timestamp != "1.1" {
		t.Fatalf("PostMessage() = %q, %v", timestamp, err)
	}
	if formValues.Get("markdown_text") != "**bold** reply" {
		t.Fatalf("markdown_text = %q, want %q", formValues.Get("markdown_text"), "**bold** reply")
	}
	if formValues.Get("text") != "" || formValues.Get("blocks") != "" {
		t.Fatalf("markdown request contains conflicting text or blocks: %v", formValues)
	}
	if formValues.Get("thread_ts") != testThread || formValues.Get("unfurl_links") != "false" || formValues.Get("unfurl_media") != "false" {
		t.Fatalf("thread or unfurl controls missing: %v", formValues)
	}
	if metadata.EventType != assistantMetadataEventType {
		t.Fatalf("metadata.EventType = %q", metadata.EventType)
	}
	if metadata.EventPayload["correlation_id"] != "intent-correlation" {
		t.Fatalf("correlation_id = %v", metadata.EventPayload["correlation_id"])
	}
	if metadata.EventPayload["render_mode"] != "markdown_v1" {
		t.Fatalf("render_mode = %v", metadata.EventPayload["render_mode"])
	}
	if v, ok := metadata.EventPayload["part_index"].(float64); !ok || int(v) != 1 {
		t.Fatalf("part_index = %v (%T)", metadata.EventPayload["part_index"], metadata.EventPayload["part_index"])
	}
	if metadata.EventPayload["content_sha256"] != contentSHA256("**bold** reply") {
		t.Fatalf("content_sha256 = %v", metadata.EventPayload["content_sha256"])
	}
}

func TestPublisherPacesAcrossResponsesPerChannel(t *testing.T) {
	client := &fakePostClient{responses: []postResponse{{timestamp: "1.1"}, {timestamp: "1.2"}, {timestamp: "2.1"}}}
	publisher := newPublisher(client, time.Second, nil, true)
	publisher.pace = time.Second
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	publisher.now = func() time.Time { return now }
	var sleeps []time.Duration
	publisher.sleep = func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		now = now.Add(duration)
		return nil
	}

	if _, err := publisher.Publish(t.Context(), domain.ReplyTarget{ChannelID: testChannel, ThreadTS: "1.0"}, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), domain.ReplyTarget{ChannelID: testChannel, ThreadTS: "2.0"}, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), domain.ReplyTarget{ChannelID: testDM}, "other channel"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{time.Second}) {
		t.Fatalf("channel pacing sleeps = %v", sleeps)
	}
}

func TestPublisherRetriesOneRateLimitUsingRetryAfter(t *testing.T) {
	t.Parallel()
	rateErr := &slackapi.RateLimitedError{RetryAfter: 37 * time.Millisecond}
	client := &fakePostClient{responses: []postResponse{{err: rateErr}, {timestamp: "2.2"}}}
	publisher := newPublisher(client, time.Second, nil, true)
	var sleeps []time.Duration
	publisher.sleep = func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		return nil
	}

	got, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testDM}, "respuesta")
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got.LastMessageTS != "2.2" || client.callCount() != 2 {
		t.Fatalf("Publish() = %#v with %d calls", got, client.callCount())
	}
	if fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{37 * time.Millisecond}) {
		t.Fatalf("retry sleeps = %v", sleeps)
	}
	calls := client.callsSnapshot()
	if calls[0].channelID != calls[1].channelID || calls[0].threadTS != calls[1].threadTS || calls[0].markdown != calls[1].markdown {
		t.Fatal("rate-limit retry changed the message target or content")
	}
}

func TestPublisherRetriesRateLimitOnlyOnceAndStops(t *testing.T) {
	t.Parallel()
	rateErr := &slackapi.RateLimitedError{RetryAfter: time.Millisecond}
	client := &fakePostClient{responses: []postResponse{{err: rateErr}, {err: rateErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil, true)
	publisher.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testDM}, strings.Repeat("x", SlackMarkdownChunkRunes+100))
	if !errors.Is(err, rateErr) {
		t.Fatalf("Publish() error = %v, want wrapped rate-limit error", err)
	}
	if client.callCount() != 2 {
		t.Fatalf("post calls = %d, want exactly 2", client.callCount())
	}
}

func TestPublisherStopsAfterChunkFailureAndReturnsLastPublishedTimestamp(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("Slack unavailable xoxb-123456789-secret")
	client := &fakePostClient{responses: []postResponse{{timestamp: "3.1"}, {err: wantErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil, true)
	publisher.sleep = func(context.Context, time.Duration) error { return nil }

	text := strings.Repeat("x", 3*SlackMarkdownChunkRunes)
	got, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testChannel, ThreadTS: testThread}, text)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Publish() error = %v, want wrapped post error", err)
	}
	if strings.Contains(err.Error(), "123456789-secret") {
		t.Fatalf("Publish() leaked token in error: %v", err)
	}
	if got.LastMessageTS != "3.1" {
		t.Fatalf("last timestamp = %q, want first successful chunk", got.LastMessageTS)
	}
	if client.callCount() != 2 {
		t.Fatalf("post calls = %d, want stop after second chunk", client.callCount())
	}
}

func TestPublisherCancellationDuringRetryWaitStopsRetry(t *testing.T) {
	t.Parallel()
	rateErr := &slackapi.RateLimitedError{RetryAfter: time.Hour}
	client := &fakePostClient{responses: []postResponse{{err: rateErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	publisher.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	_, err := publisher.Publish(ctx, domain.ReplyTarget{ChannelID: testDM}, "respuesta")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish() error = %v, want context canceled", err)
	}
	if client.callCount() != 1 {
		t.Fatalf("post calls = %d, want no retry", client.callCount())
	}
}

func TestPublisherStopsImmediatelyOnNonRateLimitFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("post failed")
	client := &fakePostClient{responses: []postResponse{{err: wantErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil, true)
	slept := false
	publisher.sleep = func(context.Context, time.Duration) error { slept = true; return nil }

	_, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testDM}, strings.Repeat("x", SlackMarkdownChunkRunes+100))
	if !errors.Is(err, wantErr) || client.callCount() != 1 || slept {
		t.Fatalf("Publish() error = %v, calls = %d, slept = %v", err, client.callCount(), slept)
	}
}

func TestPublisherValidatesInput(t *testing.T) {
	t.Parallel()
	validClient := &fakePostClient{}
	tests := []struct {
		name      string
		publisher *Publisher
		target    domain.ReplyTarget
		text      string
	}{
		{name: "missing client", publisher: newPublisher(nil, time.Second, nil, true), target: domain.ReplyTarget{ChannelID: testDM}, text: "ok"},
		{name: "missing channel", publisher: newPublisher(validClient, time.Second, nil, true), text: "ok"},
		{name: "empty text", publisher: newPublisher(validClient, time.Second, nil, true), target: domain.ReplyTarget{ChannelID: testDM}, text: " \n "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.publisher.Publish(context.Background(), tt.target, tt.text); err == nil {
				t.Fatal("Publish() error = nil")
			}
		})
	}
}

type postCall struct {
	channelID     string
	markdown      string
	threadTS      string
	correlationID string
	renderMode    string
	partIndex     int
	partCount     int
	contentSHA256 string
	hadDeadline   bool
}

type postResponse struct {
	timestamp string
	err       error
}

type fakePostClient struct {
	mu        sync.Mutex
	calls     []postCall
	responses []postResponse
}

func (c *fakePostClient) PostMessage(ctx context.Context, req postRequest) (string, error) {
	_, hadDeadline := ctx.Deadline()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, postCall{
		channelID:     req.channelID,
		markdown:      req.markdown,
		threadTS:      req.threadTS,
		correlationID: req.correlationID,
		renderMode:    req.renderMode,
		partIndex:     req.partIndex,
		partCount:     req.partCount,
		contentSHA256: req.contentSHA256,
		hadDeadline:   hadDeadline,
	})
	index := len(c.calls) - 1
	if index >= len(c.responses) {
		return fmt.Sprintf("ts-%d", index+1), nil
	}
	return c.responses[index].timestamp, c.responses[index].err
}

func (c *fakePostClient) callsSnapshot() []postCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]postCall(nil), c.calls...)
}

func (c *fakePostClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func TestSplitMarkdownNoLabelsMultipartWithoutPrefix(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("界", SlackMarkdownChunkRunes+100)
	chunks := SplitMarkdown(long, SlackMarkdownChunkRunes, false)
	if len(chunks) < 2 {
		t.Fatalf("SplitMarkdown(long, false) returned %d chunks, want >= 2", len(chunks))
	}
	for i, chunk := range chunks {
		if strings.HasPrefix(chunk, "Part ") {
			t.Fatalf("chunk %d has unexpected Part prefix: %q", i+1, chunk)
		}
		if got := utf8.RuneCountInString(chunk); got > SlackMarkdownChunkRunes {
			t.Fatalf("chunk %d has %d Unicode code points, exceeding %d", i+1, got, SlackMarkdownChunkRunes)
		}
	}
}

func TestSplitMarkdownNoLabelsChunksWithinLimit(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 3*SlackMarkdownChunkRunes)
	chunks := SplitMarkdown(long, SlackMarkdownChunkRunes, false)
	for i, chunk := range chunks {
		if got := utf8.RuneCountInString(chunk); got > SlackMarkdownChunkRunes {
			t.Fatalf("chunk %d has %d runes, want <= %d", i+1, got, SlackMarkdownChunkRunes)
		}
	}
}

func TestSplitMarkdownNoLabelsMayHaveFewerParts(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("界", SlackMarkdownChunkRunes+100)
	withLabels := SplitMarkdown(long, SlackMarkdownChunkRunes, true)
	withoutLabels := SplitMarkdown(long, SlackMarkdownChunkRunes, false)
	if len(withoutLabels) > len(withLabels) {
		t.Fatalf("without labels has %d parts, with labels has %d; expected <= ", len(withoutLabels), len(withLabels))
	}
}

func TestSplitMarkdownNoLabelsShortTextIdenticalToOriginal(t *testing.T) {
	t.Parallel()
	short := "Hello world"
	got := SplitMarkdown(short, SlackMarkdownChunkRunes, false)
	if len(got) != 1 || got[0] != short {
		t.Fatalf("SplitMarkdown(short, false) = %#v, want [%q]", got, short)
	}
}
