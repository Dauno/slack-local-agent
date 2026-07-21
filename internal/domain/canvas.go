package domain

import "time"

type CanvasOperationStatus string

const (
	CanvasOpReady     CanvasOperationStatus = "ready"
	CanvasOpCreating  CanvasOperationStatus = "creating"
	CanvasOpCompleted CanvasOperationStatus = "completed"
	CanvasOpFailed    CanvasOperationStatus = "failed"
	CanvasOpUnknown   CanvasOperationStatus = "unknown"
)

type CanvasOperation struct {
	ID             string
	ConversationKey ConversationKey
	Actor          string
	Title          string
	ContentSHA256  string
	Status         CanvasOperationStatus
	CanvasID       string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const (
	MaxCanvasTitleRunes   = 150
	MaxCanvasContentRunes = 50000
	MaxCanvasContentBytes = 5 * 1024 * 1024
)
