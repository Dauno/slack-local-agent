package domain

import "testing"

func validInvocation() Invocation {
	return Invocation{
		EventID:     "Ev123",
		EventType:   "app_mention",
		TeamID:      "T12345678",
		ChannelID:   "C12345678",
		ChannelKind: ChannelPublic,
		UserID:      "U12345678",
		EventTS:     "1700000000.000001",
		Text:        "hello",
		Trigger:     TriggerMention,
	}
}

func TestConversationKeyAndReplyTarget(t *testing.T) {
	tests := []struct {
		name       string
		invocation Invocation
		wantKey    ConversationKey
		wantTarget ReplyTarget
	}{
		{
			name:       "channel root",
			invocation: validInvocation(),
			wantKey:    "slack:T12345678:channel:C12345678:thread:1700000000.000001",
			wantTarget: ReplyTarget{ChannelID: "C12345678", ThreadTS: "1700000000.000001"},
		},
		{
			name: "channel thread",
			invocation: func() Invocation {
				i := validInvocation()
				i.EventTS = "1700000001.000002"
				i.ThreadTS = "1700000000.000001"
				return i
			}(),
			wantKey:    "slack:T12345678:channel:C12345678:thread:1700000000.000001",
			wantTarget: ReplyTarget{ChannelID: "C12345678", ThreadTS: "1700000000.000001"},
		},
		{
			name: "dm",
			invocation: Invocation{
				EventType: "message.im", TeamID: "T12345678", ChannelID: "D12345678",
				ChannelKind: ChannelDM, UserID: "U12345678", EventTS: "1700000000.000001",
				Text: "hello", Trigger: TriggerDirectMessage,
			},
			wantKey:    "slack:T12345678:dm:D12345678",
			wantTarget: ReplyTarget{ChannelID: "D12345678"},
		},
		{
			name: "threaded dm root",
			invocation: Invocation{
				EventType: "message.im", TeamID: "T12345678", ChannelID: "D12345678",
				ChannelKind: ChannelDM, UserID: "U12345678", EventTS: "1700000000.000001",
				Text: "hello", Trigger: TriggerDirectMessage, ThreadedDM: true,
			},
			wantKey:    "slack:T12345678:dm:D12345678:thread:1700000000.000001",
			wantTarget: ReplyTarget{ChannelID: "D12345678", ThreadTS: "1700000000.000001"},
		},
		{
			name: "threaded dm reply",
			invocation: Invocation{
				EventType: "message.im", TeamID: "T12345678", ChannelID: "D12345678",
				ChannelKind: ChannelDM, UserID: "U12345678", EventTS: "1700000001.000002", ThreadTS: "1700000000.000001",
				Text: "continue", Trigger: TriggerDirectMessage, ThreadedDM: true,
			},
			wantKey:    "slack:T12345678:dm:D12345678:thread:1700000000.000001",
			wantTarget: ReplyTarget{ChannelID: "D12345678", ThreadTS: "1700000000.000001"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, err := tt.invocation.ConversationKey()
			if err != nil {
				t.Fatal(err)
			}
			if gotKey != tt.wantKey {
				t.Fatalf("key = %q, want %q", gotKey, tt.wantKey)
			}
			if got := tt.invocation.ReplyTarget(); got != tt.wantTarget {
				t.Fatalf("target = %#v, want %#v", got, tt.wantTarget)
			}
		})
	}
}

func TestMetadataForThreadedDMRetainsRoot(t *testing.T) {
	invocation := Invocation{
		EventType: "message.im", TeamID: "T12345678", ChannelID: "D12345678",
		ChannelKind: ChannelDM, UserID: "U12345678", EventTS: "1700000001.000002",
		ThreadTS: "1700000000.000001", Text: "continue", Trigger: TriggerDirectMessage,
		ThreadedDM: true,
	}
	key, err := invocation.ConversationKey()
	if err != nil {
		t.Fatal(err)
	}
	metadata := MetadataFor(invocation, key)
	if metadata.RootTS != invocation.ThreadTS {
		t.Fatalf("RootTS = %q, want %q", metadata.RootTS, invocation.ThreadTS)
	}
}

func TestDedupeKeys(t *testing.T) {
	i := validInvocation()
	got := i.DedupeKeys()
	if len(got) != 2 || got[0] != "event:Ev123" || got[1] != "message:T12345678:C12345678:1700000000.000001" {
		t.Fatalf("unexpected keys: %#v", got)
	}
	i.EventID = ""
	got = i.DedupeKeys()
	if got[0] != "fallback:T12345678:C12345678:1700000000.000001:app_mention" {
		t.Fatalf("unexpected fallback key: %q", got[0])
	}
}

func TestInvocationValidateWithAttachments(t *testing.T) {
	tests := []struct {
		name string
		inv  Invocation
		ok   bool
	}{
		{
			name: "attachment-only mention",
			inv: Invocation{
				EventID: "Ev123", EventType: "app_mention",
				TeamID: "T12345678", ChannelID: "C12345678", ChannelKind: ChannelPublic,
				UserID: "U12345678", EventTS: "1700000000.000001",
				Attachments: []Attachment{{ID: "F00000001", Name: "file.txt", MIMEType: "text/plain", Size: 100}},
				Trigger:     TriggerMention,
			},
			ok: true,
		},
		{
			name: "text-and-attachment DM",
			inv: Invocation{
				EventType: "message.im", TeamID: "T12345678", ChannelID: "D12345678",
				ChannelKind: ChannelDM, UserID: "U12345678", EventTS: "1700000000.000001",
				Text: "check this file", Trigger: TriggerDirectMessage,
				Attachments: []Attachment{{ID: "F00000001", Name: "file.txt", MIMEType: "text/plain", Size: 100}},
			},
			ok: true,
		},
		{
			name: "empty text and no attachments",
			inv: Invocation{
				EventID: "Ev123", EventType: "app_mention",
				TeamID: "T12345678", ChannelID: "C12345678", ChannelKind: ChannelPublic,
				UserID: "U12345678", EventTS: "1700000000.000001",
				Text: "   ", Trigger: TriggerMention,
			},
			ok: false,
		},
		{
			name: "attachment with missing ID",
			inv: Invocation{
				EventID: "Ev123", EventType: "app_mention",
				TeamID: "T12345678", ChannelID: "C12345678", ChannelKind: ChannelPublic,
				UserID: "U12345678", EventTS: "1700000000.000001",
				Attachments: []Attachment{{Name: "file.txt", MIMEType: "text/plain", Size: 100}},
				Trigger:     TriggerMention,
			},
			ok: false,
		},
		{
			name: "attachment with negative size",
			inv: Invocation{
				EventID: "Ev123", EventType: "app_mention",
				TeamID: "T12345678", ChannelID: "C12345678", ChannelKind: ChannelPublic,
				UserID: "U12345678", EventTS: "1700000000.000001",
				Attachments: []Attachment{{ID: "F00000001", Name: "file.txt", MIMEType: "text/plain", Size: -1}},
				Trigger:     TriggerMention,
			},
			ok: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.inv.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() = %v; want nil", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("Validate() = nil; want error")
			}
		})
	}
}

func TestProcessingID(t *testing.T) {
	i := validInvocation()
	got := i.ProcessingID(0)
	want := "T12345678:C12345678:1700000000.000001:att-0"
	if got != want {
		t.Fatalf("ProcessingID(0) = %q, want %q", got, want)
	}
	got = i.ProcessingID(3)
	want = "T12345678:C12345678:1700000000.000001:att-3"
	if got != want {
		t.Fatalf("ProcessingID(3) = %q, want %q", got, want)
	}
}

func TestAccessPolicy(t *testing.T) {
	i := validInvocation()
	tests := []struct {
		name   string
		policy AccessPolicy
		inv    Invocation
		allow  bool
	}{
		{"listed user", AccessPolicy{AllowedUserIDs: []string{i.UserID}}, i, true},
		{"unknown user", AccessPolicy{AllowedUserIDs: []string{"U99999999"}}, i, false},
		{"all users wrong team", AccessPolicy{AllowAllUsers: true, AllowedTeamIDs: []string{"T99999999"}}, i, false},
		{"channel restricted", AccessPolicy{AllowAllUsers: true, AllowedChannelIDs: []string{"C99999999"}}, i, false},
		{"dm ignores channel list", AccessPolicy{AllowAllUsers: true, AllowedChannelIDs: []string{"C99999999"}}, func() Invocation {
			dm := i
			dm.ChannelKind, dm.ChannelID, dm.Trigger = ChannelDM, "D12345678", TriggerDirectMessage
			return dm
		}(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.Authorize(tt.inv).Allowed; got != tt.allow {
				t.Fatalf("allowed = %v, want %v", got, tt.allow)
			}
		})
	}
}
