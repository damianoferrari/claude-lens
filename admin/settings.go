package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/lfsc09/claude-lens/internal/database"
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
	LiteLLMSyncIntervalMinutes *int    `json:"litellm_sync_interval_minutes"`
	PriceSyncProvider          *string `json:"price_sync_provider"`
}

// updateSettings sets how often (in minutes) the background loop in
// internal/pricesync re-syncs model_prices, and/or which upstream it syncs
// from; 0 for the interval disables it. Either field may be omitted to
// leave it unchanged — the Prices page sends them independently (the
// interval input and the provider selector have separate change handlers).
// Takes effect on the loop's next poll (at most a minute later) — no
// restart needed, unlike an env var would require.
func (h *handlers) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req updateSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LiteLLMSyncIntervalMinutes == nil && req.PriceSyncProvider == nil {
		writeError(w, http.StatusBadRequest, "at least one of litellm_sync_interval_minutes or price_sync_provider is required")
		return
	}
	if req.LiteLLMSyncIntervalMinutes != nil && *req.LiteLLMSyncIntervalMinutes < 0 {
		writeError(w, http.StatusBadRequest, "litellm_sync_interval_minutes must be >= 0")
		return
	}
	if req.PriceSyncProvider != nil && *req.PriceSyncProvider != database.ProviderLiteLLM && *req.PriceSyncProvider != database.ProviderBifrost {
		writeError(w, http.StatusBadRequest, "price_sync_provider must be \""+database.ProviderLiteLLM+"\" or \""+database.ProviderBifrost+"\"")
		return
	}

	now := float64(time.Now().Unix())
	if req.LiteLLMSyncIntervalMinutes != nil {
		if err := h.db.UpdateLiteLLMSyncInterval(r.Context(), *req.LiteLLMSyncIntervalMinutes, now); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.PriceSyncProvider != nil {
		if err := h.db.UpdatePriceSyncProvider(r.Context(), *req.PriceSyncProvider, now); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	s, err := h.db.GetSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}
