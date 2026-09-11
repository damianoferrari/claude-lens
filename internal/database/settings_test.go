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
	if s.LiteLLMLastSyncError != "" {
		t.Errorf("LiteLLMLastSyncError = %q, want empty (no attempt yet)", s.LiteLLMLastSyncError)
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

func TestMarkLiteLLMSyncFailed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.MarkLiteLLMSyncFailed(ctx, "model/info returned status 404", 555); err != nil {
		t.Fatalf("MarkLiteLLMSyncFailed: %v", err)
	}
	s, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s.LiteLLMLastSyncError != "model/info returned status 404" {
		t.Errorf("LiteLLMLastSyncError = %q, want the recorded error", s.LiteLLMLastSyncError)
	}
	// A failed attempt is not a successful sync — LiteLLMLastSyncedAt must
	// stay at its prior value (0 here, nothing has ever succeeded).
	if s.LiteLLMLastSyncedAt != 0 {
		t.Errorf("LiteLLMLastSyncedAt = %v, want unchanged (0)", s.LiteLLMLastSyncedAt)
	}
	// The attempt clock must advance regardless, so RunLoop waits a full
	// interval before retrying instead of firing on every poll.
	if s.LiteLLMLastAttemptAt != 555 {
		t.Errorf("LiteLLMLastAttemptAt = %v, want 555", s.LiteLLMLastAttemptAt)
	}
}

func TestMarkLiteLLMSynced_ClearsAPriorFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.MarkLiteLLMSyncFailed(ctx, "some earlier failure", 100); err != nil {
		t.Fatalf("MarkLiteLLMSyncFailed: %v", err)
	}
	if err := db.MarkLiteLLMSynced(ctx, 999); err != nil {
		t.Fatalf("MarkLiteLLMSynced: %v", err)
	}

	s, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s.LiteLLMLastSyncError != "" {
		t.Errorf("LiteLLMLastSyncError = %q, want cleared by a subsequent success", s.LiteLLMLastSyncError)
	}
	if s.LiteLLMLastSyncedAt != 999 {
		t.Errorf("LiteLLMLastSyncedAt = %v, want 999", s.LiteLLMLastSyncedAt)
	}
	if s.LiteLLMLastAttemptAt != 999 {
		t.Errorf("LiteLLMLastAttemptAt = %v, want 999 (a success is also an attempt)", s.LiteLLMLastAttemptAt)
	}
}
