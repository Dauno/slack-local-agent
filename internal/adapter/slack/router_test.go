package slack

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

type standardAgentFixture struct {
	Cases []struct {
		Name         string          `json:"name"`
		RetryAttempt int             `json:"retry_attempt"`
		RetryReason  string          `json:"retry_reason"`
		Routed       bool            `json:"routed"`
		WantEventTS  string          `json:"want_event_ts"`
		WantThreadTS string          `json:"want_thread_ts"`
		Payload      json.RawMessage `json:"payload"`
	} `json:"cases"`
}

const (
	testBot     = "U00000001"
	testUser    = "U00000002"
	testTeam    = "T00000001"
	testChannel = "C00000001"
	testGroup   = "G00000001"
	testDM      = "D00000001"
	testEventID = "Ev00000001"
	testTS      = "1720000000.000001"
	testThread  = "1719999999.000001"
)

func TestRouterRoutesSupportedInvocations(t *testing.T) {
	t.Parallel()
	router := NewRouter(testBot)

	tests := []struct {
		name      string
		innerType slackevents.EventsAPIType
		data      any
		want      domain.Invocation
	}{
		{
			name:      "direct message strips bot mentions",
			innerType: slackevents.Message,
			data: &slackevents.MessageEvent{
				Type: "message", User: testUser, Text: "  <@U00000001> hola <@U00000001|agent>  ",
				TimeStamp: testTS, Channel: testDM, ChannelType: slackevents.ChannelTypeIM,
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "message", TeamID: testTeam, ChannelID: testDM,
				ChannelKind: domain.ChannelDM, UserID: testUser, EventTS: testTS, Text: "hola",
				Trigger: domain.TriggerDirectMessage,
			},
		},
		{
			name:      "public channel mention starts thread",
			innerType: slackevents.AppMention,
			data: slackevents.AppMentionEvent{
				Type: "app_mention", User: testUser, Text: "<@U00000001> resume esto",
				TimeStamp: testTS, Channel: testChannel,
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "app_mention", TeamID: testTeam, ChannelID: testChannel,
				ChannelKind: domain.ChannelPublic, UserID: testUser, EventTS: testTS, Text: "resume esto",
				Trigger: domain.TriggerMention,
			},
		},
		{
			name:      "private channel mention keeps existing thread",
			innerType: slackevents.AppMention,
			data: &slackevents.AppMentionEvent{
				Type: "app_mention", User: testUser, Text: "pregunta <@U00000001>", TimeStamp: testTS,
				ThreadTimeStamp: testThread, Channel: testGroup,
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "app_mention", TeamID: testTeam, ChannelID: testGroup,
				ChannelKind: domain.ChannelPrivate, UserID: testUser, EventTS: testTS, ThreadTS: testThread,
				Text: "pregunta", Trigger: domain.TriggerMention,
			},
		},
		{
			name:      "public thread follow-up",
			innerType: slackevents.Message,
			data: slackevents.MessageEvent{
				Type: "message", User: testUser, Text: "continua", TimeStamp: testTS,
				ThreadTimeStamp: testThread, Channel: testChannel, ChannelType: slackevents.ChannelTypeChannel,
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "message", TeamID: testTeam, ChannelID: testChannel,
				ChannelKind: domain.ChannelPublic, UserID: testUser, EventTS: testTS, ThreadTS: testThread,
				Text: "continua", Trigger: domain.TriggerThreadReply,
			},
		},
		{
			name:      "C-prefixed thread follow-up normalizes inconsistent group type",
			innerType: slackevents.Message,
			data: slackevents.MessageEvent{
				Type: "message", User: testUser, Text: "continua", TimeStamp: testTS,
				ThreadTimeStamp: testThread, Channel: testChannel, ChannelType: slackevents.ChannelTypeGroup,
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "message", TeamID: testTeam, ChannelID: testChannel,
				ChannelKind: domain.ChannelPublic, UserID: testUser, EventTS: testTS, ThreadTS: testThread,
				Text: "continua", Trigger: domain.TriggerThreadReply,
			},
		},
		{
			name:      "private thread follow-up",
			innerType: slackevents.Message,
			data: &slackevents.MessageEvent{
				Type: "message", User: testUser, Text: "continua", EventTimeStamp: testTS,
				ThreadTimeStamp: testThread, Channel: testGroup, ChannelType: slackevents.ChannelTypeGroup,
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "message", TeamID: testTeam, ChannelID: testGroup,
				ChannelKind: domain.ChannelPrivate, UserID: testUser, EventTS: testTS, ThreadTS: testThread,
				Text: "continua", Trigger: domain.TriggerThreadReply,
			},
		},
		{
			name:      "attachment-only app mention",
			innerType: slackevents.AppMention,
			data: slackevents.AppMentionEvent{
				Type: "app_mention", User: testUser, Text: "<@U00000001>",
				TimeStamp: testTS, Channel: testChannel,
				Files: []slackapi.File{{ID: "F00000001", Name: "notes.txt", Mimetype: "text/plain", Size: 12}},
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "app_mention", TeamID: testTeam, ChannelID: testChannel,
				ChannelKind: domain.ChannelPublic, UserID: testUser, EventTS: testTS,
				Attachments: []domain.Attachment{{ID: "F00000001", Name: "notes.txt", MIMEType: "text/plain", Size: 12}},
				Trigger:     domain.TriggerMention,
			},
		},
		{
			name:      "direct message file share",
			innerType: slackevents.Message,
			data: slackevents.MessageEvent{
				Type: "message", User: testUser, TimeStamp: testTS, Channel: testDM,
				ChannelType: slackevents.ChannelTypeIM, SubType: slackapi.MsgSubTypeFileShare,
				Message: &slackapi.Msg{User: testUser, Files: []slackapi.File{{ID: "F00000002", Name: "main.go", Mimetype: "text/plain", Size: 25}}},
			},
			want: domain.Invocation{
				EventID: testEventID, EventType: "message", TeamID: testTeam, ChannelID: testDM,
				ChannelKind: domain.ChannelDM, UserID: testUser, EventTS: testTS,
				Attachments: []domain.Attachment{{ID: "F00000002", Name: "main.go", MIMEType: "text/plain", Size: 25}},
				Trigger:     domain.TriggerDirectMessage,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := router.Route(callbackEvent(tt.innerType, tt.data))
			if !ok {
				t.Fatal("Route() ignored a supported event")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Route() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRouterUsesCallbackEventIDAndTeamFallback(t *testing.T) {
	t.Parallel()
	event := callbackEvent(slackevents.Message, &slackevents.MessageEvent{
		Type: "message", User: testUser, Text: "hola", TimeStamp: testTS,
		Channel: testDM, ChannelType: slackevents.ChannelTypeIM,
	})
	event.TeamID = testTeam
	callback := event.Data.(*slackevents.EventsAPICallbackEvent)
	callback.TeamID = ""

	got, ok := NewRouter(testBot).Route(event)
	if !ok {
		t.Fatal("Route() unexpectedly ignored event")
	}
	if got.EventID != testEventID || got.TeamID != testTeam {
		t.Fatalf("Route() IDs = event %q team %q", got.EventID, got.TeamID)
	}
}

func TestRouterMarksDMThreadedOnlyWhenConfigured(t *testing.T) {
	t.Parallel()
	event := callbackEvent(slackevents.Message, &slackevents.MessageEvent{
		Type: "message", User: testUser, Text: "hola", TimeStamp: testTS,
		Channel: testDM, ChannelType: slackevents.ChannelTypeIM,
	})

	legacy, ok := NewRouter(testBot).Route(event)
	if !ok || legacy.ThreadedDM {
		t.Fatalf("legacy DM invocation = %#v, routed=%v", legacy, ok)
	}
	threaded, ok := NewRouter(testBot, true).Route(event)
	if !ok || !threaded.ThreadedDM {
		t.Fatalf("threaded DM invocation = %#v, routed=%v", threaded, ok)
	}
}

func TestRouterMatchesObservedStandardDMAgentFixtures(t *testing.T) {
	t.Parallel()
	encoded, err := os.ReadFile("testdata/standard_agent_message_im.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture standardAgentFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	router := NewRouter("U00000001", true)
	var rootDedupe []string
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			event, err := slackevents.ParseEvent(testCase.Payload, slackevents.OptionNoVerifyToken())
			if err != nil {
				t.Fatal(err)
			}
			invocation, routed := router.Route(event)
			if routed != testCase.Routed {
				t.Fatalf("Route() routed=%v invocation=%#v", routed, invocation)
			}
			if !routed {
				return
			}
			if invocation.EventTS != testCase.WantEventTS || invocation.ThreadTS != testCase.WantThreadTS || !invocation.ThreadedDM {
				t.Fatalf("Route() invocation=%#v", invocation)
			}
			if testCase.Name == "root" {
				rootDedupe = invocation.DedupeKeys()
			}
			if testCase.Name == "root_retry_after_timeout" && !reflect.DeepEqual(invocation.DedupeKeys(), rootDedupe) {
				t.Fatalf("retry dedupe keys=%v, root=%v", invocation.DedupeKeys(), rootDedupe)
			}
		})
	}
}

func TestRouterIgnoresUnsupportedOrUnsafeEvents(t *testing.T) {
	t.Parallel()
	router := NewRouter(testBot)
	baseMessage := func() *slackevents.MessageEvent {
		return &slackevents.MessageEvent{
			Type: "message", User: testUser, Text: "hola", TimeStamp: testTS,
			Channel: testDM, ChannelType: slackevents.ChannelTypeIM,
		}
	}
	baseMention := func() *slackevents.AppMentionEvent {
		return &slackevents.AppMentionEvent{
			Type: "app_mention", User: testUser, Text: "<@" + testBot + "> hola",
			TimeStamp: testTS, Channel: testChannel,
		}
	}

	tests := []struct {
		name  string
		event slackevents.EventsAPIEvent
	}{
		{name: "non callback outer event", event: func() slackevents.EventsAPIEvent {
			e := callbackEvent(slackevents.Message, baseMessage())
			e.Type = slackevents.URLVerification
			return e
		}()},
		{name: "missing callback data", event: slackevents.EventsAPIEvent{
			Type: slackevents.CallbackEvent, TeamID: testTeam,
			InnerEvent: slackevents.EventsAPIInnerEvent{Type: "message", Data: baseMessage()},
		}},
		{name: "unsupported inner event", event: callbackEvent(slackevents.AppHomeOpened, &slackevents.AppHomeOpenedEvent{})},
		{name: "inner type and data mismatch", event: callbackEvent(slackevents.AppMention, baseMessage())},
		{name: "own direct message", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.User = testBot
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "bot message ID", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.BotID = "B00000001"
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "message subtype", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.SubType = "message_changed"
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "nested subtype", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.Message = &slackapi.Msg{SubType: slackapi.MsgSubTypeMessageChanged}
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "nested edit", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.Message = &slackapi.Msg{Edited: &slackapi.Edited{User: testUser}}
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "nested file", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.Message = &slackapi.Msg{Files: []slackapi.File{{ID: "F00000001"}}}
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "message mutation", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.PreviousMessage = &slackapi.Msg{}
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "multi party direct message", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.ChannelType = slackevents.ChannelTypeMPIM
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "unthreaded channel message", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.Channel = testChannel
			m.ChannelType = slackevents.ChannelTypeChannel
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "mentioned thread message uses app mention route", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.Channel = testChannel
			m.ChannelType = slackevents.ChannelTypeChannel
			m.ThreadTimeStamp = testThread
			m.Text = "<@" + testBot + "> hola"
			return callbackEvent(slackevents.Message, m)
		}()},
		{name: "own app mention", event: func() slackevents.EventsAPIEvent {
			m := baseMention()
			m.User = testBot
			return callbackEvent(slackevents.AppMention, m)
		}()},
		{name: "bot app mention", event: func() slackevents.EventsAPIEvent {
			m := baseMention()
			m.BotID = "B00000001"
			return callbackEvent(slackevents.AppMention, m)
		}()},
		{name: "edited app mention", event: func() slackevents.EventsAPIEvent {
			m := baseMention()
			m.Edited = &slackevents.Edited{}
			return callbackEvent(slackevents.AppMention, m)
		}()},
		{name: "app mention with malformed file metadata", event: func() slackevents.EventsAPIEvent {
			m := baseMention()
			m.Files = []slackapi.File{{ID: "F00000001"}}
			return callbackEvent(slackevents.AppMention, m)
		}()},
		{name: "app mention in DM", event: func() slackevents.EventsAPIEvent {
			m := baseMention()
			m.Channel = testDM
			return callbackEvent(slackevents.AppMention, m)
		}()},
		{name: "empty after mention removal", event: callbackEvent(slackevents.AppMention, &slackevents.AppMentionEvent{
			Type: "app_mention", User: testUser, Text: " <@" + testBot + "> ", TimeStamp: testTS, Channel: testChannel,
		})},
		{name: "malformed timestamp", event: func() slackevents.EventsAPIEvent {
			m := baseMessage()
			m.TimeStamp = "not-a-timestamp"
			return callbackEvent(slackevents.Message, m)
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := router.Route(tt.event); ok {
				t.Fatalf("Route() = %#v, true; want ignored", got)
			}
		})
	}
}

func callbackEvent(innerType slackevents.EventsAPIType, data any) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type:   slackevents.CallbackEvent,
		TeamID: testTeam,
		Data: &slackevents.EventsAPICallbackEvent{
			Type: slackevents.CallbackEvent, TeamID: testTeam, EventID: testEventID,
		},
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(innerType), Data: data},
	}
}
