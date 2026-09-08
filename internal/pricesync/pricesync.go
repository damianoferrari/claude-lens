// Package pricesync keeps claude-lens's model_prices table in sync with a
// LiteLLM proxy's authoritative per-model rates, both on demand (the admin
// UI's "Sync from LiteLLM" button) and on a background schedule stored in
// the database (see database.Settings — an admin-editable schedule lives
// in the DB, not an env var, so changing it never needs a restart).
package pricesync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lfsc09/claude-lens/internal/database"
	"github.com/lfsc09/claude-lens/internal/litellm"
	"github.com/lfsc09/claude-lens/internal/pricing"
)

// Result reports what a Sync call did, in a shape the admin API can return
// to the client as-is.
type Result struct {
	Created int      `json:"created"`
	Updated int      `json:"updated"`
	Models  []string `json:"models"`
}

// Sync pulls per-model rates from baseURL's LiteLLM /model/info endpoint
// and upserts each model's unconditional ("over 0") price rule into db —
// see database.UpsertPriceFromSync for why tiered rules are left alone —
// then refreshes est so the new rates apply immediately and marks the
// sync's completion time (see database.MarkLiteLLMSynced), so RunLoop's
// staleness check doesn't immediately fire again right after. Returns an
// error, without changing anything, if the fetch itself fails (e.g. baseURL
// isn't a LiteLLM proxy).
func Sync(ctx context.Context, db *database.DB, est *pricing.Estimator, client *litellm.Client, baseURL, authToken string) (Result, error) {
	prices, err := client.FetchModelPrices(ctx, baseURL, authToken)
	if err != nil {
		return Result{}, err
	}

	now := float64(time.Now().Unix())
	result := Result{Models: make([]string, 0, len(prices))}
	for _, p := range prices {
		created, err := db.UpsertPriceFromSync(ctx, p.ModelName, p.InputPerM, p.OutputPerM, p.CacheWritePerM, p.CacheReadPerM, now)
		if err != nil {
			return Result{}, fmt.Errorf("upsert %s: %w", p.ModelName, err)
		}
		if created {
			result.Created++
		} else {
			result.Updated++
		}
		result.Models = append(result.Models, p.ModelName)
	}

	if err := est.Refresh(ctx); err != nil {
		return Result{}, fmt.Errorf("refresh estimator: %w", err)
	}
	if err := db.MarkLiteLLMSynced(ctx, now); err != nil {
		return Result{}, fmt.Errorf("mark synced: %w", err)
	}
	return result, nil
}

// pollInterval is how often RunLoop checks whether a sync is due. It is not
// itself the sync schedule — see database.Settings.LiteLLMSyncIntervalMinutes
// for that — just the granularity at which a change to that setting (or a
// fresh install) takes effect.
const pollInterval = time.Minute

// RunLoop checks db.Settings every pollInterval and fires a Sync whenever
// LiteLLMSyncIntervalMinutes has elapsed since LiteLLMLastSyncedAt —
// including immediately, on a fresh database (LiteLLMLastSyncedAt is 0) or
// one restarted after the interval already lapsed. An interval of 0
// disables the loop entirely, checked fresh on every poll so an admin
// toggling it via the UI takes effect within a minute, no restart needed.
// The admin UI's manual "Sync from LiteLLM" button is wired separately and
// unaffected either way.
//
// Failures are logged, not propagated: this runs unconditionally whether or
// not the configured upstream is actually a LiteLLM proxy, so a direct-
// Anthropic setup fails every due check by design (see
// litellm.FetchModelPrices) and that must never take the process down.
func RunLoop(ctx context.Context, db *database.DB, est *pricing.Estimator, client *litellm.Client, baseURL, authToken string) {
	logger := slog.Default().With("component", "pricesync")

	checkAndSyncIfDue := func() {
		settings, err := db.GetSettings(ctx)
		if err != nil {
			logger.Warn("read settings", "error", err)
			return
		}
		if settings.LiteLLMSyncIntervalMinutes <= 0 {
			return
		}
		due := time.Unix(int64(settings.LiteLLMLastSyncedAt), 0).
			Add(time.Duration(settings.LiteLLMSyncIntervalMinutes) * time.Minute)
		if time.Now().Before(due) {
			return
		}

		result, err := Sync(ctx, db, est, client, baseURL, authToken)
		if err != nil {
			logger.Warn("litellm price sync failed", "error", err)
			return
		}
		logger.Info("litellm price sync complete", "created", result.Created, "updated", result.Updated, "models", len(result.Models))
	}

	checkAndSyncIfDue()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkAndSyncIfDue()
		}
	}
}
