package bifrost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchModelPrices_ConvertsPerTokenToPerMillion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-bf-test" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer sk-bf-test")
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.URL.Query().Get("provider"); got != "bedrock" {
			t.Errorf("provider query param = %q, want bedrock", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"bedrock/us.anthropic.claude-sonnet-5","pricing":{
			"prompt":"0.0000022",
			"completion":"0.000011",
			"input_cache_write":"0.00000275",
			"input_cache_read":"0.00000022"
		}}]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "sk-bf-test")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("got %d prices, want 1", len(prices))
	}

	p := prices[0]
	if p.ModelName != "claude-sonnet-5" {
		t.Errorf("ModelName = %q, want claude-sonnet-5 (stripped of the bedrock/us.anthropic. prefix)", p.ModelName)
	}
	if p.InputPerM != 2.2 || p.OutputPerM != 11 || p.CacheWritePerM != 2.75 || p.CacheReadPerM != 0.22 {
		t.Errorf("got %+v, want (2.2, 11, 2.75, 0.22)", p)
	}
}

func TestFetchModelPrices_StripsBedrockVersionSuffix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"bedrock/us.anthropic.claude-haiku-4-5-20251001-v1:0","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("got %d prices, want 1", len(prices))
	}
	if prices[0].ModelName != "claude-haiku-4-5-20251001" {
		t.Errorf("ModelName = %q, want claude-haiku-4-5-20251001 (stripped of the -v1:0 Bedrock version suffix)", prices[0].ModelName)
	}
}

func TestFetchModelPrices_SkipsNonBedrockAnthropicModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[
			{"id":"openai/gpt-4o","pricing":{"prompt":"0.000001","completion":"0.000002"}},
			{"id":"anthropic/claude-sonnet-5","pricing":{"prompt":"0.000001","completion":"0.000002"}},
			{"id":"bedrock/us.anthropic.claude-sonnet-5","pricing":{"prompt":"0.000001","completion":"0.000002"}}
		]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 1 || prices[0].ModelName != "claude-sonnet-5" {
		t.Errorf("got %+v, want exactly the one bedrock/us.anthropic.* entry", prices)
	}
}

func TestFetchModelPrices_EmptyPricingFieldIsZeroNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A model with no caching support reports no input_cache_* fields at
		// all — must come back as 0, not fail the whole sync.
		w.Write([]byte(`{"data":[{"id":"bedrock/us.anthropic.claude-sonnet-5","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("got %d prices, want 1", len(prices))
	}
	if prices[0].CacheWritePerM != 0 || prices[0].CacheReadPerM != 0 {
		t.Errorf("got %+v, want cache rates 0 for a model that doesn't report them", prices[0])
	}
}

func TestFetchModelPrices_MalformedPricingFieldIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"bedrock/us.anthropic.claude-sonnet-5","pricing":{"prompt":"not-a-number","completion":"0.000002"}}]}`))
	}))
	defer srv.Close()

	c := NewClient()
	if _, err := c.FetchModelPrices(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected an error for a malformed pricing field")
	}
}

func TestFetchModelPrices_Above200kFieldsAlwaysNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"bedrock/us.anthropic.claude-sonnet-5","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`))
	}))
	defer srv.Close()

	c := NewClient()
	prices, err := c.FetchModelPrices(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("got %d prices, want 1", len(prices))
	}

	p := prices[0]
	if p.InputPerMAbove200k != nil || p.OutputPerMAbove200k != nil ||
		p.CacheWritePerMAbove200k != nil || p.CacheReadPerMAbove200k != nil {
		t.Errorf("got %+v, want all above-200k fields nil — Bifrost's /v1/models doesn't report that tier", p)
	}
}

func TestFetchModelPrices_StripsInferenceRoutePrefixFromBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// claude-lens's proxy forwards live traffic under a path prefix
		// (e.g. ".../anthropic"), but Bifrost's /v1/models lives at the
		// host root — the request must land here, not under /anthropic.
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models (the /anthropic prefix must be stripped)", r.URL.Path)
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient()
	if _, err := c.FetchModelPrices(context.Background(), srv.URL+"/anthropic", ""); err != nil {
		t.Fatalf("FetchModelPrices: %v", err)
	}
}

func TestFetchModelPrices_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient()
	if _, err := c.FetchModelPrices(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected an error for a 404 response (e.g. a non-Bifrost upstream)")
	}
}
