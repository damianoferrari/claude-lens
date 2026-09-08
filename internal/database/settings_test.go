package database

import (
	"context"
	"testing"
)

func TestGetSettings_SeededOnFreshDB(t *testing.T) {
	db := openTestDB(t)
	s, err := db.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s.LiteLLMSyncIntervalMinutes != defaultLiteLLMSyncIntervalMinutes {
		t.Errorf("LiteLLMSyncIntervalMinutes = %d, want default %d", s.LiteLLMSyncIntervalMinutes, defaultLiteLLMSyncIntervalMinutes)
	}
	if s.LiteLLMLastSyncedAt != 0 {
		t.Errorf("LiteLLMLastSyncedAt = %v, want 0 (never synced)", s.LiteLLMLastSyncedAt)
	}
}

func TestUpdateLiteLLMSyncInterval(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.UpdateLiteLLMSyncInterval(ctx, 60, 100); err != nil {
		t.Fatalf("UpdateLiteLLMSyncInterval: %v", err)
	}
	s, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s.LiteLLMSyncIntervalMinutes != 60 || s.UpdatedAt != 100 {
		t.Errorf("got %+v, want interval=60 updated_at=100", s)
	}
}

func TestMarkLiteLLMSynced(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.MarkLiteLLMSynced(ctx, 12345); err != nil {
		t.Fatalf("MarkLiteLLMSynced: %v", err)
	}
	s, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s.LiteLLMLastSyncedAt != 12345 {
		t.Errorf("LiteLLMLastSyncedAt = %v, want 12345", s.LiteLLMLastSyncedAt)
	}
	// The sync interval itself must be untouched by a sync completing.
	if s.LiteLLMSyncIntervalMinutes != defaultLiteLLMSyncIntervalMinutes {
		t.Errorf("LiteLLMSyncIntervalMinutes changed to %d after MarkLiteLLMSynced", s.LiteLLMSyncIntervalMinutes)
	}
}
