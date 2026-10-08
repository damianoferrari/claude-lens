package database

import (
	"context"
	"time"
)

// defaultLiteLLMSyncIntervalMinutes is once an hour — model prices change
// rarely (see internal/pricing), so this just needs to catch drift, not
// react to it quickly.
const defaultLiteLLMSyncIntervalMinutes = 60

// ProviderLiteLLM and ProviderBifrost are the valid values of
// Settings.PriceSyncProvider — the only two upstream shapes
// internal/pricesync knows how to fetch from.
const (
	ProviderLiteLLM = "litellm"
	ProviderBifrost = "bifrost"
)

// defaultPriceSyncProvider preserves pre-Bifrost behavior for both fresh
// installs and ones migrating up: price sync keeps assuming a LiteLLM
// upstream until an admin picks otherwise.
const defaultPriceSyncProvider = ProviderLiteLLM

// Settings is the single global row of admin-configurable settings that
// don't belong to any one entity (unlike, say, a Limiter's own Slack
// webhook) — currently just the price sync provider and schedule.
type Settings struct {
	// LiteLLMSyncIntervalMinutes is how often internal/pricesync.RunLoop
	// re-syncs model_prices from the upstream configured by
	// PriceSyncProvider. 0 disables the background loop; the admin UI's
	// manual sync button is unaffected either way.
	LiteLLMSyncIntervalMinutes int `json:"litellm_sync_interval_minutes"`
	// LiteLLMLastSyncedAt is when a price sync (manual or background) last
	// completed successfully. Read-only from the admin API's perspective —
	// see MarkLiteLLMSynced.
	LiteLLMLastSyncedAt float64 `json:"litellm_last_synced_at"`
	// LiteLLMLastSyncError is the error message from the most recent sync
	// attempt (manual or background), or "" if that attempt succeeded (or
	// none has ever run). Since every attempt overwrites it, a non-empty
	// value always reflects current reachability, not a stale one-time
	// failure — the admin UI uses it to grey out the manual sync button
	// (see MarkLiteLLMSyncFailed) without ever needing a separate
	// capability probe. Cleared when PriceSyncProvider changes (see
	// UpdatePriceSyncProvider), since a failure recorded against the
	// previous provider says nothing about the newly selected one.
	LiteLLMLastSyncError string `json:"litellm_last_sync_error"`
	// LiteLLMLastAttemptAt is when a price sync (manual or background) was
	// last attempted, successful or not — see MarkLiteLLMSynced and
	// MarkLiteLLMSyncFailed. RunLoop uses this instead of
	// LiteLLMLastSyncedAt to decide whether a sync is due, so a run of
	// failures still waits a full interval between retries rather than
	// firing on every poll.
	LiteLLMLastAttemptAt float64 `json:"litellm_last_attempt_at"`
	// PriceSyncProvider selects which upstream API internal/pricesync
	// fetches model prices from: ProviderLiteLLM or ProviderBifrost. Only
	// one is ever active at a time.
	PriceSyncProvider string  `json:"price_sync_provider"`
	UpdatedAt         float64 `json:"updated_at"`
}

// seedDefaultSettings inserts the single settings row on a fresh database.
// A no-op on any database that already has one (including one migrated up
// from before this table existed — see the ON CONFLICT clause).
func (db *DB) seedDefaultSettings(ctx context.Context) error {
	now := float64(time.Now().Unix())
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO settings (id, litellm_sync_interval_minutes, litellm_last_synced_at, litellm_last_sync_error, litellm_last_attempt_at, price_sync_provider, updated_at)
		 VALUES (1, ?, 0, '', 0, ?, ?)
		 ON CONFLICT (id) DO NOTHING`,
		defaultLiteLLMSyncIntervalMinutes, defaultPriceSyncProvider, now,
	)
	return err
}

// GetSettings returns the single settings row.
func (db *DB) GetSettings(ctx context.Context) (Settings, error) {
	var s Settings
	err := db.sql.QueryRowContext(ctx,
		`SELECT litellm_sync_interval_minutes, litellm_last_synced_at, litellm_last_sync_error, litellm_last_attempt_at, price_sync_provider, updated_at FROM settings WHERE id = 1`,
	).Scan(&s.LiteLLMSyncIntervalMinutes, &s.LiteLLMLastSyncedAt, &s.LiteLLMLastSyncError, &s.LiteLLMLastAttemptAt, &s.PriceSyncProvider, &s.UpdatedAt)
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

// UpdatePriceSyncProvider switches which upstream price sync fetches from.
// Also clears LiteLLMLastSyncError: that field reflects the previously
// active provider's reachability, which says nothing about the newly
// selected one, so leaving it set would wrongly grey out the admin UI's
// sync button right after a switch.
func (db *DB) UpdatePriceSyncProvider(ctx context.Context, provider string, updatedAt float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE settings SET price_sync_provider = ?, litellm_last_sync_error = '', updated_at = ? WHERE id = 1`,
		provider, updatedAt,
	)
	return err
}

// MarkLiteLLMSynced records that a price sync just completed successfully,
// whether triggered by the admin UI's button or by RunLoop's own schedule.
// Resets RunLoop's staleness clock (so a manual sync doesn't get
// immediately followed by a redundant scheduled one) and clears any
// previously recorded failure, since a success means the upstream is
// reachable right now regardless of past attempts.
func (db *DB) MarkLiteLLMSynced(ctx context.Context, at float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE settings SET litellm_last_synced_at = ?, litellm_last_sync_error = '', litellm_last_attempt_at = ? WHERE id = 1`, at, at,
	)
	return err
}

// MarkLiteLLMSyncFailed records that a price sync attempt failed — e.g. the
// upstream isn't actually the configured provider's gateway, or the
// configured token can't call its pricing route. Does not touch
// LiteLLMLastSyncedAt: that field means "last successful sync", not "last
// attempt". Updates LiteLLMLastAttemptAt so RunLoop still waits a full
// interval before retrying, instead of firing on every poll while
// LiteLLMLastSyncedAt stays stuck at its last (or never) success.
func (db *DB) MarkLiteLLMSyncFailed(ctx context.Context, errMsg string, at float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE settings SET litellm_last_sync_error = ?, litellm_last_attempt_at = ? WHERE id = 1`, errMsg, at,
	)
	return err
}
