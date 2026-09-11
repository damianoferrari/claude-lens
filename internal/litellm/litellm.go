// Package litellm fetches authoritative per-model pricing from a LiteLLM
// proxy's /model/info endpoint, so claude-lens's own price table can be kept
// in sync with what the proxy actually bills instead of drifting from it.
package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// ModelPrice is one model's USD-per-million-token rates, converted from the
// per-token rates a LiteLLM proxy reports. The four *Above200k fields are
// nil unless the proxy reports that tier for this model — see
// database.Price for how a nil override behaves.
type ModelPrice struct {
	ModelName               string
	InputPerM               float64
	OutputPerM              float64
	CacheWritePerM          float64
	CacheReadPerM           float64
	InputPerMAbove200k      *float64
	OutputPerMAbove200k     *float64
	CacheWritePerMAbove200k *float64
	CacheReadPerMAbove200k  *float64
}

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
func (c *Client) FetchModelPrices(ctx context.Context, baseURL, authToken string) ([]ModelPrice, error) {
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

	prices := make([]ModelPrice, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ModelName == "" {
			continue
		}
		prices = append(prices, ModelPrice{
			ModelName:               m.ModelName,
			InputPerM:               round6(m.ModelInfo.InputCostPerToken * 1_000_000),
			OutputPerM:              round6(m.ModelInfo.OutputCostPerToken * 1_000_000),
			CacheWritePerM:          round6(m.ModelInfo.CacheCreationInputTokenCost * 1_000_000),
			CacheReadPerM:           round6(m.ModelInfo.CacheReadInputTokenCost * 1_000_000),
			InputPerMAbove200k:      perMPtr(m.ModelInfo.InputCostPerTokenAbove200k),
			OutputPerMAbove200k:     perMPtr(m.ModelInfo.OutputCostPerTokenAbove200k),
			CacheWritePerMAbove200k: perMPtr(m.ModelInfo.CacheCreationInputTokenCostAbove200k),
			CacheReadPerMAbove200k:  perMPtr(m.ModelInfo.CacheReadInputTokenCostAbove200k),
		})
	}
	return prices, nil
}

// perMPtr converts a per-token rate to a per-million-token rate, preserving
// nil (no override reported) instead of defaulting it to zero.
func perMPtr(perToken *float64) *float64 {
	if perToken == nil {
		return nil
	}
	perM := round6(*perToken * 1_000_000)
	return &perM
}

// round6 rounds to 6 decimal places, clearing the float64 noise a per-token
// rate picks up when scaled by 1,000,000 (e.g. 0.0000033*1e6 rendering as
// 3.3000000000000003 instead of 3.3).
func round6(f float64) float64 {
	const mult = 1e6
	return math.Round(f*mult) / mult
}
