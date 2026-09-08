package database

import (
	"context"
	"time"
)

// defaultLiteLLMSyncIntervalMinutes is once an hour — model prices change
// rarely (see internal/pricing), so this just needs to catch drift, not
// react to it quickly.
const defaultLiteLLMSyncIntervalMinutes = 60

// Settings is the single global row of admin-configurable settings that
// don't belong to any one entity (unlike, say, a Limiter's own Slack
// webhook) — currently just the LiteLLM price sync schedule.
type Settings struct {
	// LiteLLMSyncIntervalMinutes is how often internal/pricesync.RunLoop
	// re-syncs model_prices from the upstream's LiteLLM /model/info
	// endpoint. 0 disables the background loop; the admin UI's manual
	// "Sync from LiteLLM" button is unaffected either way.
	LiteLLMSyncIntervalMinutes int `json:"litellm_sync_interval_minutes"`
	// LiteLLMLastSyncedAt is when a LiteLLM price sync (manual or
	// background) last completed successfully. Read-only from the admin
	// API's perspective — see MarkLiteLLMSynced.
	LiteLLMLastSyncedAt float64 `json:"litellm_last_synced_at"`
	// LiteLLMLastSyncError is the error message from the most recent sync
	// attempt (manual or background), or "" if that attempt succeeded (or
	// none has ever run). Since every attempt overwrites it, a non-empty
	// value always reflects current reachability, not a stale one-time
	// failure — the admin UI uses it to grey out the manual sync button
	// (see MarkLiteLLMSyncFailed) without ever needing a separate "is this
	// even a LiteLLM proxy" probe.
	LiteLLMLastSyncError string  `json:"litellm_last_sync_error"`
	UpdatedAt            float64 `json:"updated_at"`
}

// seedDefaultSettings inserts the single settings row on a fresh database.
// A no-op on any database that already has one (including one migrated up
// from before this table existed — see the ON CONFLICT clause).
func (db *DB) seedDefaultSettings(ctx context.Context) error {
	now := float64(time.Now().Unix())
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO settings (id, litellm_sync_interval_minutes, litellm_last_synced_at, litellm_last_sync_error, updated_at)
		 VALUES (1, ?, 0, '', ?)
		 ON CONFLICT (id) DO NOTHING`,
		defaultLiteLLMSyncIntervalMinutes, now,
	)
	return err
}

// GetSettings returns the single settings row.
func (db *DB) GetSettings(ctx context.Context) (Settings, error) {
	var s Settings
	err := db.sql.QueryRowContext(ctx,
		`SELECT litellm_sync_interval_minutes, litellm_last_synced_at, litellm_last_sync_error, updated_at FROM settings WHERE id = 1`,
	).Scan(&s.LiteLLMSyncIntervalMinutes, &s.LiteLLMLastSyncedAt, &s.LiteLLMLastSyncError, &s.UpdatedAt)
	return s, err
}

// UpdateLiteLLMSyncInterval sets how often (in minutes) RunLoop should
// sync; 0 disables it. Takes effect on RunLoop's next poll (at most a
// minute later), no restart needed. Deliberately never gated by
// LiteLLMLastSyncError: even when the upstream is currently unreachable,
// this stays editable so an admin always has a way to trigger a fresh
// attempt (set a short interval) without waiting on a UI element that a
// past failure disabled.
func (db *DB) UpdateLiteLLMSyncInterval(ctx context.Context, minutes int, updatedAt float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE settings SET litellm_sync_interval_minutes = ?, updated_at = ? WHERE id = 1`,
		minutes, updatedAt,
	)
	return err
}

// MarkLiteLLMSynced records that a LiteLLM price sync just completed
// successfully, whether triggered by the admin UI's button or by RunLoop's
// own schedule. Resets RunLoop's staleness clock (so a manual sync doesn't
// get immediately followed by a redundant scheduled one) and clears any
// previously recorded failure, since a success means the upstream is
// reachable right now regardless of past attempts.
func (db *DB) MarkLiteLLMSynced(ctx context.Context, at float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE settings SET litellm_last_synced_at = ?, litellm_last_sync_error = '' WHERE id = 1`, at,
	)
	return err
}

// MarkLiteLLMSyncFailed records that a LiteLLM price sync attempt failed —
// e.g. the upstream isn't actually a LiteLLM proxy, or the configured token
// can't call its /model/info route. Does not touch LiteLLMLastSyncedAt:
// that field means "last successful sync", not "last attempt".
func (db *DB) MarkLiteLLMSyncFailed(ctx context.Context, errMsg string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE settings SET litellm_last_sync_error = ? WHERE id = 1`, errMsg)
	return err
}
