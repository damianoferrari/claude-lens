// Package litellm fetches authoritative per-model pricing from a LiteLLM
// proxy's /model/info endpoint, so claude-lens's own price table can be kept
// in sync with what the proxy actually bills instead of drifting from it.
package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lfsc09/claude-lens/internal/priceprovider"
)

// Client fetches model pricing from a LiteLLM proxy with a bounded timeout.
type Client struct {
	http *http.Client
}

// NewClient builds a Client with a 10s request timeout — generous for an
// admin-triggered, not-on-the-hot-path call.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 10 * time.Second}}
}

type modelInfoResponse struct {
	Data []struct {
		ModelName string `json:"model_name"`
		ModelInfo struct {
			InputCostPerToken                    float64  `json:"input_cost_per_token"`
			OutputCostPerToken                   float64  `json:"output_cost_per_token"`
			CacheCreationInputTokenCost          float64  `json:"cache_creation_input_token_cost"`
			CacheReadInputTokenCost              float64  `json:"cache_read_input_token_cost"`
			InputCostPerTokenAbove200k           *float64 `json:"input_cost_per_token_above_200k_tokens"`
			OutputCostPerTokenAbove200k          *float64 `json:"output_cost_per_token_above_200k_tokens"`
			CacheCreationInputTokenCostAbove200k *float64 `json:"cache_creation_input_token_cost_above_200k_tokens"`
			CacheReadInputTokenCostAbove200k     *float64 `json:"cache_read_input_token_cost_above_200k_tokens"`
		} `json:"model_info"`
	} `json:"data"`
}

// FetchModelPrices queries baseURL's /model/info endpoint (a LiteLLM proxy
// convention, not part of the Anthropic API) and returns per-million-token
// rates for every model it reports. Only a LiteLLM upstream serves this
// route — pointing claude-lens at api.anthropic.com directly, or any other
// non-LiteLLM upstream, surfaces as the status-code error below.
func (c *Client) FetchModelPrices(ctx context.Context, baseURL, authToken string) ([]priceprovider.ModelPrice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/model/info", nil)
	if err != nil {
		return nil, fmt.Errorf("build model/info request: %w", err)
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s/model/info: %w", baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/model/info returned status %d — is this upstream a LiteLLM proxy?", baseURL, resp.StatusCode)
	}

	var parsed modelInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode model/info response: %w", err)
	}

	prices := make([]priceprovider.ModelPrice, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ModelName == "" {
			continue
		}
		prices = append(prices, priceprovider.ModelPrice{
			ModelName:               m.ModelName,
			InputPerM:               priceprovider.Round6(m.ModelInfo.InputCostPerToken * 1_000_000),
			OutputPerM:              priceprovider.Round6(m.ModelInfo.OutputCostPerToken * 1_000_000),
			CacheWritePerM:          priceprovider.Round6(m.ModelInfo.CacheCreationInputTokenCost * 1_000_000),
			CacheReadPerM:           priceprovider.Round6(m.ModelInfo.CacheReadInputTokenCost * 1_000_000),
			InputPerMAbove200k:      priceprovider.PerMPtr(m.ModelInfo.InputCostPerTokenAbove200k),
			OutputPerMAbove200k:     priceprovider.PerMPtr(m.ModelInfo.OutputCostPerTokenAbove200k),
			CacheWritePerMAbove200k: priceprovider.PerMPtr(m.ModelInfo.CacheCreationInputTokenCostAbove200k),
			CacheReadPerMAbove200k:  priceprovider.PerMPtr(m.ModelInfo.CacheReadInputTokenCostAbove200k),
		})
	}
	return prices, nil
}
