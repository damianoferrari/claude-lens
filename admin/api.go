package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lfsc09/claude-lens/internal/database"
	"github.com/lfsc09/claude-lens/internal/files"
	"github.com/lfsc09/claude-lens/internal/pricesync"
	"github.com/lfsc09/claude-lens/internal/pricing"
	"github.com/lfsc09/claude-lens/internal/status"
)

// parsePageParams reads limit/offset query params, defaulting limit to
// defaultLimit and clamping it to [1, maxLimit]; offset clamps to >= 0.
func parsePageParams(r *http.Request, defaultLimit, maxLimit int) (limit, offset int, err error) {
	limit, offset = defaultLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil {
			return 0, 0, errors.New("limit and offset must be integers")
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil {
			return 0, 0, errors.New("limit and offset must be integers")
		}
	}
	limit = min(max(limit, 1), maxLimit)
	offset = max(offset, 0)
	return limit, offset, nil
}

// handlers holds the dependencies shared by every admin route.
type handlers struct {
	db            *database.DB
	est           *pricing.Estimator
	status        *status.Flag
	fresh         *status.Fresh
	limitersFresh *status.Fresh
	logger        *slog.Logger
	version       string
	dbPath        string
	logPath       string

	// priceProviders/proxyBaseURL/proxyAuthToken back syncPrices.
	// proxyBaseURL/proxyAuthToken are the same upstream claude-lens' proxy
	// forwards to — only meaningful when that upstream is also the price
	// sync provider's gateway.
	priceProviders pricesync.Providers
	proxyBaseURL   string
	proxyAuthToken string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Warn("writeJSON: write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"proxy":   string(h.status.Get()),
		"version": h.version,
	})
}

// exchangesResponse wraps a page of exchanges with the total count matching
// the current filter, for pagination.
type exchangesResponse struct {
	Rows  []database.ExchangeSummary `json:"rows"`
	Total int                        `json:"total"`
}

func (h *handlers) listExchanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")

	limit, offset, err := parsePageParams(r, 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rows, err := h.db.GetExchanges(r.Context(), q, limit, offset)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rows == nil {
		rows = []database.ExchangeSummary{}
	}
	total, err := h.db.CountExchanges(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exchangesResponse{Rows: rows, Total: total})
}

func (h *handlers) exchangeDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	row, err := h.db.GetExchangeDetail(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// bulkDeleteExchangesRequest is the payload for POST /api/exchanges/bulk-delete.
// SessionIDs scopes the delete to specific sessions; an empty SessionIDs
// deletes every exchange, but only when ConfirmAll is set, guarding against
// an accidental full wipe from a malformed/empty request.
type bulkDeleteExchangesRequest struct {
	SessionIDs              []string `json:"session_ids"`
	AlsoDeleteClaudeSession bool     `json:"also_delete_claude_session"`
	ConfirmAll              bool     `json:"confirm_all"`
}

// bulkDeleteExchanges deletes exchanges for the given sessions, or every
// exchange when session_ids is empty and confirm_all is set. When
// also_delete_claude_session is set, it also removes each affected
// session's on-disk Claude Code files and its session_names entry;
// failures there are logged but never fail the request, since the
// exchanges delete has already committed by that point.
func (h *handlers) bulkDeleteExchanges(w http.ResponseWriter, r *http.Request) {
	var req bulkDeleteExchangesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.SessionIDs) == 0 && !req.ConfirmAll {
		writeError(w, http.StatusBadRequest, "confirm_all is required to delete every session")
		return
	}

	deletedRows, affectedSessions, err := h.db.DeleteExchanges(r.Context(), req.SessionIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var deletedFiles int
	if req.AlsoDeleteClaudeSession {
		for _, sessionID := range affectedSessions {
			n, err := files.TryDeleteClaudeSession(sessionID)
			deletedFiles += n
			if err != nil {
				h.logger.Error("failed to delete Claude session", "error", err, "session_id", sessionID, "deleted", n)
			}
		}
		if err := h.db.DeleteSessionNames(r.Context(), affectedSessions); err != nil {
			h.logger.Error("failed to delete session names", "error", err, "session_ids", affectedSessions)
		}
	}

	h.logger.Info("bulk delete exchanges", "deletedRows", deletedRows, "sessionsAffected", len(affectedSessions), "also_delete_claude_session", req.AlsoDeleteClaudeSession, "deleted_files", deletedFiles)
	writeJSON(w, http.StatusOK, map[string]int64{
		"deletedRows":      deletedRows,
		"deletedFiles":     int64(deletedFiles),
		"sessionsAffected": int64(len(affectedSessions)),
	})
}

// totals accepts either ?session_id= or ?range= (never both in practice —
// range powers the dashboard, session_id powers nothing today but is kept
// for parity with the old JSON API). An absent/unrecognized range param
// leaves `since` nil, matching GetTokenTotals' all-time default.
func (h *handlers) totals(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	var since *float64
	if rangeKey := r.URL.Query().Get("range"); rangeKey != "" {
		s := sinceForRange(normalizeDashboardRange(rangeKey), time.Now())
		since = &s
	}
	t, err := h.db.GetTokenTotals(r.Context(), sessionID, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// sessionStatsResponse wraps a page of per-session aggregates with the total
// distinct-session count, mirroring exchangesResponse's shape. Total reflects
// the same underlying value in both limit/offset and since_id delta mode.
type sessionStatsResponse struct {
	Rows  []database.SessionStat `json:"rows"`
	Total int                    `json:"total"`
}

// sessionStats powers the dashboard's by-session table. With ?since_id=N it
// returns only the sessions that logged an exchange after N (used to
// live-patch the table without a full refetch); otherwise it returns a
// limit/offset page ordered by recency.
func (h *handlers) sessionStats(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("since_id"); v != "" {
		sinceID, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since_id must be an integer")
			return
		}
		rows, err := h.db.GetSessionStatsSince(r.Context(), sinceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rows == nil {
			rows = []database.SessionStat{}
		}
		total, err := h.db.CountSessionStats(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sessionStatsResponse{Rows: rows, Total: total})
		return
	}

	limit, offset, err := parsePageParams(r, 50, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rows, err := h.db.GetSessionStats(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []database.SessionStat{}
	}
	total, err := h.db.CountSessionStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessionStatsResponse{Rows: rows, Total: total})
}

type sessionNameRequest struct {
	Name string `json:"name"`
}

// setSessionName sets or clears a session's display name. An empty (or
// whitespace-only) name clears it back to showing the raw session id.
func (h *handlers) setSessionName(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	var req sessionNameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) > 200 {
		writeError(w, http.StatusBadRequest, "name must be at most 200 characters")
		return
	}

	if err := h.db.SetSessionName(r.Context(), sessionID, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := map[string]any{"session_id": sessionID, "session_name": nil}
	if name != "" {
		resp["session_name"] = name
	}
	writeJSON(w, http.StatusOK, resp)
}

// dailyCosts powers the dashboard's spending heatmap. ?days= defaults to 60
// (the heatmap's fixed window) and falls back to that default for anything
// absent, non-numeric, or non-positive.
func (h *handlers) dailyCosts(w http.ResponseWriter, r *http.Request) {
	days := 60
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	rows, err := h.db.GetDailyCosts(r.Context(), days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []database.DailyCost{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *handlers) listPrices(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.ListPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []database.Price{}
	}
	writeJSON(w, http.StatusOK, rows)
}

type createPriceRequest struct {
	Prefix                  string   `json:"model_prefix"`
	InputPerM               *float64 `json:"input_per_m"`
	OutputPerM              *float64 `json:"output_per_m"`
	CacheWritePerM          *float64 `json:"cache_write_per_m"`
	CacheReadPerM           *float64 `json:"cache_read_per_m"`
	InputPerMAbove200k      *float64 `json:"input_per_m_above_200k"`
	OutputPerMAbove200k     *float64 `json:"output_per_m_above_200k"`
	CacheWritePerMAbove200k *float64 `json:"cache_write_per_m_above_200k"`
	CacheReadPerMAbove200k  *float64 `json:"cache_read_per_m_above_200k"`
}

// isUniqueConstraintErr reports whether err is SQLite's error for a UNIQUE
// constraint violation.
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// createPrice adds the price row for a prefix. A prefix owns at most one
// row — creating a second one for the same prefix fails with 409, since
// model_prices.model_prefix is UNIQUE.
func (h *handlers) createPrice(w http.ResponseWriter, r *http.Request) {
	var req createPriceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.Prefix == "" || req.InputPerM == nil || req.OutputPerM == nil {
		writeError(w, http.StatusBadRequest, "model_prefix, input_per_m and output_per_m are required")
		return
	}

	var cacheWritePerM, cacheReadPerM float64
	if req.CacheWritePerM != nil {
		cacheWritePerM = *req.CacheWritePerM
	}
	if req.CacheReadPerM != nil {
		cacheReadPerM = *req.CacheReadPerM
	}

	now := float64(time.Now().Unix())
	p := database.Price{
		Prefix:                  req.Prefix,
		InputPerM:               *req.InputPerM,
		OutputPerM:              *req.OutputPerM,
		CacheWritePerM:          cacheWritePerM,
		CacheReadPerM:           cacheReadPerM,
		InputPerMAbove200k:      req.InputPerMAbove200k,
		OutputPerMAbove200k:     req.OutputPerMAbove200k,
		CacheWritePerMAbove200k: req.CacheWritePerMAbove200k,
		CacheReadPerMAbove200k:  req.CacheReadPerMAbove200k,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	id, err := h.db.CreatePrice(r.Context(), p)
	if err != nil {
		if isUniqueConstraintErr(err) {
			writeError(w, http.StatusConflict, "a price already exists for this prefix — edit it instead")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.ID = id
	if err := h.est.Refresh(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// syncPrices triggers an on-demand internal/pricesync.Sync, against
// whichever provider database.Settings.PriceSyncProvider currently names —
// the same sync also run periodically in the background (see main.go) —
// and reports what it did so the admin UI can show a meaningful summary
// instead of a bare "ok".
//
// Status code distinguishes the failure kind: 502 specifically means the
// upstream isn't that provider's gateway (see pricesync.ErrUpstreamUnavailable),
// which the Prices page's JS uses to grey out the manual sync button —
// anything else (e.g. a DB write error) isn't a capability signal, so it
// stays a plain 500 and the button stays usable for an immediate retry.
func (h *handlers) syncPrices(w http.ResponseWriter, r *http.Request) {
	settings, err := h.db.GetSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	client, err := h.priceProviders.Resolve(settings.PriceSyncProvider)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := pricesync.Sync(r.Context(), h.db, h.est, client, h.proxyBaseURL, h.proxyAuthToken)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, pricesync.ErrUpstreamUnavailable) {
			status = http.StatusBadGateway
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type updatePriceRequest struct {
	InputPerM               *float64 `json:"input_per_m"`
	OutputPerM              *float64 `json:"output_per_m"`
	CacheWritePerM          *float64 `json:"cache_write_per_m"`
	CacheReadPerM           *float64 `json:"cache_read_per_m"`
	InputPerMAbove200k      *float64 `json:"input_per_m_above_200k"`
	OutputPerMAbove200k     *float64 `json:"output_per_m_above_200k"`
	CacheWritePerMAbove200k *float64 `json:"cache_write_per_m_above_200k"`
	CacheReadPerMAbove200k  *float64 `json:"cache_read_per_m_above_200k"`
}

// updatePrice replaces an existing price row's rates and above-200k
// overrides wholesale. The dialog always submits the full form, so a
// missing tier field means "clear the override", not "leave unchanged".
// Prefix is immutable once created.
func (h *handlers) updatePrice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	var req updatePriceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InputPerM == nil || req.OutputPerM == nil {
		writeError(w, http.StatusBadRequest, "input_per_m and output_per_m are required numbers")
		return
	}

	existing, err := h.db.GetPrice(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	var cacheWritePerM, cacheReadPerM float64
	if req.CacheWritePerM != nil {
		cacheWritePerM = *req.CacheWritePerM
	}
	if req.CacheReadPerM != nil {
		cacheReadPerM = *req.CacheReadPerM
	}

	updatedAt := float64(time.Now().Unix())
	p := database.Price{
		InputPerM:               *req.InputPerM,
		OutputPerM:              *req.OutputPerM,
		CacheWritePerM:          cacheWritePerM,
		CacheReadPerM:           cacheReadPerM,
		InputPerMAbove200k:      req.InputPerMAbove200k,
		OutputPerMAbove200k:     req.OutputPerMAbove200k,
		CacheWritePerMAbove200k: req.CacheWritePerMAbove200k,
		CacheReadPerMAbove200k:  req.CacheReadPerMAbove200k,
		UpdatedAt:               updatedAt,
	}
	if err := h.db.UpdatePrice(r.Context(), id, p); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.est.Refresh(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	p.ID, p.Prefix, p.CreatedAt = existing.ID, existing.Prefix, existing.CreatedAt
	writeJSON(w, http.StatusOK, p)
}

func (h *handlers) deletePrice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err := h.db.DeletePrice(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.est.Refresh(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": id})
}

func (h *handlers) listLimiters(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.ListLimiters(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []database.Limiter{}
	}
	for i := range rows {
		rows[i].WithinActivePeriod = database.WithinActivePeriodNow(rows[i])
	}
	writeJSON(w, http.StatusOK, rows)
}

type limiterRequest struct {
	SessionID       string   `json:"session_id"`
	LimitAmount     *float64 `json:"limit_amount"`
	RefreshValue    *int     `json:"refresh_value"`
	RefreshUnit     string   `json:"refresh_unit"`
	RefreshAligned  bool     `json:"refresh_aligned"`
	ActiveStartHour *int     `json:"active_start_hour"`
	ActiveEndHour   *int     `json:"active_end_hour"`
	SlackWebhookURL string   `json:"slack_webhook_url"`
	// AlertThresholdPct fires a one-shot Slack alert the first time the
	// limiter's spend reaches this percentage of its budget within the
	// current refresh window. Nil disables the alert.
	AlertThresholdPct *int `json:"alert_threshold_pct"`
	// AlertRequestCostUSD fires a Slack alert every time a single exchange
	// governed by this limiter costs at least this much. Nil disables it.
	AlertRequestCostUSD *float64 `json:"alert_request_cost_usd"`
}

var limiterRefreshUnits = map[string]bool{"minutes": true, "hours": true, "days": true, "months": true}

// validateLimiterRequest checks the field presence/range rules shared by
// create and update, returning a client-facing message on the first
// violation found, or "" if req is valid.
func validateLimiterRequest(req limiterRequest) string {
	if req.LimitAmount == nil || *req.LimitAmount <= 0 {
		return "limit_amount must be a positive number"
	}
	if req.RefreshValue == nil || *req.RefreshValue <= 0 {
		return "refresh_value must be a positive integer"
	}
	if !limiterRefreshUnits[req.RefreshUnit] {
		return "refresh_unit must be one of minutes, hours, days, months"
	}
	if req.RefreshAligned && !database.SupportsAlignedRefresh(req.RefreshUnit, *req.RefreshValue) {
		return "refresh_aligned is only supported for 60 minutes, 1 hour, 24 hours, 1 day, 7 days, or 1 month"
	}
	if (req.ActiveStartHour == nil) != (req.ActiveEndHour == nil) {
		return "active_start_hour and active_end_hour must both be set or both be empty"
	}
	if req.ActiveStartHour != nil && (*req.ActiveStartHour < 0 || *req.ActiveStartHour > 23 || *req.ActiveEndHour < 0 || *req.ActiveEndHour > 23) {
		return "active_start_hour and active_end_hour must be between 0 and 23"
	}
	if req.AlertThresholdPct != nil && (*req.AlertThresholdPct < 1 || *req.AlertThresholdPct > 100) {
		return "alert_threshold_pct must be between 1 and 100"
	}
	if req.AlertRequestCostUSD != nil && *req.AlertRequestCostUSD <= 0 {
		return "alert_request_cost_usd must be a positive number"
	}
	if req.SlackWebhookURL != "" && !strings.HasPrefix(req.SlackWebhookURL, "https://") {
		return "slack_webhook_url must be a https:// URL"
	}
	if req.SlackWebhookURL == "" && (req.AlertThresholdPct != nil || req.AlertRequestCostUSD != nil) {
		return "slack_webhook_url is required when an alert is configured"
	}
	return ""
}

// overlapMessage describes an existing limiter that a new/edited active
// period collides with, for a 409 response.
func overlapMessage(l *database.Limiter) string {
	scope := "global"
	if l.SessionID != "" {
		scope = "session " + l.SessionID
	}
	period := "always active"
	if l.ActiveStartHour != nil {
		period = fmt.Sprintf("%02d–%02d", *l.ActiveStartHour, *l.ActiveEndHour)
	}
	return fmt.Sprintf("active period overlaps with limiter #%d (%s, %s)", l.ID, scope, period)
}

// resolveLimiterScope trims req.SessionID and checks it against
// FindOverlappingLimiter, writing the appropriate error response and
// returning ok=false if the caller should stop. excludeID skips a row being
// updated (pass 0 when creating).
func (h *handlers) resolveLimiterScope(w http.ResponseWriter, r *http.Request, req limiterRequest, excludeID int64) (sessionID string, ok bool) {
	sessionID = strings.TrimSpace(req.SessionID)
	conflict, err := h.db.FindOverlappingLimiter(r.Context(), sessionID, excludeID, req.ActiveStartHour, req.ActiveEndHour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return "", false
	}
	if conflict != nil {
		writeError(w, http.StatusConflict, overlapMessage(conflict))
		return "", false
	}
	return sessionID, true
}

// createLimiter adds a new limiter. An empty session_id scopes it globally.
// Rejects an active period that overlaps another limiter already covering
// the same session_id (see database.FindOverlappingLimiter).
func (h *handlers) createLimiter(w http.ResponseWriter, r *http.Request) {
	var req limiterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if msg := validateLimiterRequest(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	sessionID, ok := h.resolveLimiterScope(w, r, req, 0)
	if !ok {
		return
	}

	now := time.Now()
	l := database.Limiter{
		SessionID:           sessionID,
		LimitAmount:         *req.LimitAmount,
		RefreshValue:        *req.RefreshValue,
		RefreshUnit:         req.RefreshUnit,
		RefreshAligned:      req.RefreshAligned,
		NextRefreshAt:       float64(database.ComputeNextRefresh(now, req.RefreshUnit, *req.RefreshValue, req.RefreshAligned).Unix()),
		ActiveStartHour:     req.ActiveStartHour,
		ActiveEndHour:       req.ActiveEndHour,
		IsActive:            true,
		CreatedAt:           float64(now.Unix()),
		UpdatedAt:           float64(now.Unix()),
		SlackWebhookURL:     strings.TrimSpace(req.SlackWebhookURL),
		AlertThresholdPct:   req.AlertThresholdPct,
		AlertRequestCostUSD: req.AlertRequestCostUSD,
	}
	id, err := h.db.CreateLimiter(r.Context(), l)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.ID = id
	l.WithinActivePeriod = database.WithinActivePeriodNow(l)
	writeJSON(w, http.StatusOK, l)
}

// updateLimiter replaces a limiter's rule fields. Per the product rule,
// progress only resets when the edit actually changes the refresh
// schedule (refresh_value, refresh_unit, or refresh_aligned): current_cost
// goes back to 0 and next_refresh_at is recomputed from now, rather than
// trying to carry a partial window across a changed schedule. Other fields
// (limit_amount, active hours, session_id) leave progress untouched, so
// e.g. raising limit_amount doesn't cost the user their current_cost.
// is_active is untouched too — that's setLimiterActive's job.
func (h *handlers) updateLimiter(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	var req limiterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if msg := validateLimiterRequest(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	existing, err := h.db.GetLimiter(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	sessionID, ok := h.resolveLimiterScope(w, r, req, id)
	if !ok {
		return
	}

	scheduleChanged := *req.RefreshValue != existing.RefreshValue ||
		req.RefreshUnit != existing.RefreshUnit ||
		req.RefreshAligned != existing.RefreshAligned

	now := time.Now()
	existing.SessionID = sessionID
	existing.LimitAmount = *req.LimitAmount
	existing.RefreshValue = *req.RefreshValue
	existing.RefreshUnit = req.RefreshUnit
	existing.RefreshAligned = req.RefreshAligned
	existing.ActiveStartHour = req.ActiveStartHour
	existing.ActiveEndHour = req.ActiveEndHour
	existing.UpdatedAt = float64(now.Unix())
	existing.SlackWebhookURL = strings.TrimSpace(req.SlackWebhookURL)
	existing.AlertThresholdPct = req.AlertThresholdPct
	existing.AlertRequestCostUSD = req.AlertRequestCostUSD
	if scheduleChanged {
		existing.CurrentCost = 0
		existing.AlertSent = false
		existing.NextRefreshAt = float64(database.ComputeNextRefresh(now, req.RefreshUnit, *req.RefreshValue, req.RefreshAligned).Unix())
	}

	if err := h.db.UpdateLimiter(r.Context(), *existing); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	existing.WithinActivePeriod = database.WithinActivePeriodNow(*existing)
	writeJSON(w, http.StatusOK, existing)
}

type setLimiterActiveRequest struct {
	IsActive *bool `json:"is_active"`
}

// setLimiterActive toggles a limiter's master switch without resetting its
// accrued progress.
func (h *handlers) setLimiterActive(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	var req setLimiterActiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IsActive == nil {
		writeError(w, http.StatusBadRequest, "is_active is required")
		return
	}

	updatedAt := float64(time.Now().Unix())
	if err := h.db.SetLimiterActive(r.Context(), id, *req.IsActive, updatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"is_active": *req.IsActive})
}

func (h *handlers) deleteLimiter(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err := h.db.DeleteLimiter(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": id})
}
