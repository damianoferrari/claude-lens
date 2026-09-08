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

	settings, err := db.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.LiteLLMLastSyncedAt == 0 {
		t.Error("Sync didn't mark LiteLLMLastSyncedAt")
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

func TestRunLoop_SyncsImmediatelyWhenDueThenStopsOnCancel(t *testing.T) {
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
		RunLoop(ctx, db, est, litellm.NewClient(), srv.URL, "")
		close(done)
	}()

	// A fresh DB has LiteLLMLastSyncedAt == 0, so RunLoop's immediate
	// check-and-sync (before it ever waits on pollInterval) must fire
	// right away, without needing to wait out a real interval.
	deadline := time.After(2 * time.Second)
	for hits == 0 {
		select {
		case <-deadline:
			t.Fatal("RunLoop never synced within 2s of starting (a fresh DB should always be immediately due)")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop didn't return after ctx cancel")
	}

	settings, err := db.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.LiteLLMLastSyncedAt == 0 {
		t.Error("RunLoop's sync didn't mark LiteLLMLastSyncedAt")
	}
}

func TestRunLoop_ZeroIntervalNeverSyncs(t *testing.T) {
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
	if err := db.UpdateLiteLLMSyncInterval(context.Background(), 0, 1); err != nil {
		t.Fatalf("UpdateLiteLLMSyncInterval: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, db, est, litellm.NewClient(), srv.URL, "")
		close(done)
	}()

	// Give the immediate check a moment to run (and, correctly, do nothing).
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop didn't return after ctx cancel")
	}

	if hits != 0 {
		t.Errorf("got %d requests to the LiteLLM server, want 0 (sync interval disabled)", hits)
	}
}
