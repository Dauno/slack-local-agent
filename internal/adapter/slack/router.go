package slack

import (
	"regexp"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// Router translates supported Slack Events API callbacks into provider-neutral
// invocations. It is immutable after construction and safe for concurrent use.
type Router struct {
	botUserID  string
	botMention *regexp.Regexp
	threadedDM bool
}

func NewRouter(botUserID string, threadedDM ...bool) Router {
	useThreadedDM := len(threadedDM) > 0 && threadedDM[0]
	return Router{
		botUserID:  botUserID,
		threadedDM: useThreadedDM,
		botMention: regexp.MustCompile(
			`<@` + regexp.QuoteMeta(botUserID) + `(?:\|[^>]+)?>`,
		),
	}
}

// Route returns false for unsupported, malformed, or deliberately ignored
// events. Authorization and thread-participation decisions belong to the bot
// use case and are intentionally not made here.
func (r Router) Route(event slackevents.EventsAPIEvent) (domain.Invocation, bool) {
	callback, ok := callbackFrom(event)
	if !ok || event.Type != slackevents.CallbackEvent {
		return domain.Invocation{}, false
	}

	teamID := callback.TeamID
	if teamID == "" {
		teamID = event.TeamID
	}

	switch inner := event.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		if inner == nil || event.InnerEvent.Type != string(slackevents.AppMention) {
			return domain.Invocation{}, false
		}
		return r.routeMention(callback.EventID, teamID, *inner)
	case slackevents.AppMentionEvent:
		if event.InnerEvent.Type != string(slackevents.AppMention) {
			return domain.Invocation{}, false
		}
		return r.routeMention(callback.EventID, teamID, inner)
	case *slackevents.MessageEvent:
		if inner == nil || event.InnerEvent.Type != string(slackevents.Message) {
			return domain.Invocation{}, false
		}
		return r.routeMessage(callback.EventID, teamID, *inner)
	case slackevents.MessageEvent:
		if event.InnerEvent.Type != string(slackevents.Message) {
			return domain.Invocation{}, false
		}
		return r.routeMessage(callback.EventID, teamID, inner)
	default:
		return domain.Invocation{}, false
	}
}

func (r Router) routeMention(eventID, teamID string, event slackevents.AppMentionEvent) (domain.Invocation, bool) {
	if event.User == r.botUserID || event.User == "" || event.BotID != "" || event.Edited != nil {
		return domain.Invocation{}, false
	}

	kind, ok := channelKindFromID(event.Channel)
	if !ok || kind == domain.ChannelDM {
		return domain.Invocation{}, false
	}

	attachments := mapFilesToAttachments(event.Files)
	if len(attachments) == 0 && strings.TrimSpace(event.Text) == "" {
		return domain.Invocation{}, false
	}

	return r.invocation(domain.Invocation{
		EventID:     eventID,
		EventType:   string(slackevents.AppMention),
		TeamID:      teamID,
		ChannelID:   event.Channel,
		ChannelKind: kind,
		UserID:      event.User,
		EventTS:     firstNonEmpty(event.TimeStamp, event.EventTimeStamp),
		ThreadTS:    event.ThreadTimeStamp,
		Text:        r.withoutBotMention(event.Text),
		Attachments: attachments,
		Trigger:     domain.TriggerMention,
	})
}

func (r Router) routeMessage(eventID, teamID string, event slackevents.MessageEvent) (domain.Invocation, bool) {
	if r.ignoreMessage(event) {
		return domain.Invocation{}, false
	}

	files := filesFromMessageEvent(event)

	invocation := domain.Invocation{
		EventID:   eventID,
		EventType: string(slackevents.Message),
		TeamID:    teamID,
		ChannelID: event.Channel,
		UserID:    event.User,
		EventTS:   firstNonEmpty(event.TimeStamp, event.EventTimeStamp),
		ThreadTS:  event.ThreadTimeStamp,
	}

	switch event.ChannelType {
	case slackevents.ChannelTypeIM:
		invocation.ChannelKind = domain.ChannelDM
		invocation.ThreadedDM = r.threadedDM
		invocation.Trigger = domain.TriggerDirectMessage
		invocation.Text = r.withoutBotMention(event.Text)
		invocation.Attachments = mapFilesToAttachments(files)
	case slackevents.ChannelTypeChannel, slackevents.ChannelTypeGroup:
		if event.ThreadTimeStamp == "" || r.botMention.MatchString(event.Text) {
			return domain.Invocation{}, false
		}
		kind, ok := channelKindFromID(event.Channel)
		if !ok || kind == domain.ChannelDM {
			return domain.Invocation{}, false
		}
		invocation.ChannelKind = kind
		invocation.Trigger = domain.TriggerThreadReply
		invocation.Text = strings.TrimSpace(event.Text)
		invocation.Attachments = mapFilesToAttachments(files)
	default: // Includes multi-party DMs.
		return domain.Invocation{}, false
	}

	return r.invocation(invocation)
}

func (r Router) ignoreMessage(event slackevents.MessageEvent) bool {
	if event.User == "" || event.User == r.botUserID || event.BotID != "" ||
		event.PreviousMessage != nil || event.DeletedTimeStamp != "" {
		return true
	}
	if event.SubType == "file_share" {
		if event.Message == nil {
			return true
		}
		message := event.Message
		return message.User == r.botUserID || message.BotID != "" || message.Edited != nil ||
			message.Hidden || message.Upload || len(message.Files) == 0
	}
	if event.SubType != "" {
		return true
	}
	if event.Message == nil {
		return false
	}
	message := event.Message
	return message.User == r.botUserID || message.BotID != "" || message.SubType != "" ||
		message.Edited != nil || message.Hidden || message.Upload || len(message.Files) != 0
}

func (r Router) invocation(invocation domain.Invocation) (domain.Invocation, bool) {
	if err := invocation.Validate(); err != nil {
		return domain.Invocation{}, false
	}
	return invocation, true
}

func (r Router) withoutBotMention(text string) string {
	return strings.TrimSpace(r.botMention.ReplaceAllString(text, ""))
}

func callbackFrom(event slackevents.EventsAPIEvent) (slackevents.EventsAPICallbackEvent, bool) {
	switch callback := event.Data.(type) {
	case *slackevents.EventsAPICallbackEvent:
		if callback == nil {
			return slackevents.EventsAPICallbackEvent{}, false
		}
		return *callback, true
	case slackevents.EventsAPICallbackEvent:
		return callback, true
	default:
		return slackevents.EventsAPICallbackEvent{}, false
	}
}

func channelKindFromID(channelID string) (domain.ChannelKind, bool) {
	if channelID == "" {
		return "", false
	}
	switch channelID[0] {
	case 'C':
		return domain.ChannelPublic, true
	case 'G':
		return domain.ChannelPrivate, true
	case 'D':
		return domain.ChannelDM, true
	default:
		return "", false
	}
}

func firstNonEmpty(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func mapFilesToAttachments(files []slack.File) []domain.Attachment {
	if len(files) == 0 {
		return nil
	}
	result := make([]domain.Attachment, 0, len(files))
	for _, f := range files {
		if f.ID == "" {
			continue
		}
		result = append(result, domain.Attachment{
			ID:       f.ID,
			Name:     f.Name,
			MIMEType: f.Mimetype,
			Size:     int64(f.Size),
		})
	}
	return result
}

func filesFromMessageEvent(event slackevents.MessageEvent) []slack.File {
	if event.SubType == "file_share" && event.Message != nil {
		return event.Message.Files
	}
	return nil
}
