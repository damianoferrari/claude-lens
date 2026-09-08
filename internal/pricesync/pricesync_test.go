package pricesync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
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

	_, err = Sync(context.Background(), db, est, litellm.NewClient(), notLiteLLM.URL, "")
	if err == nil {
		t.Fatal("expected an error for a non-LiteLLM upstream")
	}
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("got %v, want an error wrapping ErrUpstreamUnavailable (the admin API and UI key off this)", err)
	}

	after, err := db.ListPrices(context.Background())
	if err != nil {
		t.Fatalf("ListPrices: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("prices changed on a failed sync: before=%d after=%d", len(before), len(after))
	}

	settings, err := db.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.LiteLLMLastSyncError == "" {
		t.Error("Sync's failure wasn't recorded in LiteLLMLastSyncError")
	}
}

func TestSync_SuccessClearsAPriorFailure(t *testing.T) {
	notLiteLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notLiteLLM.Close()
	litellmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer litellmSrv.Close()

	db := openTestDB(t)
	est := pricing.New(db)
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if _, err := Sync(context.Background(), db, est, litellm.NewClient(), notLiteLLM.URL, ""); err == nil {
		t.Fatal("expected the first sync (against a non-LiteLLM upstream) to fail")
	}
	if _, err := Sync(context.Background(), db, est, litellm.NewClient(), litellmSrv.URL, ""); err != nil {
		t.Fatalf("expected the second sync (against a real LiteLLM upstream) to succeed: %v", err)
	}

	settings, err := db.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.LiteLLMLastSyncError != "" {
		t.Errorf("LiteLLMLastSyncError = %q, want cleared after the subsequent successful sync", settings.LiteLLMLastSyncError)
	}
}

func TestSync_NonFetchFailureIsNotErrUpstreamUnavailable(t *testing.T) {
	litellmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"model_name":"m","model_info":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}]}`))
	}))
	defer litellmSrv.Close()

	db := openTestDB(t)
	est := pricing.New(db)
	if err := est.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	db.Close() // fetch succeeds, but the subsequent upsert now fails — a DB error, not a reachability one.

	_, err := Sync(context.Background(), db, est, litellm.NewClient(), litellmSrv.URL, "")
	if err == nil {
		t.Fatal("expected an error once the DB is closed")
	}
	if errors.Is(err, ErrUpstreamUnavailable) {
		t.Errorf("got %v wrapping ErrUpstreamUnavailable, want it reserved for fetch failures only — "+
			"the admin API maps ErrUpstreamUnavailable to 502 specifically so the UI greys out its sync "+
			"button, which a mere DB error shouldn't trigger", err)
	}
}

func TestRunLoop_SyncsImmediatelyWhenDueThenStopsOnCancel(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
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
	// right away, without needing to wait out a real interval. hits is
	// written from the httptest server's own goroutine while this loop
	// polls it from the test goroutine, hence the atomic rather than a
	// plain int.
	deadline := time.After(2 * time.Second)
	for hits.Load() == 0 {
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
