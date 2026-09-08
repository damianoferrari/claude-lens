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
	UpdatedAt           float64 `json:"updated_at"`
}

// seedDefaultSettings inserts the single settings row on a fresh database.
// A no-op on any database that already has one (including one migrated up
// from before this table existed — see the ON CONFLICT clause).
func (db *DB) seedDefaultSettings(ctx context.Context) error {
	now := float64(time.Now().Unix())
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO settings (id, litellm_sync_interval_minutes, litellm_last_synced_at, updated_at)
		 VALUES (1, ?, 0, ?)
		 ON CONFLICT (id) DO NOTHING`,
		defaultLiteLLMSyncIntervalMinutes, now,
	)
	return err
}

// GetSettings returns the single settings row.
func (db *DB) GetSettings(ctx context.Context) (Settings, error) {
	var s Settings
	err := db.sql.QueryRowContext(ctx,
		`SELECT litellm_sync_interval_minutes, litellm_last_synced_at, updated_at FROM settings WHERE id = 1`,
	).Scan(&s.LiteLLMSyncIntervalMinutes, &s.LiteLLMLastSyncedAt, &s.UpdatedAt)
	return s, err
}

// UpdateLiteLLMSyncInterval sets how often (in minutes) RunLoop should
// sync; 0 disables it. Takes effect on RunLoop's next poll (at most a
// minute later), no restart needed.
func (db *DB) UpdateLiteLLMSyncInterval(ctx context.Context, minutes int, updatedAt float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE settings SET litellm_sync_interval_minutes = ?, updated_at = ? WHERE id = 1`,
		minutes, updatedAt,
	)
	return err
}

// MarkLiteLLMSynced records that a LiteLLM price sync just completed,
// whether triggered by the admin UI's button or by RunLoop's own schedule
// — either way it resets RunLoop's staleness clock, so a manual sync
// doesn't get immediately followed by a redundant scheduled one.
func (db *DB) MarkLiteLLMSynced(ctx context.Context, at float64) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE settings SET litellm_last_synced_at = ? WHERE id = 1`, at)
	return err
}
