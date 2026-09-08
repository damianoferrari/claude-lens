package database

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lfsc09/claude-lens/internal/notify"
)

// newAlertServer spins up a test HTTP server standing in for a Slack
// incoming webhook, counting how many requests it receives.
func newAlertServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, &count
}

// waitForCount polls got until it reaches want, or fails the test after 2s
// — accrueLimiterCost dispatches alerts from a background goroutine.
func waitForCount(t *testing.T, got *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.Load() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for alert count = %d, got %d", want, got.Load())
}

// assertNoMoreAlerts waits briefly and confirms got hasn't grown past want,
// for cases where there's nothing to poll for arriving.
func assertNoMoreAlerts(t *testing.T, got *atomic.Int32, want int32) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if n := got.Load(); n != want {
		t.Fatalf("alert count = %d, want %d (no further alerts)", n, want)
	}
}

func TestAccrueLimiterCost_BudgetAlertFiresOnceOnThresholdCross(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	server, count := newAlertServer(t)
	db.SetNotifications(notify.NewClient())

	id, err := db.CreateLimiter(ctx, Limiter{
		SessionID:         "sess_alert",
		LimitAmount:       1.0,
		RefreshValue:      60,
		RefreshUnit:       "minutes",
		NextRefreshAt:     float64(now.Add(time.Hour).Unix()),
		IsActive:          true,
		CreatedAt:         float64(now.Unix()),
		UpdatedAt:         float64(now.Unix()),
		AlertThresholdPct: intPtr(80),
		SlackWebhookURL:   server.URL,
	})
	if err != nil {
		t.Fatalf("CreateLimiter: %v", err)
	}

	// Below the 80% threshold: no alert yet.
	saveExchangeWithCost(t, db, "sess_alert", 0.79)
	assertNoMoreAlerts(t, count, 0)

	// Crosses 80% of the $1.00 budget: exactly one alert fires.
	saveExchangeWithCost(t, db, "sess_alert", 0.05)
	waitForCount(t, count, 1)

	got, err := db.GetLimiter(ctx, id)
	if err != nil {
		t.Fatalf("GetLimiter: %v", err)
	}
	if !got.AlertSent {
		t.Error("AlertSent = false after crossing threshold, want true")
	}

	// Further accrual within the same window doesn't resend it.
	saveExchangeWithCost(t, db, "sess_alert", 0.05)
	assertNoMoreAlerts(t, count, 1)
}

// TestAccrueLimiterCost_BudgetAlertResetsOnRefresh confirms AlertSent is
// cleared by refreshIfDue, so a limiter that crosses its threshold again in
// a later window gets a fresh alert instead of staying silenced forever.
func TestAccrueLimiterCost_BudgetAlertResetsOnRefresh(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	server, count := newAlertServer(t)
	db.SetNotifications(notify.NewClient())

	id, err := db.CreateLimiter(ctx, Limiter{
		SessionID:         "sess_reset",
		LimitAmount:       1.0,
		RefreshValue:      60,
		RefreshUnit:       "minutes",
		NextRefreshAt:     float64(now.Add(time.Hour).Unix()),
		IsActive:          true,
		CreatedAt:         float64(now.Unix()),
		UpdatedAt:         float64(now.Unix()),
		AlertThresholdPct: intPtr(50),
		SlackWebhookURL:   server.URL,
	})
	if err != nil {
		t.Fatalf("CreateLimiter: %v", err)
	}

	// First window: crosses 50% and alerts once.
	saveExchangeWithCost(t, db, "sess_reset", 0.60)
	waitForCount(t, count, 1)

	got, err := db.GetLimiter(ctx, id)
	if err != nil {
		t.Fatalf("GetLimiter: %v", err)
	}
	if !got.AlertSent {
		t.Fatal("AlertSent = false after crossing threshold, want true")
	}

	// Force the window due again (as if its refresh boundary had passed)
	// without touching AlertSent — that's refreshIfDue's job, not ours.
	got.NextRefreshAt = float64(time.Now().Add(-time.Minute).Unix())
	if err := db.UpdateLimiter(ctx, *got); err != nil {
		t.Fatalf("UpdateLimiter: %v", err)
	}

	// Re-crossing the threshold in the new window fires a second,
	// independent alert.
	saveExchangeWithCost(t, db, "sess_reset", 0.60)
	waitForCount(t, count, 2)

	final, err := db.GetLimiter(ctx, id)
	if err != nil {
		t.Fatalf("GetLimiter: %v", err)
	}
	if !final.AlertSent {
		t.Error("AlertSent = false after re-crossing threshold in the new window, want true")
	}
	if final.CurrentCost != 0.60 {
		t.Errorf("CurrentCost = %v, want 0.60 (reset by refresh, then accrued fresh)", final.CurrentCost)
	}
}

func TestAccrueLimiterCost_NoAlertWhenThresholdUnset(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	server, count := newAlertServer(t)
	db.SetNotifications(notify.NewClient())

	if _, err := db.CreateLimiter(ctx, Limiter{
		SessionID:       "sess_no_alert",
		LimitAmount:     1.0,
		RefreshValue:    60,
		RefreshUnit:     "minutes",
		NextRefreshAt:   float64(now.Add(time.Hour).Unix()),
		IsActive:        true,
		CreatedAt:       float64(now.Unix()),
		UpdatedAt:       float64(now.Unix()),
		SlackWebhookURL: server.URL,
		// AlertThresholdPct/AlertRequestCostUSD left nil: alerting disabled.
	}); err != nil {
		t.Fatalf("CreateLimiter: %v", err)
	}

	saveExchangeWithCost(t, db, "sess_no_alert", 1.0)
	assertNoMoreAlerts(t, count, 0)
}

// TestAccrueLimiterCost_RequestSpikeAlertFiresOverThreshold confirms a
// single exchange costing at least AlertRequestCostUSD fires an alert, with
// no "already sent" gating — unlike the budget alert, this is expected to
// fire on every qualifying request.
func TestAccrueLimiterCost_RequestSpikeAlertFiresOverThreshold(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	server, count := newAlertServer(t)
	db.SetNotifications(notify.NewClient())

	if _, err := db.CreateLimiter(ctx, Limiter{
		SessionID:           "sess_spike",
		LimitAmount:         100.0,
		RefreshValue:        60,
		RefreshUnit:         "minutes",
		NextRefreshAt:       float64(now.Add(time.Hour).Unix()),
		IsActive:            true,
		CreatedAt:           float64(now.Unix()),
		UpdatedAt:           float64(now.Unix()),
		AlertRequestCostUSD: floatPtr(1.0),
		SlackWebhookURL:     server.URL,
	}); err != nil {
		t.Fatalf("CreateLimiter: %v", err)
	}

	// Below the per-request threshold: no alert.
	saveExchangeWithCost(t, db, "sess_spike", 0.50)
	assertNoMoreAlerts(t, count, 0)

	// Meets the threshold: fires.
	saveExchangeWithCost(t, db, "sess_spike", 1.0)
	waitForCount(t, count, 1)

	// Fires again on a second qualifying request in the same window — no
	// "already sent" gating like the budget alert has.
	saveExchangeWithCost(t, db, "sess_spike", 2.0)
	waitForCount(t, count, 2)
}

// TestAccrueLimiterCost_NoRequestSpikeAlertWhenUnsetOrInactive confirms the
// request-spike alert respects both AlertRequestCostUSD being unset and the
// same is-active/in-window gate the budget alert uses.
func TestAccrueLimiterCost_NoRequestSpikeAlertWhenUnsetOrInactive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	server, count := newAlertServer(t)
	db.SetNotifications(notify.NewClient())

	if _, err := db.CreateLimiter(ctx, Limiter{
		SessionID:       "sess_spike_unset",
		LimitAmount:     100.0,
		RefreshValue:    60,
		RefreshUnit:     "minutes",
		NextRefreshAt:   float64(now.Add(time.Hour).Unix()),
		IsActive:        true,
		CreatedAt:       float64(now.Unix()),
		UpdatedAt:       float64(now.Unix()),
		SlackWebhookURL: server.URL,
		// AlertRequestCostUSD left nil.
	}); err != nil {
		t.Fatalf("CreateLimiter: %v", err)
	}
	saveExchangeWithCost(t, db, "sess_spike_unset", 50.0)
	assertNoMoreAlerts(t, count, 0)

	if _, err := db.CreateLimiter(ctx, Limiter{
		SessionID:           "sess_spike_inactive",
		LimitAmount:         100.0,
		RefreshValue:        60,
		RefreshUnit:         "minutes",
		NextRefreshAt:       float64(now.Add(time.Hour).Unix()),
		IsActive:            false,
		CreatedAt:           float64(now.Unix()),
		UpdatedAt:           float64(now.Unix()),
		AlertRequestCostUSD: floatPtr(1.0),
		SlackWebhookURL:     server.URL,
	}); err != nil {
		t.Fatalf("CreateLimiter: %v", err)
	}
	saveExchangeWithCost(t, db, "sess_spike_inactive", 50.0)
	assertNoMoreAlerts(t, count, 0)
}

// TestAccrueLimiterCost_ConcurrentCrossingsSendExactlyOneBudgetAlert drives
// accrueLimiterCost from many goroutines whose combined cost crosses the
// budget threshold, and asserts exactly one Slack alert is sent — guarding
// against the read-check-write race on AlertSent. Run with -race.
func TestAccrueLimiterCost_ConcurrentCrossingsSendExactlyOneBudgetAlert(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	server, count := newAlertServer(t)
	db.SetNotifications(notify.NewClient())

	if _, err := db.CreateLimiter(ctx, Limiter{
		SessionID:         "sess_concurrent",
		LimitAmount:       1.0,
		RefreshValue:      60,
		RefreshUnit:       "minutes",
		NextRefreshAt:     float64(now.Add(time.Hour).Unix()),
		IsActive:          true,
		CreatedAt:         float64(now.Unix()),
		UpdatedAt:         float64(now.Unix()),
		AlertThresholdPct: intPtr(50),
		SlackWebhookURL:   server.URL,
	}); err != nil {
		t.Fatalf("CreateLimiter: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	var failures atomic.Int32
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			// Each accrual alone crosses the 50% threshold of the $1.00
			// budget, so every one of them independently observes the
			// crossing if the read-check-write isn't serialized.
			if err := db.SaveExchange(ctx, Exchange{
				SessionID:   "sess_concurrent",
				Path:        "/v1/messages",
				Timestamp:   float64(time.Now().Unix()),
				RawRequest:  "{}",
				RawResponse: "{}",
				InputCost:   floatPtr(0.60),
			}); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("SaveExchange failed %d/%d times", n, goroutines)
	}

	waitForCount(t, count, 1)
	assertNoMoreAlerts(t, count, 1)
}
