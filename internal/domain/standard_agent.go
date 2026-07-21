package domain

import "time"

type ProgressState string

const (
	ProgressWorking             ProgressState = "working"
	ProgressWaitingConfirmation ProgressState = "waiting_confirmation"
	ProgressFinalizing          ProgressState = "finalizing"
	ProgressCleared             ProgressState = "cleared"
	ProgressFailed              ProgressState = "failed"
	ProgressInterrupted         ProgressState = "interrupted"
)

func (s ProgressState) Terminal() bool {
	return s == ProgressCleared || s == ProgressFailed || s == ProgressInterrupted
}

type ProgressOperation struct {
	ID              string
	ConversationKey ConversationKey
	ChannelID       string
	ThreadTS        string
	MessageTS       string
	State           ProgressState
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type IncrementalStatus string

const (
	IncrementalPrepared       IncrementalStatus = "prepared"
	IncrementalMessageCreated IncrementalStatus = "message_created"
	IncrementalUpdating       IncrementalStatus = "updating"
	IncrementalFinalized      IncrementalStatus = "finalized"
	IncrementalInterrupted    IncrementalStatus = "interrupted"
	IncrementalUnknown        IncrementalStatus = "unknown"
)

type IncrementalOperation struct {
	ID              string
	ConversationKey ConversationKey
	ChannelID       string
	ThreadTS        string
	MessageTS       string
	RendererVersion string
	Sequence        int
	PrefixDigest    string
	Status          IncrementalStatus
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
