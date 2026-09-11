// Package pricing estimates the USD cost of a model API call from the
// admin-managed model_prices table.
//
// Prices live in the database (see internal/database) and are managed via
// the admin UI with no restart needed; Estimator keeps an in-memory, sorted
// cache so the hot path (every proxied response) never hits SQLite.
package pricing

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/lfsc09/claude-lens/internal/database"
)

// above200kTokens is the prompt-size threshold past which a Price's
// *Above200k override rates apply instead of its base ones — the
// documented surcharge tier some Claude long-context variants bill at.
const above200kTokens = 200_000

// Estimator matches a model name against the longest matching model_prices
// prefix to find that prefix's price row.
type Estimator struct {
	db *database.DB

	mu                   sync.RWMutex
	prefixesLongestFirst []string
	priceByPrefix        map[string]database.Price
}

// New creates an Estimator backed by db. Call Refresh before first use to
// populate the cache.
func New(db *database.DB) *Estimator {
	return &Estimator{db: db}
}

// Refresh reloads the price cache from the database. Call it after any
// admin CRUD on model_prices (CreatePrice/UpdatePrice/DeletePrice) — the
// table is tiny and prices change rarely, so a synchronous reload is
// simpler than any pub/sub or TTL scheme.
func (e *Estimator) Refresh(ctx context.Context) error {
	prices, err := e.db.ListPrices(ctx)
	if err != nil {
		return err
	}

	priceByPrefix := make(map[string]database.Price, len(prices))
	for _, p := range prices {
		priceByPrefix[p.Prefix] = p
	}

	// Longest prefix first, so the first HasPrefix match found while
	// scanning is always the most specific one — this also naturally
	// prefers an exact match (prefix == model) over any shorter prefix,
	// without needing a separate exact-match pass. Sorting by length
	// descending resolves prefix ambiguity deliberately (e.g.
	// "claude-sonnet-4" and "claude-sonnet" both prefix
	// "claude-sonnet-4-5") instead of leaving it to accident of insertion
	// order.
	prefixes := make([]string, 0, len(priceByPrefix))
	for prefix := range priceByPrefix {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i]) > len(prefixes[j])
	})

	e.mu.Lock()
	e.prefixesLongestFirst = prefixes
	e.priceByPrefix = priceByPrefix
	e.mu.Unlock()
	return nil
}

// Costs is the USD breakdown of a single model call, split by token type —
// plain input/output plus Anthropic's prompt-caching tokens (cache creation
// is priced above input, cache read well below it).
type Costs struct {
	InputCost         float64
	OutputCost        float64
	CacheCreationCost float64
	CacheReadCost     float64
}

// EstimateCosts returns the USD cost breakdown for a call's token counts
// against the given model, or ok == false if no price matches.
func (e *Estimator) EstimateCosts(model string, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int) (Costs, bool) {
	costs, _, ok := e.EstimateCostsAndRule(model, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens)
	return costs, ok
}

// EstimateCostsAndRule returns both the USD cost breakdown and the price
// row that produced it, or ok == false if no price matches.
//
// The prompt size — input + cache-creation + cache-read tokens, not a
// cross-call session sum, since the client resends the full conversation
// every turn and a single request's prompt size already reflects how far
// the session has grown — decides whether each cost category bills at its
// matched price's base rate or its above200kTokens override, when one is
// configured.
func (e *Estimator) EstimateCostsAndRule(model string, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int) (Costs, database.Price, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	price, ok := e.matchPriceLocked(model)
	if !ok {
		return Costs{}, database.Price{}, false
	}

	promptTokens := inputTokens + cacheCreationTokens + cacheReadTokens
	aboveTier := promptTokens > above200kTokens

	return Costs{
		InputCost:         round6(float64(inputTokens) * rateFor(price.InputPerM, price.InputPerMAbove200k, aboveTier) / 1_000_000),
		OutputCost:        round6(float64(outputTokens) * rateFor(price.OutputPerM, price.OutputPerMAbove200k, aboveTier) / 1_000_000),
		CacheCreationCost: round6(float64(cacheCreationTokens) * rateFor(price.CacheWritePerM, price.CacheWritePerMAbove200k, aboveTier) / 1_000_000),
		CacheReadCost:     round6(float64(cacheReadTokens) * rateFor(price.CacheReadPerM, price.CacheReadPerMAbove200k, aboveTier) / 1_000_000),
	}, price, true
}

// rateFor picks a cost category's effective rate: the above-200k override
// when the prompt crossed the threshold and an override is configured,
// otherwise the base rate.
func rateFor(base float64, above200k *float64, aboveTier bool) float64 {
	if aboveTier && above200k != nil {
		return *above200k
	}
	return base
}

// matchPriceLocked finds the price row for the longest prefix of
// prefixesLongestFirst that matches model; callers must hold e.mu.
func (e *Estimator) matchPriceLocked(model string) (database.Price, bool) {
	for _, prefix := range e.prefixesLongestFirst {
		if strings.HasPrefix(model, prefix) {
			return e.priceByPrefix[prefix], true
		}
	}
	return database.Price{}, false
}

func round6(f float64) float64 {
	const mult = 1e6
	return math.Round(f*mult) / mult
}
