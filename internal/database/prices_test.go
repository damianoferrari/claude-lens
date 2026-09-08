package database

import (
	"context"
	"testing"
)

func TestUpsertPriceFromSync_CreatesWhenAbsent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.UpsertPriceFromSync(ctx, "brand-new-model", 2.20, 11.00, 2.75, 0.22, 1)
	if err != nil {
		t.Fatalf("UpsertPriceFromSync: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a prefix with no existing rule")
	}

	prices, err := db.ListPrices(ctx)
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	found := false
	for _, p := range prices {
		if p.Prefix == "brand-new-model" && p.Rule == "over" && p.RuleTokens == 0 {
			found = true
			if p.InputPerM != 2.20 || p.OutputPerM != 11.00 {
				t.Errorf("got (input=%v, output=%v), want (2.20, 11.00)", p.InputPerM, p.OutputPerM)
			}
		}
	}
	if !found {
		t.Fatal("expected a new over-0 rule for brand-new-model")
	}
}

func TestUpsertPriceFromSync_UpdatesExistingOverZeroRuleOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// A prefix openTestDB doesn't already seed, so this test owns its only
	// over-0 row and isn't racing the seeded defaults for it.
	baseID, err := db.CreatePrice(ctx, Price{Prefix: "tiered-sync-model", Rule: "over", RuleTokens: 0, InputPerM: 3.00, OutputPerM: 15.00, CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatalf("CreatePrice(base): %v", err)
	}
	tieredID, err := db.CreatePrice(ctx, Price{Prefix: "tiered-sync-model", Rule: "under", RuleTokens: 1000, InputPerM: 9.00, OutputPerM: 9.00, CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatalf("CreatePrice(tiered): %v", err)
	}

	created, err := db.UpsertPriceFromSync(ctx, "tiered-sync-model", 2.20, 11.00, 2.75, 0.22, 2)
	if err != nil {
		t.Fatalf("UpsertPriceFromSync: %v", err)
	}
	if created {
		t.Fatal("expected created=false when an over-0 rule already exists")
	}

	base, err := db.GetPrice(ctx, baseID)
	if err != nil || base == nil {
		t.Fatalf("GetPrice(base): %v", err)
	}
	if base.InputPerM != 2.20 || base.OutputPerM != 11.00 {
		t.Errorf("base rule got (input=%v, output=%v), want (2.20, 11.00)", base.InputPerM, base.OutputPerM)
	}

	tiered, err := db.GetPrice(ctx, tieredID)
	if err != nil || tiered == nil {
		t.Fatalf("GetPrice(tiered): %v", err)
	}
	if tiered.InputPerM != 9.00 || tiered.OutputPerM != 9.00 {
		t.Errorf("tiered rule should be untouched by sync, got (input=%v, output=%v)", tiered.InputPerM, tiered.OutputPerM)
	}
}
