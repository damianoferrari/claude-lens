// Package pricesync keeps claude-lens's model_prices table in sync with a
// LiteLLM proxy's authoritative per-model rates, both on demand (the admin
// UI's "Sync from LiteLLM" button) and on a background interval.
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
// then refreshes est so the new rates apply immediately. Returns an error,
// without changing anything, if the fetch itself fails (e.g. baseURL isn't
// a LiteLLM proxy).
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
	return result, nil
}

// RunLoop calls Sync once immediately, then again every interval until ctx
// is done — the immediate call means a freshly started process doesn't
// wait a full interval before prices are current. A non-positive interval
// is a no-op — the caller (main) still wires the admin UI's manual sync
// button regardless, this only controls the background schedule.
//
// Failures are logged, not propagated: this runs unconditionally whether or
// not the configured upstream is actually a LiteLLM proxy, so a direct-
// Anthropic setup fails every call by design (see litellm.FetchModelPrices)
// and that must never take the process down.
func RunLoop(ctx context.Context, db *database.DB, est *pricing.Estimator, client *litellm.Client, baseURL, authToken string, interval time.Duration) {
	if interval <= 0 {
		return
	}
	logger := slog.Default().With("component", "pricesync")

	runOnce := func() {
		result, err := Sync(ctx, db, est, client, baseURL, authToken)
		if err != nil {
			logger.Warn("litellm price sync failed", "error", err)
			return
		}
		logger.Info("litellm price sync complete", "created", result.Created, "updated", result.Updated, "models", len(result.Models))
	}

	runOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
