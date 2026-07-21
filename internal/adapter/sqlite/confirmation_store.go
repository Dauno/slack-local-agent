package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ConfirmationDeliveryStore = (*ConfirmationStore)(nil)

// ConfirmationStore implements port.ConfirmationDeliveryStore backed by
// the tool_confirmation_deliveries table.
type ConfirmationStore struct {
	db *sql.DB
}

func NewConfirmationStore(store *Store) *ConfirmationStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &ConfirmationStore{db: store.db}
}

func (s *ConfirmationStore) CreateDelivery(ctx context.Context, delivery port.ConfirmationDelivery) error {
	if delivery.WrapperCallID == "" || delivery.OriginalCallID == "" || delivery.SessionID == "" || delivery.Actor == "" || delivery.TeamID == "" || delivery.ChannelID == "" || delivery.Expiry.IsZero() {
		return errors.New("confirmation delivery has required fields missing")
	}
	if delivery.Status == "" {
		delivery.Status = port.ConfirmationPending
	}
	presentation, err := json.Marshal(struct {
		Summary       string `json:"summary"`
		ParameterHash string `json:"parameter_hash"`
	}{Summary: delivery.Summary, ParameterHash: delivery.ParameterHash})
	if err != nil {
		return fmt.Errorf("encode confirmation presentation: %w", err)
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tool_confirmation_deliveries
		 (wrapper_call_id, original_call_id, session_id, actor, slack_team_id,
		  slack_channel_id, slack_thread_ts, presentation, expiry,
		  status, correlation_id, slack_message_ts, renderer_mode, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(wrapper_call_id) DO NOTHING`,
		delivery.WrapperCallID,
		delivery.OriginalCallID,
		delivery.SessionID,
		delivery.Actor,
		delivery.TeamID,
		delivery.ChannelID,
		delivery.ThreadTS,
		string(presentation),
		delivery.Expiry.Unix(),
		string(delivery.Status),
		delivery.CorrelationID,
		delivery.SlackMessageTS,
		delivery.RendererMode,
		now.Unix(),
		now.Unix(),
	)
	return err
}

func (s *ConfirmationStore) MarkPublished(ctx context.Context, wrapperCallID, correlationID, slackMessageTS, rendererMode string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`UPDATE tool_confirmation_deliveries
		 SET status = ?, correlation_id = ?, slack_message_ts = ?, renderer_mode = ?, updated_at = ?
		 WHERE wrapper_call_id = ? AND status = ?`,
		string(port.ConfirmationPublished), correlationID, slackMessageTS, rendererMode, now.Unix(),
		wrapperCallID, string(port.ConfirmationPending),
	)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("delivery %s not found or not pending", wrapperCallID)
	}
	return nil
}

func (s *ConfirmationStore) MarkConsumed(ctx context.Context, wrapperCallID string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`UPDATE tool_confirmation_deliveries
		 SET status = ?, updated_at = ?
		 WHERE wrapper_call_id = ? AND status IN (?, ?)`,
		string(port.ConfirmationConsumed), now.Unix(),
		wrapperCallID, string(port.ConfirmationPublished), string(port.ConfirmationPending),
	)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("delivery %s not consumable", wrapperCallID)
	}
	return nil
}

func (s *ConfirmationStore) RejectDelivery(ctx context.Context, wrapperCallID string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`UPDATE tool_confirmation_deliveries
		 SET status = ?, updated_at = ?
		 WHERE wrapper_call_id = ? AND status IN (?, ?)`,
		string(port.ConfirmationRejected), now.Unix(),
		wrapperCallID, string(port.ConfirmationPending), string(port.ConfirmationPublished),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("delivery %s not rejectable", wrapperCallID)
	}
	return nil
}

func (s *ConfirmationStore) GetByWrapperCallID(ctx context.Context, wrapperCallID string) (*port.ConfirmationDelivery, error) {
	var (
		originalCallID, sessionID, actor, teamID, channelID, threadTS string
		presentationJSON, status, correlationID                       string
		slackMessageTS, rendererMode                                  string
		expiryUnix                                                    int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT original_call_id, session_id, actor, slack_team_id,
		 slack_channel_id, slack_thread_ts, presentation, status, correlation_id,
		 slack_message_ts, renderer_mode, expiry
		 FROM tool_confirmation_deliveries
		 WHERE wrapper_call_id = ?`,
		wrapperCallID,
	).Scan(&originalCallID, &sessionID, &actor, &teamID, &channelID, &threadTS, &presentationJSON, &status, &correlationID, &slackMessageTS, &rendererMode, &expiryUnix)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get confirmation delivery: %w", err)
	}

	key := deliveryConversationKey(teamID, channelID, threadTS)
	var presentation struct {
		Summary       string `json:"summary"`
		ParameterHash string `json:"parameter_hash"`
	}
	if err := json.Unmarshal([]byte(presentationJSON), &presentation); err != nil {
		return nil, fmt.Errorf("decode confirmation presentation: %w", err)
	}

	return &port.ConfirmationDelivery{
		WrapperCallID:   wrapperCallID,
		OriginalCallID:  originalCallID,
		SessionID:       sessionID,
		Actor:           actor,
		TeamID:          teamID,
		ChannelID:       channelID,
		ThreadTS:        threadTS,
		ConversationKey: key,
		Summary:         presentation.Summary,
		ParameterHash:   presentation.ParameterHash,
		Status:          port.ConfirmationDeliveryStatus(status),
		CorrelationID:   correlationID,
		SlackMessageTS:  slackMessageTS,
		RendererMode:    rendererMode,
		Expiry:          time.Unix(expiryUnix, 0),
	}, nil
}

func (s *ConfirmationStore) ListPending(ctx context.Context) ([]port.ConfirmationDelivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT wrapper_call_id, original_call_id, session_id, actor,
		 slack_team_id, slack_channel_id, slack_thread_ts, presentation, status, correlation_id,
		 slack_message_ts, renderer_mode, expiry
		 FROM tool_confirmation_deliveries
		 WHERE (status = ? OR status = ?) AND expiry > ?
		 ORDER BY created_at ASC`,
		string(port.ConfirmationPending), string(port.ConfirmationPublished), time.Now().UTC().Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("list pending confirmations: %w", err)
	}
	defer rows.Close()

	var deliveries []port.ConfirmationDelivery
	for rows.Next() {
		var d port.ConfirmationDelivery
		var presentationJSON, status, correlationID string
		var slackMessageTS, rendererMode string
		var expiryUnix int64
		if err := rows.Scan(&d.WrapperCallID, &d.OriginalCallID, &d.SessionID, &d.Actor,
			&d.TeamID, &d.ChannelID, &d.ThreadTS, &presentationJSON, &status, &correlationID,
			&slackMessageTS, &rendererMode, &expiryUnix,
		); err != nil {
			return nil, fmt.Errorf("scan confirmation row: %w", err)
		}
		d.Status = port.ConfirmationDeliveryStatus(status)
		var presentation struct {
			Summary       string `json:"summary"`
			ParameterHash string `json:"parameter_hash"`
		}
		if err := json.Unmarshal([]byte(presentationJSON), &presentation); err != nil {
			return nil, fmt.Errorf("decode confirmation presentation: %w", err)
		}
		d.Summary = presentation.Summary
		d.ParameterHash = presentation.ParameterHash
		d.CorrelationID = correlationID
		d.SlackMessageTS = slackMessageTS
		d.RendererMode = rendererMode
		d.Expiry = time.Unix(expiryUnix, 0)
		d.ConversationKey = deliveryConversationKey(d.TeamID, d.ChannelID, d.ThreadTS)
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}

func (s *ConfirmationStore) ExpireDeliveries(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_confirmation_deliveries
		 SET status = ?, updated_at = ?
		 WHERE status IN (?, ?) AND expiry <= ?`,
		string(port.ConfirmationExpired),
		now.Unix(),
		string(port.ConfirmationPending), string(port.ConfirmationPublished),
		now.Unix(),
	)
	return err
}

func deliveryConversationKey(teamID, channelID, threadTS string) domain.ConversationKey {
	if threadTS == "" {
		return domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s", teamID, channelID))
	}
	return domain.ConversationKey(fmt.Sprintf("slack:%s:channel:%s:thread:%s", teamID, channelID, threadTS))
}
