package pricesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lfsc09/claude-lens/internal/database"
	"github.com/lfsc09/claude-lens/internal/litellm"
	"github.com/lfsc09/claude-lens/internal/pricing"
)

func openTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSync_UpsertsAndRefreshesEstimator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"model_name":"brand-new-model","model_info":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}]}`))
	}))
	defer srv.Close()

	db := openTestDB(t)
	est := pricing.New(db)
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	result, err := Sync(context.Background(), db, est, litellm.NewClient(), srv.URL, "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Created != 1 || len(result.Models) != 1 {
		t.Errorf("got %+v, want 1 created model", result)
	}

	// The estimator must already reflect the new price without a separate
	// Refresh call — Sync is responsible for that.
	if _, ok := est.EstimateCosts("brand-new-model", 1_000_000, 0, 0, 0); !ok {
		t.Error("estimator wasn't refreshed after Sync")
	}
}

func TestSync_NonLiteLLMUpstreamReturnsErrorWithoutSideEffects(t *testing.T) {
	notLiteLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notLiteLLM.Close()

	db := openTestDB(t)
	est := pricing.New(db)
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	before, err := db.ListPrices(context.Background())
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}

	if _, err := Sync(context.Background(), db, est, litellm.NewClient(), notLiteLLM.URL, ""); err == nil {
		t.Fatal("expected an error for a non-LiteLLM upstream")
	}

	after, err := db.ListPrices(context.Background())
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("prices changed on a failed sync: before=%d after=%d", len(before), len(after))
	}
}

func TestRunLoop_FiresOnTickerAndStopsOnCancel(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	db := openTestDB(t)
	est := pricing.New(db)
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, db, est, litellm.NewClient(), srv.URL, "", 10*time.Millisecond)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for hits == 0 {
		select {
		case <-deadline:
			t.Fatal("RunLoop never hit the server within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop didn't return after ctx cancel")
	}
}

func TestRunLoop_NonPositiveIntervalIsNoop(t *testing.T) {
	db := openTestDB(t)
	est := pricing.New(db)
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	done := make(chan struct{})
	go func() {
		// A URL that would error if ever called — proves RunLoop returns
		// immediately for interval <= 0 without attempting a sync.
		RunLoop(context.Background(), db, est, litellm.NewClient(), "http://127.0.0.1:0", "", 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("RunLoop with a zero interval didn't return immediately")
	}
}
