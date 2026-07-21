package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeBlockClient struct {
	calls        int
	channelID    string
	fallbackText string
	blocks       []slackapi.Block
	metadata     slackapi.SlackMetadata
	threadTS     string
	returnTS     string
	returnErr    error
}

func (f *fakeBlockClient) PostBlocks(_ context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	f.calls++
	f.channelID = channelID
	f.fallbackText = fallbackText
	f.blocks = blocks
	f.metadata = metadata
	f.threadTS = threadTS
	if f.returnTS != "" {
		return f.returnTS, f.returnErr
	}
	return "1700000000.000001", f.returnErr
}

func TestBlockPublisherPublishNilClient(t *testing.T) {
	p := newBlockPublisher(nil, 0, nil)
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, domain.Presentation{FallbackMarkdown: "test"})
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestBlockPublisherPublishEmptyChannel(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{}, domain.Presentation{FallbackMarkdown: "test"})
	if err == nil {
		t.Fatal("expected error for empty channel")
	}
}

func TestBlockPublisherPublishInvalidPresentation(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, domain.Presentation{})
	if err == nil {
		t.Fatal("expected error for invalid presentation")
	}
}

func TestBlockPublisherPublishFallbackOnly(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "1700000000.000001"}
	p := newBlockPublisher(fake, 0, nil)
	pub, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, domain.Presentation{FallbackMarkdown: "hello world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.LastMessageTS != "1700000000.000001" {
		t.Fatalf("expected ts, got %q", pub.LastMessageTS)
	}
	if len(fake.blocks) == 0 {
		t.Fatal("expected at least one block")
	}
	if fake.fallbackText != "hello world" {
		t.Fatalf("expected fallback 'hello world', got %q", fake.fallbackText)
	}
}

func TestBlockPublisherPublishWithSources(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)
	pub := domain.Presentation{
		FallbackMarkdown: "Sources: Example, Docs",
		Sources: []domain.Source{
			{Text: "Example", URL: "https://example.com"},
			{Text: "Docs", URL: "https://docs.example.com"},
		},
	}
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, pub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.blocks) != 1 {
		t.Fatalf("expected 1 block (context), got %d", len(fake.blocks))
	}
	contextBlock, ok := fake.blocks[0].(*slackapi.ContextBlock)
	if !ok || len(contextBlock.ContextElements.Elements) != 2 {
		t.Fatalf("unexpected source context block: %#v", fake.blocks[0])
	}
	text, ok := contextBlock.ContextElements.Elements[0].(*slackapi.TextBlockObject)
	if !ok || text.Text != "<https://example.com|Example>" {
		t.Fatalf("source link was not rendered as Slack mrkdwn: %#v", contextBlock.ContextElements.Elements[0])
	}
}

func TestBlockPublisherPublishWithTable(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)
	pub := domain.Presentation{
		FallbackMarkdown: "Name | Value\nA | 1\nB | 2",
		Table: &domain.Table{
			Caption: "Test Table",
			Headers: []string{"Name", "Value"},
			Rows:    [][]string{{"A", "1"}, {"B", "2"}},
		},
	}
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, pub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.blocks) != 2 {
		t.Fatalf("expected caption and table blocks, got %d", len(fake.blocks))
	}
	table, ok := fake.blocks[1].(*slackapi.TableBlock)
	if !ok {
		t.Fatalf("expected table block, got %T", fake.blocks[1])
	}
	if _, ok := table.Rows[1][0].(*slackapi.TableRichTextCell); !ok {
		t.Fatalf("row header cell type = %T, want rich text", table.Rows[1][0])
	}
	if _, ok := table.Rows[1][1].(*slackapi.TableRawTextCell); !ok {
		t.Fatalf("ordinary cell type = %T, want raw text", table.Rows[1][1])
	}
}

func TestBlockPublisherPublishWithCorrelationID(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{
		ChannelID:     "C123",
		CorrelationID: "test-correlation",
	}, domain.Presentation{FallbackMarkdown: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.metadata.EventType != assistantMetadataEventType {
		t.Fatalf("expected assistant_exchange metadata, got %q", fake.metadata.EventType)
	}
	if fake.metadata.EventPayload["correlation_id"] != "test-correlation" {
		t.Fatalf("expected test-correlation, got %v", fake.metadata.EventPayload["correlation_id"])
	}
}

func TestBlockPublisherPublishError(t *testing.T) {
	fake := &fakeBlockClient{returnErr: errors.New("slack error"), returnTS: ""}
	p := newBlockPublisher(fake, 0, nil)
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, domain.Presentation{FallbackMarkdown: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBlockPublisherTimeout(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, time.Minute, nil)
	if _, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, domain.Presentation{FallbackMarkdown: "test"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRenderPresentationBlocks(t *testing.T) {
	t.Run("sources only", func(t *testing.T) {
		pub := domain.Presentation{
			FallbackMarkdown: "Source A",
			Sources:          []domain.Source{{Text: "A", URL: "https://a.com"}},
		}
		blocks, err := renderPresentationBlocks(pub)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(blocks) != 1 {
			t.Fatalf("expected 1 block, got %d", len(blocks))
		}
	})

	t.Run("table only", func(t *testing.T) {
		pub := domain.Presentation{
			FallbackMarkdown: "X\n1",
			Table: &domain.Table{
				Headers: []string{"X"},
				Rows:    [][]string{{"1"}},
			},
		}
		blocks, err := renderPresentationBlocks(pub)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(blocks) != 1 {
			t.Fatalf("expected 1 block, got %d", len(blocks))
		}
	})

	t.Run("nil table", func(t *testing.T) {
		pub := domain.Presentation{
			FallbackMarkdown: "Source A",
			Sources:          []domain.Source{{Text: "A", URL: "https://a.com"}},
			Table:            nil,
		}
		blocks, err := renderPresentationBlocks(pub)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(blocks) != 1 {
			t.Fatalf("expected 1 block, got %d", len(blocks))
		}
	})
}

func TestEscapeSlackControl(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"<script>", "&lt;script&gt;"},
		{"a & b", "a &amp; b"},
	}
	for _, tt := range tests {
		got := escapeSlackControl(tt.input)
		if got != tt.want {
			t.Errorf("escapeSlackControl(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBlockPublisherImplementsInterface(t *testing.T) {
	var p *BlockPublisher
	var _ port.StructuredPublisher = p
}

func TestBlockPublisherPublishMultiPartTable(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)
	p.pace = 0

	rows := make([][]string, 120)
	for i := range rows {
		rows[i] = []string{fmt.Sprintf("item-%d", i), "value"}
	}

	pub := domain.Presentation{
		FallbackMarkdown: "Large table",
		Table: &domain.Table{
			Caption: "Large Table",
			Headers: []string{"Item", "Value"},
			Rows:    rows,
		},
	}
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, pub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fake.calls != 2 {
		t.Fatalf("expected 2 Slack messages for large table, got %d", fake.calls)
	}
}

func TestNewBlockPublisher(t *testing.T) {
	p := NewBlockPublisher(nil, 0, nil)
	if p == nil {
		t.Fatal("expected non-nil publisher")
	}
}

func TestBlockPublisherEventPayload(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)

	pub := domain.Presentation{
		FallbackMarkdown: "check the sources",
		Sources: []domain.Source{
			{Text: "Docs", URL: "https://docs.example.com"},
		},
	}
	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{
		ChannelID:     "C456",
		CorrelationID: "corr-1",
	}, pub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	payload := fake.metadata.EventPayload
	if payload["render_mode"] != "blocks_v1" {
		t.Fatalf("expected blocks_v1 render mode, got %v", payload["render_mode"])
	}
	if fmt.Sprintf("%v", payload["part_index"]) != "1" {
		t.Fatalf("expected part_index 1, got %v", payload["part_index"])
	}
	if payload["content_sha256"] == "" {
		t.Fatal("expected non-empty content_sha256")
	}
}

func TestBlockPublisherPublishWithThread(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	p := newBlockPublisher(fake, 0, nil)

	_, err := p.PublishStructured(context.Background(), domain.ReplyTarget{
		ChannelID: "C123",
		ThreadTS:  "1700000000.000001",
	}, domain.Presentation{FallbackMarkdown: "reply in thread"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fake.threadTS != "1700000000.000001" {
		t.Fatalf("expected thread_ts, got %q", fake.threadTS)
	}
}

func TestBlockPublisherRejectsOversizedFallbackBeforePosting(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	publisher := newBlockPublisher(fake, 0, nil)
	presentation := domain.Presentation{FallbackMarkdown: strings.Repeat("界", maxFallbackText+1)}

	if _, err := publisher.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, presentation); err == nil {
		t.Fatal("expected oversized fallback error")
	}
	if fake.calls != 0 {
		t.Fatalf("invalid presentation made %d Slack requests", fake.calls)
	}
}

func TestBlockPublisherSplitsTableAtAggregateCharacterLimit(t *testing.T) {
	fake := &fakeBlockClient{returnTS: "ts"}
	publisher := newBlockPublisher(fake, 0, nil)
	publisher.pace = 0
	presentation := domain.Presentation{
		FallbackMarkdown: "Large cells",
		Table: &domain.Table{
			Headers: []string{"Value"},
			Rows:    [][]string{{strings.Repeat("a", 6000)}, {strings.Repeat("b", 6000)}},
		},
	}

	if _, err := publisher.PublishStructured(context.Background(), domain.ReplyTarget{ChannelID: "C123"}, presentation); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 {
		t.Fatalf("aggregate table limit produced %d Slack messages, want 2", fake.calls)
	}
}
