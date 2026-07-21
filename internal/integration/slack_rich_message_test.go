package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	adapterslack "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	slackapi "github.com/slack-go/slack"
)

func TestSlackMarkdownPublicationReconcilesTranslatedHistory(t *testing.T) {
	const (
		botUserID     = "U12345678"
		channelID     = "D12345678"
		correlation   = "assistant_exchange_test"
		messageTS     = "1720000001.000001"
		canonicalText = "# Result\n\n**Safe** mention: <@U99999999>"
	)

	var postedMetadata map[string]any
	var postedMarkdown string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm() error = %v", err)
			}
			postedMarkdown = r.Form.Get("markdown_text")
			if err := json.Unmarshal([]byte(r.Form.Get("metadata")), &postedMetadata); err != nil {
				t.Errorf("metadata error = %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": channelID, "ts": messageTS})
		case "/conversations.history":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{{
					"type": "message", "user": botUserID, "text": "translated by Slack", "ts": messageTS,
					"metadata": postedMetadata,
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	publisher := adapterslack.NewPublisher(client, time.Second, nil, true)
	published, err := publisher.Publish(t.Context(), domain.ReplyTarget{
		ChannelID: channelID, CorrelationID: correlation,
	}, canonicalText)
	if err != nil || published.LastMessageTS != messageTS {
		t.Fatalf("Publish() = %#v, %v", published, err)
	}
	if postedMarkdown != "# Result\n\n**Safe** mention: &lt;@U99999999>" {
		t.Fatalf("posted markdown = %q", postedMarkdown)
	}

	reader := adapterslack.NewHistoryReader(client, botUserID, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(t.Context(), port.AssistantExchangeIntent{
		ChannelID: channelID, ChannelKind: domain.ChannelDM, Content: canonicalText, CorrelationID: correlation,
	})
	if err != nil || !found || timestamp != messageTS {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestSlackStructuredPublicationUsesBlocksAndReconcilesFromMetadata(t *testing.T) {
	const (
		botUserID   = "U12345678"
		channelID   = "D12345678"
		correlation = "structured_exchange_test"
		messageTS   = "1720000002.000001"
	)
	presentation := domain.Presentation{
		FallbackMarkdown: "Name | Value\nalpha | 1",
		Sources:          []domain.Source{{Text: "Docs", URL: "https://example.com/docs"}},
		Table:            &domain.Table{Caption: "Results", Headers: []string{"Name", "Value"}, Rows: [][]string{{"alpha", "1"}}},
	}
	presentationJSON, err := json.Marshal(presentation)
	if err != nil {
		t.Fatal(err)
	}

	var postedMetadata map[string]any
	var postedText, postedMarkdown, postedBlocks, unfurlLinks, unfurlMedia string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm() error = %v", err)
			}
			postedText = r.Form.Get("text")
			postedMarkdown = r.Form.Get("markdown_text")
			postedBlocks = r.Form.Get("blocks")
			unfurlLinks = r.Form.Get("unfurl_links")
			unfurlMedia = r.Form.Get("unfurl_media")
			if err := json.Unmarshal([]byte(r.Form.Get("metadata")), &postedMetadata); err != nil {
				t.Errorf("metadata error = %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": channelID, "ts": messageTS})
		case "/conversations.history":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{{
					"type": "message", "user": botUserID, "text": "Slack-normalized fallback", "ts": messageTS,
					"metadata": postedMetadata,
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	publisher := adapterslack.NewBlockPublisher(client, time.Second, nil)
	published, err := publisher.PublishStructured(t.Context(), domain.ReplyTarget{ChannelID: channelID, CorrelationID: correlation}, presentation)
	if err != nil || published.LastMessageTS != messageTS {
		t.Fatalf("PublishStructured() = %#v, %v", published, err)
	}
	if postedText != presentation.FallbackMarkdown || postedMarkdown != "" || postedBlocks == "" {
		t.Fatalf("structured Slack fields: text=%q markdown_text=%q blocks=%q", postedText, postedMarkdown, postedBlocks)
	}
	if unfurlLinks != "false" || unfurlMedia != "false" || postedMetadata["event_type"] != "local_agent_assistant_exchange" {
		t.Fatalf("structured Slack options: unfurl_links=%q unfurl_media=%q metadata=%#v", unfurlLinks, unfurlMedia, postedMetadata)
	}
	payload, _ := postedMetadata["event_payload"].(map[string]any)
	if payload["render_mode"] != "blocks_v1" {
		t.Fatalf("render mode = %#v", payload["render_mode"])
	}

	reader := adapterslack.NewHistoryReader(client, botUserID, time.Second, nil, true)
	timestamp, found, err := reader.FindPublishedAssistantExchange(t.Context(), port.AssistantExchangeIntent{
		ChannelID: channelID, ChannelKind: domain.ChannelDM, Content: presentation.FallbackMarkdown,
		CorrelationID: correlation, PresentationJSON: string(presentationJSON),
	})
	if err != nil || !found || timestamp != messageTS {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}
