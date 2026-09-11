package database

import (
	"context"
	"testing"
)

func TestUpsertPriceFromSync_CreatesWhenAbsent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	created, err := db.UpsertPriceFromSync(ctx, "brand-new-model", 2.20, 11.00, 2.75, 0.22, nil, nil, nil, nil, 1)
	if err != nil {
		t.Fatalf("UpsertPriceFromSync: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a prefix with no existing row")
	}

	p, err := db.GetPriceByPrefix(ctx, "brand-new-model")
	if err != nil {
		t.Fatalf("GetPriceByPrefix: %v", err)
	}
	if p == nil {
		t.Fatal("expected a new row for brand-new-model")
	}
	if p.InputPerM != 2.20 || p.OutputPerM != 11.00 {
		t.Errorf("got (input=%v, output=%v), want (2.20, 11.00)", p.InputPerM, p.OutputPerM)
	}
}

func TestUpsertPriceFromSync_OverwritesBaseRatesOnExistingRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreatePrice(ctx, Price{Prefix: "sync-model", InputPerM: 3.00, OutputPerM: 15.00, CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatalf("CreatePrice: %v", err)
	}

	created, err := db.UpsertPriceFromSync(ctx, "sync-model", 2.20, 11.00, 2.75, 0.22, nil, nil, nil, nil, 2)
	if err != nil {
		t.Fatalf("UpsertPriceFromSync: %v", err)
	}
	if created {
		t.Fatal("expected created=false when a row already exists")
	}

	p, err := db.GetPrice(ctx, id)
	if err != nil || p == nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if p.InputPerM != 2.20 || p.OutputPerM != 11.00 {
		t.Errorf("got (input=%v, output=%v), want (2.20, 11.00)", p.InputPerM, p.OutputPerM)
	}
}

// TestUpsertPriceFromSync_PreservesManualAbove200kOverride verifies that a
// sync which doesn't report an above-200k tier for a model leaves a
// manually configured override in place, rather than clearing it.
func TestUpsertPriceFromSync_PreservesManualAbove200kOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	manualOverride := 20.0
	id, err := db.CreatePrice(ctx, Price{
		Prefix: "manual-tier-model", InputPerM: 3.00, OutputPerM: 15.00,
		InputPerMAbove200k: &manualOverride, CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatalf("CreatePrice: %v", err)
	}

	if _, err := db.UpsertPriceFromSync(ctx, "manual-tier-model", 2.20, 11.00, 2.75, 0.22, nil, nil, nil, nil, 2); err != nil {
		t.Fatalf("UpsertPriceFromSync: %v", err)
	}

	p, err := db.GetPrice(ctx, id)
	if err != nil || p == nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if p.InputPerMAbove200k == nil || *p.InputPerMAbove200k != manualOverride {
		t.Errorf("InputPerMAbove200k = %v, want the preserved manual override %v", p.InputPerMAbove200k, manualOverride)
	}
}

// TestUpsertPriceFromSync_OverwritesAbove200kWhenSyncReportsIt verifies that
// when the sync does supply a non-nil above-200k rate, it overwrites
// whatever was there before.
func TestUpsertPriceFromSync_OverwritesAbove200kWhenSyncReportsIt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	oldOverride := 20.0
	id, err := db.CreatePrice(ctx, Price{
		Prefix: "reported-tier-model", InputPerM: 3.00, OutputPerM: 15.00,
		InputPerMAbove200k: &oldOverride, CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatalf("CreatePrice: %v", err)
	}

	newOverride := 30.0
	if _, err := db.UpsertPriceFromSync(ctx, "reported-tier-model", 2.20, 11.00, 2.75, 0.22, &newOverride, nil, nil, nil, 2); err != nil {
		t.Fatalf("UpsertPriceFromSync: %v", err)
	}

	p, err := db.GetPrice(ctx, id)
	if err != nil || p == nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if p.InputPerMAbove200k == nil || *p.InputPerMAbove200k != newOverride {
		t.Errorf("InputPerMAbove200k = %v, want the newly synced override %v", p.InputPerMAbove200k, newOverride)
	}
}
