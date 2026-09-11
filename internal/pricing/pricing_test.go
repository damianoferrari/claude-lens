package pricing

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lfsc09/claude-lens/internal/database"
)

func openTestDB(t *testing.T) *database.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// clearPrices deletes every seeded/default price so a test can install its
// own exact scenario.
func clearPrices(t *testing.T, db *database.DB) {
	t.Helper()
	for _, p := range mustListPrices(t, db) {
		if err := db.DeletePrice(context.Background(), p.ID); err != nil {
			t.Fatalf("DeletePrice(%d): %v", p.ID, err)
		}
	}
}

func createPrice(t *testing.T, db *database.DB, p database.Price) int64 {
	t.Helper()
	id, err := db.CreatePrice(context.Background(), p)
	if err != nil {
		t.Fatalf("CreatePrice(%s): %v", p.Prefix, err)
	}
	return id
}

func floatPtr(f float64) *float64 { return &f }

func TestEstimateCosts_UsesDefaultSeededPrices(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	costs, ok := e.EstimateCosts("claude-sonnet-5-20260101", 1_000_000, 1_000_000, 0, 0)
	if !ok {
		t.Fatal("expected a match against the seeded claude-sonnet-5 price")
	}
	if costs.InputCost != 3.00 || costs.OutputCost != 15.00 {
		t.Errorf("got (%v, %v), want (3.00, 15.00)", costs.InputCost, costs.OutputCost)
	}
}

func TestEstimateCosts_ComputesCacheCosts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// claude-sonnet-5 is seeded with CacheWritePerM=3.75, CacheReadPerM=0.30.
	costs, ok := e.EstimateCosts("claude-sonnet-5-20260101", 0, 0, 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("expected a match against the seeded claude-sonnet-5 price")
	}
	if costs.CacheCreationCost != 3.75 || costs.CacheReadCost != 0.30 {
		t.Errorf("got (cacheCreation=%v, cacheRead=%v), want (3.75, 0.30)", costs.CacheCreationCost, costs.CacheReadCost)
	}
}

func TestEstimateCosts_ZeroCacheRateDoesNotBreakInputOutput(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clearPrices(t, db)
	// A price row with no cache rates set (e.g. one created before this
	// feature, or never edited) must still price input/output normally and
	// simply report 0 cache cost, not fail the match.
	createPrice(t, db, database.Price{Prefix: "no-cache-rate-model", InputPerM: 2.00, OutputPerM: 10.00, CreatedAt: 1, UpdatedAt: 1})

	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	costs, ok := e.EstimateCosts("no-cache-rate-model", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if !ok {
		t.Fatal("expected a match")
	}
	if costs.InputCost != 2.00 || costs.OutputCost != 10.00 {
		t.Errorf("got (input=%v, output=%v), want (2.00, 10.00)", costs.InputCost, costs.OutputCost)
	}
	if costs.CacheCreationCost != 0 || costs.CacheReadCost != 0 {
		t.Errorf("got (cacheCreation=%v, cacheRead=%v), want (0, 0) for an unpriced cache rate", costs.CacheCreationCost, costs.CacheReadCost)
	}
}

func TestEstimateCosts_UnknownModel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	_, ok := e.EstimateCosts("some-unrelated-model", 100, 100, 0, 0)
	if ok {
		t.Fatal("expected no match for an unrelated model name")
	}
}

func TestEstimateCosts_LongestPrefixWins(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// "claude-sonnet-4" and "claude-sonnet" both prefix
	// "claude-sonnet-4-5" — the more specific (longer) prefix must win, not
	// whichever happens to be inserted or scanned first.
	clearPrices(t, db)
	// Insert in an order where the shorter prefix comes last, to prove the
	// result doesn't depend on insertion/scan order.
	createPrice(t, db, database.Price{Prefix: "claude-sonnet", InputPerM: 1.00, OutputPerM: 1.00, CreatedAt: 1, UpdatedAt: 1})
	createPrice(t, db, database.Price{Prefix: "claude-sonnet-4", InputPerM: 3.00, OutputPerM: 15.00, CreatedAt: 2, UpdatedAt: 2})

	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	costs, ok := e.EstimateCosts("claude-sonnet-4-5", 1_000_000, 1_000_000, 0, 0)
	if !ok {
		t.Fatal("expected a match")
	}
	if costs.InputCost != 3.00 || costs.OutputCost != 15.00 {
		t.Errorf("got (%v, %v), want the claude-sonnet-4 price (3.00, 15.00), not the shorter claude-sonnet prefix", costs.InputCost, costs.OutputCost)
	}
}

func TestEstimateCosts_ExactMatchWinsOverShorterPrefix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clearPrices(t, db)
	createPrice(t, db, database.Price{Prefix: "claude", InputPerM: 1.00, OutputPerM: 1.00, CreatedAt: 1, UpdatedAt: 1})
	createPrice(t, db, database.Price{Prefix: "claude-sonnet-5", InputPerM: 3.00, OutputPerM: 15.00, CreatedAt: 2, UpdatedAt: 2})

	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	costs, ok := e.EstimateCosts("claude-sonnet-5", 1_000_000, 0, 0, 0)
	if !ok || costs.InputCost != 3.00 {
		t.Errorf("got ok=%v inputCost=%v, want exact match at 3.00", ok, costs.InputCost)
	}
}

func TestEstimateCostsAndRule_ReturnsTheMatchedPrice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clearPrices(t, db)
	createPrice(t, db, database.Price{Prefix: "tiered-model", InputPerM: 1.00, OutputPerM: 1.00, CreatedAt: 1, UpdatedAt: 1})

	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	costs, price, ok := e.EstimateCostsAndRule("tiered-model", 201, 0, 0, 0)
	if !ok || costs.InputCost != 0.000201 {
		t.Errorf("got ok=%v inputCost=%v, want 0.000201", ok, costs.InputCost)
	}
	if price.Prefix != "tiered-model" {
		t.Errorf("price = %+v, want the tiered-model price", price)
	}

	if _, _, ok := e.EstimateCostsAndRule("unknown-model", 100, 0, 0, 0); ok {
		t.Fatal("expected no match for an unknown model")
	}
}

func TestEstimateCosts_Above200kUsesOverrideRate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clearPrices(t, db)
	createPrice(t, db, database.Price{
		Prefix: "long-context-model", InputPerM: 1.00, OutputPerM: 2.00, CacheWritePerM: 3.00, CacheReadPerM: 4.00,
		InputPerMAbove200k: floatPtr(10.00), OutputPerMAbove200k: floatPtr(20.00),
		CacheWritePerMAbove200k: floatPtr(30.00), CacheReadPerMAbove200k: floatPtr(40.00),
		CreatedAt: 1, UpdatedAt: 1,
	})

	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// prompt = 200_000 exactly: not yet above the threshold, base rates apply.
	costs, ok := e.EstimateCosts("long-context-model", 200_000, 1_000_000, 0, 0)
	if !ok || costs.InputCost != 0.20 {
		t.Errorf("prompt=200_000: got ok=%v inputCost=%v, want base rate 0.20", ok, costs.InputCost)
	}

	// prompt = 200_001: crosses the threshold, override rates apply to every
	// cost category, using the call's actual token counts for each category.
	costs, ok = e.EstimateCosts("long-context-model", 200_001, 1_000_000, 0, 0)
	if !ok {
		t.Fatal("expected a match")
	}
	wantInput := 10.00 * 200_001 / 1_000_000
	if round6(costs.InputCost) != round6(wantInput) {
		t.Errorf("prompt=200_001 InputCost = %v, want override rate ~%v", costs.InputCost, wantInput)
	}
	if costs.OutputCost != 20.00 {
		t.Errorf("prompt=200_001 OutputCost = %v, want override rate 20.00", costs.OutputCost)
	}
}

func TestEstimateCosts_Above200kFallsBackToBaseWhenNoOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clearPrices(t, db)
	// No above-200k override configured — a large prompt must still price at
	// the base rate rather than failing the match or zeroing out.
	createPrice(t, db, database.Price{Prefix: "no-override-model", InputPerM: 1.00, OutputPerM: 2.00, CreatedAt: 1, UpdatedAt: 1})

	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	costs, ok := e.EstimateCosts("no-override-model", 1_000_000, 1_000_000, 0, 0)
	if !ok || costs.InputCost != 1.00 || costs.OutputCost != 2.00 {
		t.Errorf("got ok=%v (input=%v, output=%v), want base rates (1.00, 2.00)", ok, costs.InputCost, costs.OutputCost)
	}
}

func TestEstimateCosts_PromptSizeIncludesCacheTokens(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clearPrices(t, db)
	createPrice(t, db, database.Price{
		Prefix: "cache-heavy-model", InputPerM: 1.00, OutputPerM: 1.00,
		InputPerMAbove200k: floatPtr(9.00), CreatedAt: 1, UpdatedAt: 1,
	})

	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// 50k input + 100k cache-creation + 60k cache-read = 210k prompt tokens,
	// above the threshold even though input tokens alone aren't.
	costs, ok := e.EstimateCosts("cache-heavy-model", 50_000, 0, 100_000, 60_000)
	if !ok {
		t.Fatal("expected a match")
	}
	wantInput := 9.00 * 50_000 / 1_000_000
	if round6(costs.InputCost) != round6(wantInput) {
		t.Errorf("InputCost = %v, want override rate ~%v (prompt size crosses 200k via cache tokens)", costs.InputCost, wantInput)
	}
}

func TestRefresh_PicksUpChangesWithoutRestart(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	e := New(db)
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if _, ok := e.EstimateCosts("brand-new-model", 100, 100, 0, 0); ok {
		t.Fatal("expected no match before the price exists")
	}

	createPrice(t, db, database.Price{Prefix: "brand-new-model", InputPerM: 9, OutputPerM: 9, CreatedAt: 1, UpdatedAt: 1})
	if err := e.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	costs, ok := e.EstimateCosts("brand-new-model", 1_000_000, 0, 0, 0)
	if !ok || costs.InputCost != 9.00 {
		t.Errorf("got ok=%v inputCost=%v, want ok=true inputCost=9.00 after Refresh", ok, costs.InputCost)
	}
}

func mustListPrices(t *testing.T, db *database.DB) []database.Price {
	t.Helper()
	prices, err := db.ListPrices(context.Background())
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	return prices
}
