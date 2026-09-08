package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lfsc09/claude-lens/internal/database"
)

func TestSettingsGetReturnsSeededDefaults(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var settings database.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if settings.LiteLLMSyncIntervalMinutes <= 0 {
		t.Errorf("LiteLLMSyncIntervalMinutes = %d, want a positive seeded default", settings.LiteLLMSyncIntervalMinutes)
	}
}

func TestSettingsUpdate(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doJSON(t, s, http.MethodPut, "/api/settings", map[string]any{"litellm_sync_interval_minutes": 60})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var settings database.Settings
	json.Unmarshal(rec.Body.Bytes(), &settings)
	if settings.LiteLLMSyncIntervalMinutes != 60 {
		t.Errorf("LiteLLMSyncIntervalMinutes = %d, want 60", settings.LiteLLMSyncIntervalMinutes)
	}

	// 0 is a valid, meaningful value (disables the background sync) — must
	// not be rejected as "missing".
	rec = doJSON(t, s, http.MethodPut, "/api/settings", map[string]any{"litellm_sync_interval_minutes": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for interval=0: %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &settings)
	if settings.LiteLLMSyncIntervalMinutes != 0 {
		t.Errorf("LiteLLMSyncIntervalMinutes = %d, want 0", settings.LiteLLMSyncIntervalMinutes)
	}
}

func TestSettingsUpdate_RejectsMissingAndNegative(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doJSON(t, s, http.MethodPut, "/api/settings", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing field: status = %d, want 400", rec.Code)
	}

	rec = doJSON(t, s, http.MethodPut, "/api/settings", map[string]any{"litellm_sync_interval_minutes": -1})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative value: status = %d, want 400", rec.Code)
	}
}
