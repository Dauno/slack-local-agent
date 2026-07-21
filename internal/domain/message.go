package domain

import (
	"slices"
	"time"
	"unicode/utf8"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role       Role
	Content    string
	UserID     string
	ExternalTS string
	CreatedAt  time.Time
}

type ConversationMetadata struct {
	Key         ConversationKey
	TeamID      string
	ChannelID   string
	ChannelKind ChannelKind
	RootTS      string
	LastTS      string
}

func MetadataFor(i Invocation, key ConversationKey) ConversationMetadata {
	rootTS := i.EventTS
	if i.ChannelKind == ChannelDM && !i.ThreadedDM {
		rootTS = ""
	} else if i.ThreadTS != "" {
		rootTS = i.ThreadTS
	}
	return ConversationMetadata{
		Key:         key,
		TeamID:      i.TeamID,
		ChannelID:   i.ChannelID,
		ChannelKind: i.ChannelKind,
		RootTS:      rootTS,
		LastTS:      i.EventTS,
	}
}

type ContextLimits struct {
	MaxMessages int
	MaxChars    int
}

func LimitMessages(messages []Message, limits ContextLimits) []Message {
	if len(messages) == 0 || limits.MaxMessages <= 0 || limits.MaxChars <= 0 {
		return nil
	}
	start := max(0, len(messages)-limits.MaxMessages)
	selected := slices.Clone(messages[start:])
	remaining := limits.MaxChars
	result := make([]Message, 0, len(selected))
	for idx := len(selected) - 1; idx >= 0 && remaining > 0; idx-- {
		message := selected[idx]
		runes := []rune(message.Content)
		if len(runes) > remaining {
			if idx == len(selected)-1 {
				message.Content = string(runes[:remaining])
			} else {
				message.Content = string(runes[len(runes)-remaining:])
			}
		}
		remaining -= utf8.RuneCountInString(message.Content)
		result = append(result, message)
	}
	slices.Reverse(result)
	return result
}
