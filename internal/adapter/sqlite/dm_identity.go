package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const dmIdentityModeStateKey = "dm_identity_mode"

// EnsureDMIdentityMode stamps DM identity before session access. Existing
// channel-wide DM state cannot be silently re-keyed into independent threads.
func (s *Store) EnsureDMIdentityMode(ctx context.Context, threaded bool) error {
	mode := "channel"
	if threaded {
		mode = "threaded"
	}

	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT state_value FROM runtime_state WHERE state_key = ?`, dmIdentityModeStateKey).Scan(&stored)
	if err == nil {
		if stored != mode {
			return fmt.Errorf("%w: stored %q, configured %q", ErrDMIdentityModeMismatch, stored, mode)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read durable DM identity mode: %w", err)
	}
	if threaded {
		var hasLegacyDM bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE channel_kind = 'dm')`).Scan(&hasLegacyDM); err != nil {
			return fmt.Errorf("inspect legacy DM conversations: %w", err)
		}
		if hasLegacyDM {
			return fmt.Errorf("%w: existing DM conversations require backup and init --reset-state", ErrDMIdentityModeMismatch)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, ?)`, dmIdentityModeStateKey, mode, time.Now().UTC().UnixNano()); err != nil {
		return fmt.Errorf("stamp durable DM identity mode: %w", err)
	}
	return nil
}
