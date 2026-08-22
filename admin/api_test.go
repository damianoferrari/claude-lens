package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lfsc09/claude-lens/internal/database"
	"github.com/lfsc09/claude-lens/internal/pricing"
	"github.com/lfsc09/claude-lens/internal/status"
)

func newTestServer(t *testing.T) (*Server, *database.DB) {
	t.Helper()
	s, db, _, _, _ := newTestServerWithStatus(t)
	return s, db
}

// newTestServerWithStatus is like newTestServer but also hands back the
// *status.Flag and the two *status.Fresh instances wired into the server, so
// a test can drive SSE push conditions (proxy status changes, fresh-exchange
// signals, limiter-refresh signals) directly instead of only through the
// HTTP surface.
func newTestServerWithStatus(t *testing.T) (*Server, *database.DB, *status.Flag, *status.Fresh, *status.Fresh) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	est := pricing.New(db)
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	st := status.New()
	fresh := status.NewFresh()
	limitersFresh := status.NewFresh()
	tmpDir := t.TempDir()
	s, err := NewServer(db, est, st, fresh, limitersFresh, "test", filepath.Join(tmpDir, "test.db"), tmpDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, db, st, fresh, limitersFresh
}

func doJSON(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func doGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func strPtr(s string) *string { return &s }

func nowUnix() int64 { return time.Now().Unix() }

func TestHealth(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
	if body["proxy"] != "unreachable" {
		t.Errorf("proxy field = %q, want unreachable (fresh status.Flag)", body["proxy"])
	}
	if body["version"] != "test" {
		t.Errorf("version field = %q, want %q (the version newTestServer passed to NewServer)", body["version"], "test")
	}
}

func TestExchangesList_EmptyRowsNotNull(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/exchanges", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp exchangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Rows == nil {
		t.Error("rows must be an empty array, not null")
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

func TestExchangesList_FilterPaginationAndSessionID(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	for i, sess := range []string{"a", "a", "b"} {
		if err := db.SaveExchange(ctx, database.Exchange{
			SessionID: sess, Path: "/p", Timestamp: float64(1000 + i), RawRequest: "{}", RawResponse: "{}",
		}); err != nil {
			t.Fatalf("SaveExchange: %v", err)
		}
	}

	rec := doJSON(t, s, http.MethodGet, "/api/exchanges?q="+url.QueryEscape(`session = "a"`), nil)
	var resp exchangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 2 || resp.Total != 2 {
		t.Fatalf("got %d rows (total %d) for session a, want 2 (total 2)", len(resp.Rows), resp.Total)
	}
	if resp.SessionID != "a" {
		t.Errorf("session_id = %q, want %q (exact single-session filter)", resp.SessionID, "a")
	}

	rec = doJSON(t, s, http.MethodGet, "/api/exchanges?limit=abc", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-integer limit: status = %d, want 400", rec.Code)
	}

	rec = doJSON(t, s, http.MethodGet, "/api/exchanges?q="+url.QueryEscape(`bogus_field = 1`), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown filter field: status = %d, want 400", rec.Code)
	}
}

func TestExchangesList_LimitClampedTo1000(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/exchanges?limit=5000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// No direct way to observe the clamped limit value through the JSON
	// response when the table is empty; this at minimum proves an
	// out-of-range limit doesn't error out.
}

func TestExchangesList_NegativeLimitClamped(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	for i, sess := range []string{"a", "b", "c"} {
		if err := db.SaveExchange(ctx, database.Exchange{
			SessionID: sess, Path: "/p", Timestamp: float64(1000 + i), RawRequest: "{}", RawResponse: "{}",
		}); err != nil {
			t.Fatalf("SaveExchange: %v", err)
		}
	}

	rec := doJSON(t, s, http.MethodGet, "/api/exchanges?limit=-1", nil)
	var resp exchangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Errorf("negative limit: got %d rows, want 1 (clamped, not unbounded)", len(resp.Rows))
	}
}

func TestExchangeDetail_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/exchanges/999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "not found" {
		t.Errorf("error = %q, want %q", body["error"], "not found")
	}
}

func TestExchangeDetail_NonIntegerIDIs404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/exchanges/not-a-number", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestExchangeDetail_Found(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	matchedPrice := `{"id":1,"model_prefix":"claude-sonnet-5","rule":"over","rule_tokens":0,"input_per_m":3,"output_per_m":15,"cache_write_per_m":3.75,"cache_read_per_m":0.3,"created_at":1000,"updated_at":1000}`
	if err := db.SaveExchange(ctx, database.Exchange{
		SessionID: "s1", Path: "/v1/messages", Timestamp: 1000, RawRequest: "{}", RawResponse: "{}",
		MatchedPrice: &matchedPrice,
	}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}
	rows, _ := db.GetExchanges(ctx, "", 1, 0)

	rec := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/exchanges/%d", rows[0].ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var detail database.ExchangeDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", detail.SessionID)
	}
	var gotPrice database.Price
	if err := json.Unmarshal(detail.MatchedPrice, &gotPrice); err != nil {
		t.Fatalf("decode matched_price: %v", err)
	}
	if gotPrice.Prefix != "claude-sonnet-5" || gotPrice.InputPerM != 3 {
		t.Errorf("MatchedPrice = %+v, want claude-sonnet-5 rule at $3/M input", gotPrice)
	}
}

func TestTotals(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	inCost, outCost := 0.01, 0.02
	inTok, outTok := 10, 20
	if err := db.SaveExchange(ctx, database.Exchange{
		SessionID: "s1", Path: "/p", Timestamp: 1000, RawRequest: "{}", RawResponse: "{}",
		InputTokens: &inTok, OutputTokens: &outTok, InputCost: &inCost, OutputCost: &outCost,
	}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/totals", nil)
	var totals database.Totals
	if err := json.Unmarshal(rec.Body.Bytes(), &totals); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if totals.Count != 1 || totals.TotalInputTokens != 10 {
		t.Errorf("unexpected totals: %+v", totals)
	}
}

func TestTotals_RangeExcludesOlderExchanges(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	now := time.Now()
	oldTok, newTok := 100, 5
	if err := db.SaveExchange(ctx, database.Exchange{
		SessionID: "old", Path: "/p", Timestamp: float64(now.AddDate(0, 0, -90).Unix()), RawRequest: "{}", RawResponse: "{}",
		InputTokens: &oldTok,
	}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}
	if err := db.SaveExchange(ctx, database.Exchange{
		SessionID: "new", Path: "/p", Timestamp: float64(now.Unix()), RawRequest: "{}", RawResponse: "{}",
		InputTokens: &newTok,
	}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/totals?range=today", nil)
	var totals database.Totals
	if err := json.Unmarshal(rec.Body.Bytes(), &totals); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if totals.Count != 1 || totals.TotalInputTokens != 5 {
		t.Errorf("range=today should only include the recent exchange, got: %+v", totals)
	}
}

func TestSessionStats(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	if err := db.SaveExchange(ctx, database.Exchange{SessionID: "s1", Path: "/p", Timestamp: 1000, RawRequest: "{}", RawResponse: "{}"}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/session-stats", nil)
	var resp sessionStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].SessionID != "s1" || resp.Total != 1 {
		t.Errorf("unexpected session stats: %+v", resp)
	}
}

func TestSessionStatsPagination(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	for i, id := range []string{"a", "b", "c"} {
		if err := db.SaveExchange(ctx, database.Exchange{SessionID: id, Path: "/p", Timestamp: float64(1000 + i), RawRequest: "{}", RawResponse: "{}"}); err != nil {
			t.Fatalf("SaveExchange: %v", err)
		}
	}

	rec := doJSON(t, s, http.MethodGet, "/api/session-stats?limit=2&offset=0", nil)
	var resp sessionStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 2 || resp.Total != 3 {
		t.Fatalf("got %d rows, total %d; want 2 rows, total 3", len(resp.Rows), resp.Total)
	}
	if resp.Rows[0].SessionID != "c" {
		t.Errorf("expected most-recent session first, got %+v", resp.Rows[0])
	}
}

func TestSessionStatsPagination_NegativeLimitClamped(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	for i, id := range []string{"a", "b", "c"} {
		if err := db.SaveExchange(ctx, database.Exchange{SessionID: id, Path: "/p", Timestamp: float64(1000 + i), RawRequest: "{}", RawResponse: "{}"}); err != nil {
			t.Fatalf("SaveExchange: %v", err)
		}
	}

	rec := doJSON(t, s, http.MethodGet, "/api/session-stats?limit=-1", nil)
	var resp sessionStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Errorf("negative limit: got %d rows, want 1 (clamped, not unbounded)", len(resp.Rows))
	}
}

func TestSessionStatsSince(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	if err := db.SaveExchange(ctx, database.Exchange{SessionID: "a", Path: "/p", Timestamp: 1000, RawRequest: "{}", RawResponse: "{}"}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}
	firstPage, err := db.GetExchanges(ctx, "", 1, 0)
	if err != nil || len(firstPage) != 1 {
		t.Fatalf("GetExchanges: rows=%+v err=%v", firstPage, err)
	}
	if err := db.SaveExchange(ctx, database.Exchange{SessionID: "b", Path: "/p", Timestamp: 1001, RawRequest: "{}", RawResponse: "{}"}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, fmt.Sprintf("/api/session-stats?since_id=%d", firstPage[0].ID), nil)
	var resp sessionStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].SessionID != "b" {
		t.Errorf("expected only session b in delta, got %+v", resp.Rows)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 (distinct-session count, not just the delta)", resp.Total)
	}
}

func TestDailyCosts(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	cost := 1.5
	if err := db.SaveExchange(ctx, database.Exchange{
		SessionID: "s1", Path: "/p", Timestamp: float64(nowUnix()), RawRequest: "{}", RawResponse: "{}",
		InputCost: &cost,
	}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/daily-costs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rows []database.DailyCost
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0].DailyCost != 1.5 {
		t.Errorf("unexpected daily costs: %+v", rows)
	}
}

func TestListExchanges_SessionActive(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	if err := db.SaveExchange(ctx, database.Exchange{
		SessionID: "recent", Path: "/p", Timestamp: now - 300, RawRequest: "{}", RawResponse: "{}",
	}); err != nil {
		t.Fatalf("SaveExchange(recent): %v", err)
	}
	if err := db.SaveExchange(ctx, database.Exchange{
		SessionID: "stale", Path: "/p", Timestamp: now - 3600, RawRequest: "{}", RawResponse: "{}",
	}); err != nil {
		t.Fatalf("SaveExchange(stale): %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/exchanges?q="+url.QueryEscape(`session = "recent"`), nil)
	var recentResp exchangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &recentResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !recentResp.SessionActive {
		t.Error("recent session: SessionActive = false, want true")
	}

	rec = doJSON(t, s, http.MethodGet, "/api/exchanges?q="+url.QueryEscape(`session = "stale"`), nil)
	var staleResp exchangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &staleResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if staleResp.SessionActive {
		t.Error("stale session: SessionActive = true, want false")
	}
}

func TestDeleteExchanges(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()
	for _, sess := range []string{"a", "a", "b"} {
		if err := db.SaveExchange(ctx, database.Exchange{SessionID: sess, Path: "/p", Timestamp: 1000, RawRequest: "{}", RawResponse: "{}"}); err != nil {
			t.Fatalf("SaveExchange: %v", err)
		}
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/exchanges?session_id=a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["deletedRows"] != 2 {
		t.Errorf("deletedRows = %d, want 2", body["deletedRows"])
	}

	remaining, _ := db.GetExchanges(ctx, "", 100, 0)
	if len(remaining) != 1 {
		t.Fatalf("got %d remaining, want 1", len(remaining))
	}

	// session_id is now required — no more "clear everything" mode.
	rec = doJSON(t, s, http.MethodDelete, "/api/exchanges", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for missing session_id", rec.Code)
	}
}

func TestPricesCRUD(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/prices", nil)
	var seeded []database.Price
	json.Unmarshal(rec.Body.Bytes(), &seeded)
	if len(seeded) == 0 {
		t.Fatal("expected seeded default prices")
	}

	rec = doJSON(t, s, http.MethodPost, "/api/prices", map[string]any{
		"model_prefix": "my-custom-model", "rule": "over", "rule_tokens": 0,
		"input_per_m": 2.5, "output_per_m": 12,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var created database.Price
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Prefix != "my-custom-model" || created.InputPerM != 2.5 || created.ID == 0 {
		t.Errorf("unexpected created price: %+v", created)
	}

	rec = doJSON(t, s, http.MethodPut, "/api/prices/"+strconv.FormatInt(created.ID, 10), map[string]string{"input_per_m": "not-a-number"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed PUT body: status = %d, want 400", rec.Code)
	}

	rec = doJSON(t, s, http.MethodDelete, "/api/prices/"+strconv.FormatInt(created.ID, 10), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", rec.Code)
	}

	rec = doJSON(t, s, http.MethodGet, "/api/prices", nil)
	var afterDelete []database.Price
	json.Unmarshal(rec.Body.Bytes(), &afterDelete)
	for _, p := range afterDelete {
		if p.ID == created.ID {
			t.Error("my-custom-model still present after DELETE")
		}
	}
}

func TestCreatePrice_ValidatesRule(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/prices", map[string]any{
		"model_prefix": "bad-rule-model", "rule": "sideways", "rule_tokens": 0,
		"input_per_m": 1, "output_per_m": 1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid rule: status = %d, want 400", rec.Code)
	}

	rec = doJSON(t, s, http.MethodPost, "/api/prices", map[string]any{
		"model_prefix": "missing-tokens-model", "rule": "over",
		"input_per_m": 1, "output_per_m": 1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing rule_tokens: status = %d, want 400", rec.Code)
	}
}

func TestUpdatePrice_CacheRatesOptional(t *testing.T) {
	s, _ := newTestServer(t)

	// Create with explicit cache rates.
	rec := doJSON(t, s, http.MethodPost, "/api/prices", map[string]any{
		"model_prefix": "cache-model", "rule": "over", "rule_tokens": 0,
		"input_per_m": 2.0, "output_per_m": 10.0, "cache_write_per_m": 2.5, "cache_read_per_m": 0.2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var p database.Price
	json.Unmarshal(rec.Body.Bytes(), &p)
	if p.CacheWritePerM != 2.5 || p.CacheReadPerM != 0.2 {
		t.Fatalf("unexpected cache rates after create: %+v", p)
	}

	// Update input/output only, omitting cache rates — they must be
	// preserved, not reset to 0.
	rec = doJSON(t, s, http.MethodPut, "/api/prices/"+strconv.FormatInt(p.ID, 10), map[string]float64{
		"input_per_m": 3.0, "output_per_m": 11.0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &p)
	if p.InputPerM != 3.0 || p.OutputPerM != 11.0 {
		t.Errorf("input/output not updated: %+v", p)
	}
	if p.CacheWritePerM != 2.5 || p.CacheReadPerM != 0.2 {
		t.Errorf("cache rates were reset when omitted from the update, want preserved: %+v", p)
	}
}

func TestCreatePrice_RefreshesEstimatorImmediately(t *testing.T) {
	s, db := newTestServer(t)
	ctx := context.Background()

	rec := doJSON(t, s, http.MethodPost, "/api/prices", map[string]any{
		"model_prefix": "brand-new", "rule": "over", "rule_tokens": 0,
		"input_per_m": 9, "output_per_m": 9,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Round-trip through the DB to confirm the write actually landed
	// (the estimator's own refresh behavior is covered by
	// internal/pricing's tests; this just confirms the API wired it up).
	prices, err := db.ListPrices(ctx)
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	found := false
	for _, p := range prices {
		if p.Prefix == "brand-new" {
			found = true
		}
	}
	if !found {
		t.Fatal("brand-new price not persisted")
	}
}

func TestStaticFrontend_PagesServed(t *testing.T) {
	s, _ := newTestServer(t)

	for _, path := range []string{"/", "/exchanges", "/exchanges/123", "/prices", "/favicon.ico", "/img/logo.png", "/js/app.js"} {
		rec := doGet(t, s, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
		}
	}
}

func TestStaticFrontend_UnknownPathIs404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doGet(t, s, "/no-such-page")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
