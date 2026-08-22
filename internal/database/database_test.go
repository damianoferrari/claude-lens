package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }
func strPtr(s string) *string     { return &s }

func TestOpen_SeedsDefaultPrices(t *testing.T) {
	db := openTestDB(t)
	prices, err := db.ListPrices(context.Background())
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	if len(prices) != len(defaultPrices) {
		t.Fatalf("got %d seeded prices, want %d", len(prices), len(defaultPrices))
	}

	// Re-opening must not duplicate or reset seeded/edited rows.
	var opusID int64
	for _, p := range prices {
		if p.Prefix == "claude-opus-5" {
			opusID = p.ID
		}
	}
	if err := db.UpdatePrice(context.Background(), opusID, 99, 99, 0, 0, 1); err != nil {
		t.Fatalf("UpdatePrice: %v", err)
	}
	if err := db.seedDefaultPrices(context.Background()); err != nil {
		t.Fatalf("seedDefaultPrices (idempotency check): %v", err)
	}
	prices, err = db.ListPrices(context.Background())
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	for _, p := range prices {
		if p.Prefix == "claude-opus-5" && p.InputPerM != 99 {
			t.Fatalf("seedDefaultPrices overwrote an existing row: %+v", p)
		}
	}
}

func TestPricesCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreatePrice(ctx, Price{Prefix: "custom-model", Rule: "over", RuleTokens: 0, InputPerM: 2.5, OutputPerM: 10, CreatedAt: 123, UpdatedAt: 123})
	if err != nil {
		t.Fatalf("CreatePrice: %v", err)
	}
	if err := db.UpdatePrice(ctx, id, 3.5, 11, 0, 0, 124); err != nil {
		t.Fatalf("UpdatePrice: %v", err)
	}

	prices, err := db.ListPrices(ctx)
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	found := false
	for _, p := range prices {
		if p.ID == id {
			found = true
			if p.InputPerM != 3.5 || p.OutputPerM != 11 {
				t.Errorf("update did not apply in place: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("custom-model not found after create")
	}

	if err := db.DeletePrice(ctx, id); err != nil {
		t.Fatalf("DeletePrice: %v", err)
	}
	if err := db.DeletePrice(ctx, 9999999); err != nil {
		t.Fatalf("DeletePrice on missing id should not error: %v", err)
	}
	prices, err = db.ListPrices(ctx)
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	for _, p := range prices {
		if p.ID == id {
			t.Fatal("custom-model still present after delete")
		}
	}
}

func TestSaveAndGetExchange(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	matchedPrice := `{"id":1,"model_prefix":"claude-sonnet-5","rule":"over","rule_tokens":0,"input_per_m":3,"output_per_m":15}`
	e := Exchange{
		SessionID:     "sess-1",
		SessionName:   strPtr("my session"),
		Path:          "/v1/messages",
		Timestamp:     now,
		IsStreaming:   true,
		InputMessages: strPtr(`[{"role":"user","content":"hi"}]`),
		RawRequest:    `{"model":"claude-sonnet-5"}`,
		RawResponse:   `{"type":"message"}`,
		OutputText:    strPtr("hello there"),
		InputTokens:   intPtr(100),
		OutputTokens:  intPtr(50),
		Model:         strPtr("claude-sonnet-5"),
		InputCost:     floatPtr(0.0003),
		OutputCost:    floatPtr(0.00075),
		MatchedPrice:  &matchedPrice,
	}
	if err := db.SaveExchange(ctx, e); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}

	list, err := db.GetExchanges(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("GetExchanges: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d exchanges, want 1", len(list))
	}
	got := list[0]
	if got.SessionID != "sess-1" || got.Model == nil || *got.Model != "claude-sonnet-5" {
		t.Errorf("unexpected summary row: %+v", got)
	}
	wantCost := 0.00105
	if got.Cost == nil || round4(*got.Cost) != round4(wantCost) {
		t.Errorf("cost = %v, want ~%v", got.Cost, wantCost)
	}

	detail, err := db.GetExchangeDetail(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetExchangeDetail: %v", err)
	}
	if detail == nil {
		t.Fatal("GetExchangeDetail returned nil for existing id")
	}
	if detail.RawRequest == nil || *detail.RawRequest != e.RawRequest {
		t.Errorf("RawRequest mismatch: %+v", detail.RawRequest)
	}
	if detail.OutputText == nil || *detail.OutputText != "hello there" {
		t.Errorf("OutputText mismatch: %+v", detail.OutputText)
	}
	if string(detail.MatchedPrice) != matchedPrice {
		t.Errorf("MatchedPrice = %s, want %s", detail.MatchedPrice, matchedPrice)
	}

	missing, err := db.GetExchangeDetail(ctx, got.ID+999)
	if err != nil {
		t.Fatalf("GetExchangeDetail(missing): %v", err)
	}
	if missing != nil {
		t.Fatalf("GetExchangeDetail(missing) = %+v, want nil", missing)
	}
}

func TestGetExchangeSessionInputs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	if err := db.SaveExchange(ctx, Exchange{
		SessionID: "sess-1", Path: "/p", Timestamp: now,
		InputMessages: strPtr(`[{"role":"user","content":"first"}]`),
		RawRequest:    "{}", RawResponse: "{}",
	}); err != nil {
		t.Fatalf("SaveExchange 1: %v", err)
	}
	if err := db.SaveExchange(ctx, Exchange{
		SessionID: "sess-1", Path: "/p", Timestamp: now + 1,
		InputMessages: strPtr(`[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]`),
		RawRequest:    "{}", RawResponse: "{}",
	}); err != nil {
		t.Fatalf("SaveExchange 2: %v", err)
	}
	if err := db.SaveExchange(ctx, Exchange{
		SessionID: "sess-2", Path: "/p", Timestamp: now,
		InputMessages: strPtr(`[{"role":"user","content":"other session"}]`),
		RawRequest:    "{}", RawResponse: "{}",
	}); err != nil {
		t.Fatalf("SaveExchange other session: %v", err)
	}

	list, err := db.GetExchanges(ctx, `session = "sess-1"`, 10, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("GetExchanges: rows=%+v err=%v", list, err)
	}
	first, second := list[1], list[0] // GetExchanges is newest first
	if first.Timestamp > second.Timestamp {
		first, second = second, first
	}

	rows, err := db.GetExchangeSessionInputs(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetExchangeSessionInputs: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatalf("got %+v, want exactly the sibling exchange %d", rows, first.ID)
	}
	if rows[0].LastInputMessage == nil || *rows[0].LastInputMessage != `{"role":"user","content":"first"}` {
		t.Errorf("LastInputMessage = %v, want only the sibling's last message, not its full input_messages array", rows[0].LastInputMessage)
	}
}

func TestGetExchanges_SessionFilterAndPagination(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := float64(time.Now().Unix())

	for i, sess := range []string{"a", "a", "b", "a"} {
		e := Exchange{
			SessionID:   sess,
			Path:        "/v1/messages",
			Timestamp:   base + float64(i),
			RawRequest:  "{}",
			RawResponse: "{}",
		}
		if err := db.SaveExchange(ctx, e); err != nil {
			t.Fatalf("SaveExchange[%d]: %v", i, err)
		}
	}

	all, err := db.GetExchanges(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("GetExchanges(all): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d, want 4", len(all))
	}
	// newest first
	if all[0].Timestamp < all[len(all)-1].Timestamp {
		t.Errorf("expected newest-first ordering, got %+v", all)
	}

	sessA, err := db.GetExchanges(ctx, `session = "a"`, 100, 0)
	if err != nil {
		t.Fatalf("GetExchanges(a): %v", err)
	}
	if len(sessA) != 3 {
		t.Fatalf("got %d for session a, want 3", len(sessA))
	}

	page1, err := db.GetExchanges(ctx, "", 2, 0)
	if err != nil {
		t.Fatalf("GetExchanges(page1): %v", err)
	}
	page2, err := db.GetExchanges(ctx, "", 2, 2)
	if err != nil {
		t.Fatalf("GetExchanges(page2): %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("pagination sizes = %d, %d, want 2, 2", len(page1), len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Errorf("page1 and page2 overlap: %+v vs %+v", page1, page2)
	}
}

// TestGetExchanges_FilterQuery exercises the search-box query language (see
// query_filter.go) end-to-end against a real DB: text =/like, numeric/bool
// comparisons, computed fields (cache_tokens), AND/OR precedence, and
// parens overriding that precedence.
func TestGetExchanges_FilterQuery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := float64(time.Now().Unix())

	seed := []Exchange{
		{ // row 1
			SessionID: "sess-a", Path: "/v1/messages", Timestamp: base,
			RawRequest: "{}", RawResponse: "{}",
			Model:               strPtr("claude-sonnet-4-5"),
			InputTokens:         intPtr(100),
			OutputTokens:        intPtr(50),
			CacheCreationTokens: intPtr(10),
			CacheReadTokens:     intPtr(5),
			InputCost:           floatPtr(0.03),
			IsStreaming:         false,
		},
		{ // row 2
			SessionID: "sess-b", SessionName: strPtr("my project"), Path: "/v1/messages", Timestamp: base + 100,
			RawRequest: "{}", RawResponse: "{}",
			Model:        strPtr("claude-haiku-4-5"),
			InputTokens:  intPtr(200),
			OutputTokens: intPtr(80),
			InputCost:    floatPtr(0.20),
			IsStreaming:  true,
		},
		{ // row 3: no usage captured at all (e.g. a parse failure)
			SessionID: "sess-a", Path: "/v1/complete", Timestamp: base + 200,
			RawRequest: "{}", RawResponse: "{}",
			Model:       strPtr("claude-opus-4-5"),
			IsStreaming: false,
		},
	}
	for i, e := range seed {
		if err := db.SaveExchange(ctx, e); err != nil {
			t.Fatalf("SaveExchange[%d]: %v", i, err)
		}
	}

	tests := []struct {
		name      string
		query     string
		wantPaths []string // by .Path, since it's distinct per row above
	}{
		{"text like", `model like sonnet`, []string{"/v1/messages"}},
		{"text equals exact model", `model = "claude-haiku-4-5"`, []string{"/v1/messages"}},
		{"session matches two rows", `session = "sess-a"`, []string{"/v1/complete", "/v1/messages"}},
		{"path like across sessions", `path like messages`, []string{"/v1/messages", "/v1/messages"}},
		{"bool stream", `stream = true`, []string{"/v1/messages"}},
		{"number gt", `input_tokens > 150`, []string{"/v1/messages"}},
		{"cost gt", `cost > 0.1`, []string{"/v1/messages"}},
		{"and excludes non-matching cost", `model like sonnet AND cost > 0.1`, nil},
		{"or includes both", `model like sonnet OR cost > 0.1`, []string{"/v1/messages", "/v1/messages"}},
		{
			"parens override precedence",
			`(model like sonnet OR model like haiku) AND session = "sess-a"`,
			[]string{"/v1/messages"},
		},
		{"cache_tokens computed field", `cache_tokens > 0`, []string{"/v1/messages"}},
		{"total_tokens excludes no-usage row", `total_tokens > 0`, []string{"/v1/messages", "/v1/messages"}},
		{"no match", `model = "does-not-exist"`, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := db.GetExchanges(ctx, tt.query, 100, 0)
			if err != nil {
				t.Fatalf("GetExchanges(%q): %v", tt.query, err)
			}
			var gotPaths []string
			for _, r := range rows {
				gotPaths = append(gotPaths, r.Path)
			}
			sort.Strings(gotPaths)
			want := append([]string(nil), tt.wantPaths...)
			sort.Strings(want)
			if !reflect.DeepEqual(gotPaths, want) {
				t.Errorf("GetExchanges(%q) paths = %v, want %v", tt.query, gotPaths, want)
			}
		})
	}

	if _, err := db.GetExchanges(ctx, `nope = 1`, 100, 0); err == nil {
		t.Error("GetExchanges with an unknown filter field: expected error, got nil")
	}
}

func TestGetTokenTotals(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	rows := []Exchange{
		{SessionID: "a", Path: "/p", Timestamp: now, RawRequest: "{}", RawResponse: "{}",
			InputTokens: intPtr(10), OutputTokens: intPtr(20), InputCost: floatPtr(0.01), OutputCost: floatPtr(0.02)},
		{SessionID: "a", Path: "/p", Timestamp: now + 1, RawRequest: "{}", RawResponse: "{}",
			InputTokens: intPtr(5), OutputTokens: intPtr(15), InputCost: floatPtr(0.005), OutputCost: floatPtr(0.015)},
		{SessionID: "b", Path: "/p", Timestamp: now + 2, RawRequest: "{}", RawResponse: "{}",
			InputTokens: intPtr(1), OutputTokens: intPtr(1)},
	}
	for _, r := range rows {
		if err := db.SaveExchange(ctx, r); err != nil {
			t.Fatalf("SaveExchange: %v", err)
		}
	}

	totals, err := db.GetTokenTotals(ctx, "", nil)
	if err != nil {
		t.Fatalf("GetTokenTotals(all): %v", err)
	}
	if totals.Count != 3 || totals.TotalInputTokens != 16 || totals.TotalOutputTokens != 36 {
		t.Errorf("unexpected totals: %+v", totals)
	}
	if totals.TotalCost == nil || round4(*totals.TotalCost) != round4(0.05) {
		t.Errorf("TotalCost = %v, want ~0.05 (row without cost contributes NULL, not 0)", deref(totals.TotalCost))
	}

	sessA, err := db.GetTokenTotals(ctx, "a", nil)
	if err != nil {
		t.Fatalf("GetTokenTotals(a): %v", err)
	}
	if sessA.Count != 2 || sessA.TotalInputTokens != 15 {
		t.Errorf("unexpected session-scoped totals: %+v", sessA)
	}

	since := now + 1
	recent, err := db.GetTokenTotals(ctx, "", &since)
	if err != nil {
		t.Fatalf("GetTokenTotals(since): %v", err)
	}
	if recent.Count != 2 {
		t.Errorf("since-filtered count = %d, want 2", recent.Count)
	}
}

func TestGetSessionStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	_ = db.SaveExchange(ctx, Exchange{SessionID: "old", SessionName: strPtr("Old"), Path: "/p", Timestamp: now, RawRequest: "{}", RawResponse: "{}"})
	_ = db.SaveExchange(ctx, Exchange{SessionID: "new", SessionName: strPtr("New"), Path: "/p", Timestamp: now + 10, RawRequest: "{}", RawResponse: "{}"})

	stats, err := db.GetSessionStats(ctx, 50, 0)
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d session stats, want 2", len(stats))
	}
	if stats[0].SessionID != "new" {
		t.Errorf("expected most-recently-active session first, got %+v", stats[0])
	}

	page, err := db.GetSessionStats(ctx, 1, 0)
	if err != nil {
		t.Fatalf("GetSessionStats(limit=1): %v", err)
	}
	if len(page) != 1 || page[0].SessionID != "new" {
		t.Fatalf("got %+v, want single most-recent session", page)
	}
	page, err = db.GetSessionStats(ctx, 1, 1)
	if err != nil {
		t.Fatalf("GetSessionStats(offset=1): %v", err)
	}
	if len(page) != 1 || page[0].SessionID != "old" {
		t.Fatalf("got %+v, want single second-most-recent session", page)
	}

	total, err := db.CountSessionStats(ctx)
	if err != nil {
		t.Fatalf("CountSessionStats: %v", err)
	}
	if total != 2 {
		t.Errorf("CountSessionStats = %d, want 2", total)
	}
}

func TestGetSessionStatsSince(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	_ = db.SaveExchange(ctx, Exchange{SessionID: "a", Path: "/p", Timestamp: now, RawRequest: "{}", RawResponse: "{}"})
	firstPage, err := db.GetExchanges(ctx, "", 1, 0)
	if err != nil || len(firstPage) != 1 {
		t.Fatalf("GetExchanges: rows=%+v err=%v", firstPage, err)
	}
	sinceID := firstPage[0].ID

	// Two different sessions each write a new exchange after sinceID,
	// simulating two Claude Code terminals active within the same poll
	// window — both must appear in the delta, not just the one owning the
	// highest exchange id.
	_ = db.SaveExchange(ctx, Exchange{SessionID: "b", Path: "/p", Timestamp: now + 1, RawRequest: "{}", RawResponse: "{}"})
	_ = db.SaveExchange(ctx, Exchange{SessionID: "c", Path: "/p", Timestamp: now + 2, RawRequest: "{}", RawResponse: "{}"})

	delta, err := db.GetSessionStatsSince(ctx, sinceID)
	if err != nil {
		t.Fatalf("GetSessionStatsSince: %v", err)
	}
	if len(delta) != 2 {
		t.Fatalf("got %d delta rows, want 2 (b and c, not a): %+v", len(delta), delta)
	}
	seen := map[string]bool{delta[0].SessionID: true, delta[1].SessionID: true}
	if !seen["b"] || !seen["c"] {
		t.Errorf("expected sessions b and c in delta, got %+v", delta)
	}
}

func TestGetDailyCosts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	if err := db.SaveExchange(ctx, Exchange{
		SessionID: "a", Path: "/p", Timestamp: now, RawRequest: "{}", RawResponse: "{}",
		InputCost: floatPtr(0.01), OutputCost: floatPtr(0.02),
	}); err != nil {
		t.Fatalf("SaveExchange: %v", err)
	}

	daily, err := db.GetDailyCosts(ctx, 60)
	if err != nil {
		t.Fatalf("GetDailyCosts: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("got %d daily buckets, want 1: %+v", len(daily), daily)
	}
	if round4(daily[0].DailyCost) != round4(0.03) {
		t.Errorf("DailyCost = %v, want ~0.03", daily[0].DailyCost)
	}

	// A tight window that excludes the row entirely.
	oldRow := now - float64(120*24*3600)
	if err := db.SaveExchange(ctx, Exchange{
		SessionID: "old", Path: "/p", Timestamp: oldRow, RawRequest: "{}", RawResponse: "{}",
		InputCost: floatPtr(1), OutputCost: floatPtr(1),
	}); err != nil {
		t.Fatalf("SaveExchange(old): %v", err)
	}
	daily, err = db.GetDailyCosts(ctx, 60)
	if err != nil {
		t.Fatalf("GetDailyCosts: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("120-day-old row leaked into a 60-day window: %+v", daily)
	}
}

func TestDeleteExchanges(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	for _, sess := range []string{"a", "a", "b"} {
		if err := db.SaveExchange(ctx, Exchange{SessionID: sess, Path: "/p", Timestamp: now, RawRequest: "{}", RawResponse: "{}"}); err != nil {
			t.Fatalf("SaveExchange: %v", err)
		}
	}

	n, err := db.DeleteExchanges(ctx, "a")
	if err != nil {
		t.Fatalf("DeleteExchanges(a): %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d rows, want 2", n)
	}
	remaining, err := db.GetExchanges(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("GetExchanges: %v", err)
	}
	if len(remaining) != 1 || remaining[0].SessionID != "b" {
		t.Fatalf("unexpected remaining rows: %+v", remaining)
	}

	if _, err = db.DeleteExchanges(ctx, ""); err == nil {
		t.Fatal("DeleteExchanges(\"\"): want error, got nil")
	}
	remaining, err = db.GetExchanges(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("GetExchanges: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("empty session_id should not delete rows: got %d remaining, want 1", len(remaining))
	}
}

func TestSessionActiveWithin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	if active, err := db.SessionActiveWithin(ctx, "unknown", 30*time.Minute, now); err != nil || active {
		t.Fatalf("unknown session: active=%v err=%v, want false, nil", active, err)
	}

	recent := Exchange{SessionID: "s", Path: "/p", Timestamp: float64(now.Add(-5 * time.Minute).Unix()), RawRequest: "{}", RawResponse: "{}"}
	if err := db.SaveExchange(ctx, recent); err != nil {
		t.Fatalf("SaveExchange(recent): %v", err)
	}
	if active, err := db.SessionActiveWithin(ctx, "s", 30*time.Minute, now); err != nil || !active {
		t.Fatalf("recent exchange: active=%v err=%v, want true, nil", active, err)
	}
	if active, err := db.SessionActiveWithin(ctx, "s", 1*time.Minute, now); err != nil || active {
		t.Fatalf("recent exchange outside narrower window: active=%v err=%v, want false, nil", active, err)
	}

	stale := Exchange{SessionID: "old", Path: "/p", Timestamp: float64(now.Add(-2 * time.Hour).Unix()), RawRequest: "{}", RawResponse: "{}"}
	if err := db.SaveExchange(ctx, stale); err != nil {
		t.Fatalf("SaveExchange(stale): %v", err)
	}
	if active, err := db.SessionActiveWithin(ctx, "old", 30*time.Minute, now); err != nil || active {
		t.Fatalf("stale exchange: active=%v err=%v, want false, nil", active, err)
	}

	if active, err := db.SessionActiveWithin(ctx, "", 30*time.Minute, now); err != nil || active {
		t.Fatalf("empty session_id: active=%v err=%v, want false, nil", active, err)
	}
}

// TestConcurrentWrites proves the §4 gotcha is actually handled: many
// goroutines writing at once through the shared connection pool must not
// produce SQLITE_BUSY, since SetMaxOpenConns(1) serializes them.
func TestConcurrentWrites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := db.SaveExchange(ctx, Exchange{
				SessionID: "concurrent", Path: "/p", Timestamp: now + float64(i),
				RawRequest: "{}", RawResponse: "{}",
			})
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("concurrent SaveExchange failed: %v", err)
		}
	}

	rows, err := db.GetExchanges(ctx, `session = "concurrent"`, 100, 0)
	if err != nil {
		t.Fatalf("GetExchanges: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("got %d rows after concurrent writes, want %d", len(rows), n)
	}
}

func round4(f float64) float64 {
	return float64(int(f*10000+0.5)) / 10000
}

// TestOpen_MigratesPreexistingDatabase proves that a database created before
// the cache-token columns existed gets them added via ALTER TABLE on the
// next Open, instead of CREATE TABLE IF NOT EXISTS silently no-op'ing and
// leaving the columns missing.
func TestOpen_MigratesPreexistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build a pre-migration schema by hand (no cache_* columns anywhere),
	// simulating a database created by an older claude-lens binary.
	legacySchema := `
CREATE TABLE exchanges (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id     TEXT    NOT NULL,
    session_name   TEXT,
    path           TEXT    NOT NULL,
    timestamp      REAL    NOT NULL,
    is_streaming   INTEGER NOT NULL DEFAULT 0,
    input_messages TEXT,
    output_text    TEXT,
    input_tokens   INTEGER,
    output_tokens  INTEGER,
    model          TEXT,
    cost           REAL,
    input_cost     REAL,
    output_cost    REAL,
    raw_request    TEXT,
    raw_response   TEXT
);
CREATE TABLE model_prices (
    model_prefix TEXT PRIMARY KEY,
    input_per_m  REAL NOT NULL,
    output_per_m REAL NOT NULL,
    updated_at   REAL NOT NULL
);
INSERT INTO exchanges (session_id, path, timestamp, raw_request, raw_response, input_tokens, output_tokens)
VALUES ('legacy-session', '/p', 1000, '{}', '{}', 10, 20);
INSERT INTO model_prices (model_prefix, input_per_m, output_per_m, updated_at)
VALUES ('legacy-model', 1.0, 2.0, 1000);
`
	setupDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := setupDB.Exec(legacySchema); err != nil {
		t.Fatalf("apply legacy schema: %v", err)
	}
	if err := setupDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open (migration): %v", err)
	}
	defer db.Close()

	// The pre-existing rows must survive the migration untouched, with the
	// new columns present but NULL/zero (no backfill of historical data).
	list, err := db.GetExchanges(context.Background(), `session = "legacy-session"`, 10, 0)
	if err != nil {
		t.Fatalf("GetExchanges after migration: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d legacy exchanges, want 1", len(list))
	}
	if list[0].CacheCreationTokens != nil || list[0].CacheReadTokens != nil {
		t.Errorf("legacy row got backfilled cache tokens, want nil: %+v", list[0])
	}
	if list[0].InputTokens == nil || *list[0].InputTokens != 10 {
		t.Errorf("legacy row's pre-existing input_tokens was lost: %+v", list[0])
	}
	// total_tokens must still reflect the real input/output tokens even
	// though the cache columns are NULL — a naive "col+col+col+col" sum
	// would collapse to NULL here since SQL NULL propagates through '+'.
	if list[0].TotalTokens == nil || *list[0].TotalTokens != 10+20 {
		t.Errorf("legacy row's total_tokens = %v, want %d (10 input + 20 output, NULL cache cols ignored)",
			list[0].TotalTokens, 10+20)
	}

	prices, err := db.ListPrices(context.Background())
	if err != nil {
		t.Fatalf("ListPrices after migration: %v", err)
	}
	found := false
	for _, p := range prices {
		if p.Prefix == "legacy-model" {
			found = true
			if p.CacheWritePerM != 0 || p.CacheReadPerM != 0 {
				t.Errorf("legacy price row got non-zero cache rates, want 0 default: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("legacy-model price row lost during migration")
	}

	// New writes must be able to use the newly added columns.
	if err := db.SaveExchange(context.Background(), Exchange{
		SessionID: "new-session", Path: "/p", Timestamp: 2000,
		RawRequest: "{}", RawResponse: "{}",
		CacheCreationTokens: intPtr(50), CacheReadTokens: intPtr(5),
	}); err != nil {
		t.Fatalf("SaveExchange after migration: %v", err)
	}
	detail, err := db.GetExchanges(context.Background(), `session = "new-session"`, 1, 0)
	if err != nil {
		t.Fatalf("GetExchanges(new-session): %v", err)
	}
	if len(detail) != 1 || detail[0].CacheCreationTokens == nil || *detail[0].CacheCreationTokens != 50 {
		t.Fatalf("post-migration cache token write/read failed: %+v", detail)
	}

	// Running Open (and thus migrateSchema) again must be a no-op, not an
	// error from re-adding an already-present column.
	db2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("re-Open after migration: %v", err)
	}
	db2.Close()
}
