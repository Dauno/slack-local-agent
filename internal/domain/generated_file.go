package domain

import "time"

type GeneratedFileFormat string

const (
	GeneratedFileText     GeneratedFileFormat = "text"
	GeneratedFileMarkdown GeneratedFileFormat = "markdown"
	GeneratedFileCSV      GeneratedFileFormat = "csv"
	GeneratedFileJSON     GeneratedFileFormat = "json"
)

type GeneratedFileOperationStatus string

const (
	GeneratedFileOpPendingConfirmation GeneratedFileOperationStatus = "pending_confirmation"
	GeneratedFileOpURLRequested        GeneratedFileOperationStatus = "url_requested"
	GeneratedFileOpBytesUploaded       GeneratedFileOperationStatus = "bytes_uploaded"
	GeneratedFileOpCompleted           GeneratedFileOperationStatus = "completed"
	GeneratedFileOpFailed              GeneratedFileOperationStatus = "failed"
	GeneratedFileOpUnknown             GeneratedFileOperationStatus = "unknown"
)

type GeneratedFileOperation struct {
	ID              string
	ConversationKey ConversationKey
	Actor           string
	Filename        string
	MediaType       string
	ContentSHA256   string
	SizeBytes       int
	Status          GeneratedFileOperationStatus
	SlackFileID     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const (
	MaxGeneratedFilenameRunes = 128
	MaxGeneratedFileBytes     = 1024 * 1024
)
