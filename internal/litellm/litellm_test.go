package litellm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchModelPrices_ConvertsPerTokenToPerMillion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer sk-test")
		}
		if r.URL.Path != "/model/info" {
			t.Errorf("path = %q, want /model/info", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"model_name":"claude-sonnet-5","model_info":{
			"input_cost_per_token":0.0000022,
			"output_cost_per_token":0.000011,
			"cache_creation_input_token_cost":0.00000275,
			"cache_read_input_token_cost":0.00000022
		}}]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("got %d prices, want 1", len(prices))
	}

	p := prices[0]
	if p.ModelName != "claude-sonnet-5" {
		t.Errorf("ModelName = %q, want claude-sonnet-5", p.ModelName)
	}
	if p.InputPerM != 2.2 || p.OutputPerM != 11 || p.CacheWritePerM != 2.75 || p.CacheReadPerM != 0.22 {
		t.Errorf("got %+v, want (2.2, 11, 2.75, 0.22)", p)
	}
}

func TestFetchModelPrices_ConvertsAbove200kTiers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[
			{"model_name":"claude-tiered","model_info":{
				"input_cost_per_token":0.000001,
				"output_cost_per_token":0.000002,
				"input_cost_per_token_above_200k_tokens":0.000002,
				"output_cost_per_token_above_200k_tokens":0.000004,
				"cache_creation_input_token_cost_above_200k_tokens":0.0000025,
				"cache_read_input_token_cost_above_200k_tokens":0.0000002
			}},
			{"model_name":"claude-no-tiers","model_info":{
				"input_cost_per_token":0.000001,
				"output_cost_per_token":0.000002
			}}
		]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("got %d prices, want 2", len(prices))
	}

	tiered := prices[0]
	if tiered.InputPerMAbove200k == nil || *tiered.InputPerMAbove200k != 2.0 {
		t.Errorf("InputPerMAbove200k = %v, want 2.0", tiered.InputPerMAbove200k)
	}
	if tiered.OutputPerMAbove200k == nil || *tiered.OutputPerMAbove200k != 4.0 {
		t.Errorf("OutputPerMAbove200k = %v, want 4.0", tiered.OutputPerMAbove200k)
	}
	if tiered.CacheWritePerMAbove200k == nil || *tiered.CacheWritePerMAbove200k != 2.5 {
		t.Errorf("CacheWritePerMAbove200k = %v, want 2.5", tiered.CacheWritePerMAbove200k)
	}
	if tiered.CacheReadPerMAbove200k == nil || *tiered.CacheReadPerMAbove200k != 0.2 {
		t.Errorf("CacheReadPerMAbove200k = %v, want 0.2", tiered.CacheReadPerMAbove200k)
	}

	noTiers := prices[1]
	if noTiers.InputPerMAbove200k != nil || noTiers.OutputPerMAbove200k != nil ||
		noTiers.CacheWritePerMAbove200k != nil || noTiers.CacheReadPerMAbove200k != nil {
		t.Errorf("got %+v, want all above-200k fields nil when the upstream doesn't report them", noTiers)
	}
}

func TestFetchModelPrices_SkipsEntriesWithoutModelName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"model_name":"","model_info":{"input_cost_per_token":1}}]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("got %d prices, want 0 for an entry with no model_name", len(prices))
	}
}

func TestFetchModelPrices_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient()
	if _, err := c.FetchModelPrices(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected an error for a 404 response (e.g. a non-LiteLLM upstream)")
	}
}
