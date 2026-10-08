// Package pricesync keeps claude-lens's model_prices table in sync with
// the configured upstream gateway's authoritative per-model rates, both on
// demand (the admin UI's manual sync button) and on a background schedule
// stored in the database (see database.Settings — an admin-editable
// schedule and provider choice live in the DB, not an env var, so changing
// either never needs a restart).
package pricesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lfsc09/claude-lens/internal/database"
	"github.com/lfsc09/claude-lens/internal/priceprovider"
	"github.com/lfsc09/claude-lens/internal/pricing"
)

// Result reports what a Sync call did, in a shape the admin API can return
// to the client as-is.
type Result struct {
	Created int      `json:"created"`
	Updated int      `json:"updated"`
	Models  []string `json:"models"`
}

// ErrUpstreamUnavailable wraps a Sync failure that specifically means "this
// upstream doesn't support price sync at all" (the fetch itself failed) —
// as opposed to some other failure further down Sync's pipeline (e.g. a DB
// write error), which says nothing about whether the upstream is reachable.
// Callers use errors.Is against this to decide whether the failure is a
// genuine capability signal: the admin API maps it to 502 (see
// admin.syncPrices), which the Prices page uses to grey out its manual
// sync button.
var ErrUpstreamUnavailable = errors.New("price sync upstream unavailable")

// Providers holds one Fetcher per upstream gateway shape claude-lens knows
// how to sync prices from. Only one is ever active at a time — see Resolve.
type Providers struct {
	LiteLLM priceprovider.Fetcher
	Bifrost priceprovider.Fetcher
}

// Resolve returns the Fetcher for kind — a database.Settings.PriceSyncProvider
// value. An empty kind resolves to LiteLLM, matching the pre-provider-choice
// default so an existing database without that column migrated up yet
// still syncs the way it always did.
func (p Providers) Resolve(kind string) (priceprovider.Fetcher, error) {
	switch kind {
	case "", database.ProviderLiteLLM:
		return p.LiteLLM, nil
	case database.ProviderBifrost:
		return p.Bifrost, nil
	default:
		return nil, fmt.Errorf("unknown price sync provider %q", kind)
	}
}

// Sync pulls per-model rates from baseURL via client and upserts each
// model's price row into db — see database.UpsertPriceFromSync for why a
// manually configured above-200k override survives a sync that doesn't
// report that tier — then refreshes est so the new rates apply immediately
// and marks the sync's completion time (see database.MarkLiteLLMSynced), so
// RunLoop's staleness check doesn't immediately fire again right after.
// Returns an error, without changing anything, if the fetch itself fails
// (e.g. baseURL isn't an upstream of the kind client expects) — wrapped in
// ErrUpstreamUnavailable — and records that failure via
// database.MarkLiteLLMSyncFailed regardless of whether this call came from
// the admin UI's button or RunLoop's own schedule, so the admin UI can grey
// out the button the moment *any* attempt establishes the upstream doesn't
// support this at all, not just a scheduled one.
func Sync(ctx context.Context, db *database.DB, est *pricing.Estimator, client priceprovider.Fetcher, baseURL, authToken string) (Result, error) {
	now := float64(time.Now().Unix())

	prices, fetchErr := client.FetchModelPrices(ctx, baseURL, authToken)
	if fetchErr != nil {
		_ = db.MarkLiteLLMSyncFailed(ctx, fetchErr.Error(), now)
		return Result{}, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, fetchErr)
	}

	result := Result{Models: make([]string, 0, len(prices))}
	for _, p := range prices {
		created, err := db.UpsertPriceFromSync(ctx, p.ModelName, p.InputPerM, p.OutputPerM, p.CacheWritePerM, p.CacheReadPerM,
			p.InputPerMAbove200k, p.OutputPerMAbove200k, p.CacheWritePerMAbove200k, p.CacheReadPerMAbove200k, now)
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

// syncDue reports whether settings.LiteLLMSyncIntervalMinutes has elapsed
// since the later of LiteLLMLastSyncedAt and LiteLLMLastAttemptAt, as of
// now. Always false when the interval is 0 (auto-sync disabled).
func syncDue(settings database.Settings, now time.Time) bool {
	if settings.LiteLLMSyncIntervalMinutes <= 0 {
		return false
	}
	lastAttempt := max(settings.LiteLLMLastSyncedAt, settings.LiteLLMLastAttemptAt)
	due := time.Unix(int64(lastAttempt), 0).
		Add(time.Duration(settings.LiteLLMSyncIntervalMinutes) * time.Minute)
	return !now.Before(due)
}

// RunLoop checks db.Settings every pollInterval and fires a Sync whenever
// LiteLLMSyncIntervalMinutes has elapsed since the later of
// LiteLLMLastSyncedAt and LiteLLMLastAttemptAt — including immediately, on
// a fresh database (both are 0) or one restarted after the interval already
// lapsed. Using the last attempt rather than just the last success means a
// run of failures still retries once per interval, not on every poll. An
// interval of 0 disables the loop entirely, checked fresh on every poll so
// an admin toggling it via the UI takes effect within a minute, no restart
// needed. The admin UI's manual sync button is wired separately and
// unaffected either way. Which provider syncs from is re-read from
// settings on every due check too, so switching it in the admin UI also
// takes effect without a restart.
//
// Failures are logged, not propagated: this runs unconditionally whether or
// not the configured upstream is actually reachable as the configured
// provider's gateway, so a mismatched setup fails every due check by design
// and that must never take the process down.
func RunLoop(ctx context.Context, db *database.DB, est *pricing.Estimator, providers Providers, baseURL, authToken string) {
	logger := slog.Default().With("component", "pricesync")

	checkAndSyncIfDue := func() {
		settings, err := db.GetSettings(ctx)
		if err != nil {
			logger.Warn("read settings", "error", err)
			return
		}
		if !syncDue(settings, time.Now()) {
			return
		}

		client, err := providers.Resolve(settings.PriceSyncProvider)
		if err != nil {
			logger.Warn("resolve price sync provider", "error", err)
			return
		}

		result, err := Sync(ctx, db, est, client, baseURL, authToken)
		if err != nil {
			logger.Warn("price sync failed", "provider", settings.PriceSyncProvider, "error", err)
			return
		}
		logger.Info("price sync complete", "provider", settings.PriceSyncProvider, "created", result.Created, "updated", result.Updated, "models", len(result.Models))
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
