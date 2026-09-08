package admin

import (
	"encoding/json"
	"net/http"
	"time"
)

func (h *handlers) getSettings(w http.ResponseWriter, r *http.Request) {
	s, err := h.db.GetSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

type updateSettingsRequest struct {
	LiteLLMSyncIntervalMinutes *int `json:"litellm_sync_interval_minutes"`
}

// updateSettings sets how often (in minutes) the background loop in
// internal/pricesync re-syncs model_prices from LiteLLM; 0 disables it.
// Takes effect on the loop's next poll (at most a minute later) — no
// restart needed, unlike an env var would require.
func (h *handlers) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req updateSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.LiteLLMSyncIntervalMinutes == nil || *req.LiteLLMSyncIntervalMinutes < 0 {
		writeError(w, http.StatusBadRequest, "litellm_sync_interval_minutes is required and must be >= 0")
		return
	}

	if err := h.db.UpdateLiteLLMSyncInterval(r.Context(), *req.LiteLLMSyncIntervalMinutes, float64(time.Now().Unix())); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s, err := h.db.GetSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}
