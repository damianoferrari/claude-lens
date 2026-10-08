// Package bifrost fetches authoritative per-model pricing from a Bifrost
// gateway's /v1/models endpoint, so claude-lens's own price table can be
// kept in sync with what the gateway actually bills instead of drifting
// from it.
//
// Bifrost's dedicated management-API pricing endpoint
// (/api/models/details) requires a management API key, which this
// deployment doesn't issue — only the virtual key already used for live
// inference traffic (CLENS_PROXY_AUTH_TOKEN) is available. /v1/models is
// the OpenRouter-compatible model-catalog route Bifrost exposes to that
// same virtual key, and it carries pricing too, so this package uses that
// instead.
package bifrost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lfsc09/claude-lens/internal/priceprovider"
)

// Client fetches model pricing from a Bifrost gateway with a bounded
// timeout.
type Client struct {
	http *http.Client
}

// NewClient builds a Client with a 10s request timeout — generous for an
// admin-triggered, not-on-the-hot-path call.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 10 * time.Second}}
}

type listModelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			Prompt          string `json:"prompt"`
			Completion      string `json:"completion"`
			InputCacheWrite string `json:"input_cache_write"`
			InputCacheRead  string `json:"input_cache_read"`
		} `json:"pricing"`
	} `json:"data"`
}

// bedrockAnthropicPrefix and bedrockVersionSuffix convert a Bifrost model
// id back to the plain Claude model name Claude Code actually sends (and
// every exchange's model column stores) — e.g.
// "bedrock/us.anthropic.claude-sonnet-5" -> "claude-sonnet-5",
// "bedrock/us.anthropic.claude-haiku-4-5-20251001-v1:0" ->
// "claude-haiku-4-5-20251001". Models under any other provider prefix are
// skipped entirely: claude-lens only ever proxies Anthropic-shaped Claude
// Code traffic, so a non-Bedrock-Anthropic entry can never match a real
// exchange's model name regardless of its own pricing.
const (
	bedrockAnthropicPrefix = "bedrock/us.anthropic."
	bedrockVersionSuffix   = "-v1:0"
)

// claudeModelName returns id's plain Claude model name and true, or ""
// and false if id isn't a Bedrock-hosted Anthropic model.
func claudeModelName(id string) (string, bool) {
	name, ok := strings.CutPrefix(id, bedrockAnthropicPrefix)
	if !ok {
		return "", false
	}
	return strings.TrimSuffix(name, bedrockVersionSuffix), true
}

// FetchModelPrices queries baseURL's host's /v1/models endpoint, filtered
// to provider=bedrock, and returns per-million-token rates for every
// Bedrock-hosted Claude model it reports — see claudeModelName for why
// only those are kept. Only a Bifrost upstream serves this route —
// pointing claude-lens at api.anthropic.com directly, or any other
// non-Bifrost upstream, surfaces as the status-code error below.
//
// baseURL may carry a path — e.g. ".../anthropic", the inference-route
// prefix claude-lens's reverse proxy forwards live traffic under — which
// this strips: Bifrost's /v1/models lives at the host root, a sibling of
// that inference prefix, not nested under it.
//
// /v1/models' pricing fields are decimal strings (an OpenRouter-API
// convention Bifrost mirrors here), not JSON numbers like LiteLLM's
// /model/info — hence the explicit strconv.ParseFloat below. It also
// doesn't report an above-200k long-context tier, so the four
// *Above200k fields on every returned priceprovider.ModelPrice are always
// nil — database.UpsertPriceFromSync treats a nil override as "keep
// whatever's already configured" rather than clearing it.
func (c *Client) FetchModelPrices(ctx context.Context, baseURL, authToken string) ([]priceprovider.ModelPrice, error) {
	hostRoot, err := hostRootURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL %q: %w", baseURL, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hostRoot+"/v1/models?provider=bedrock&page_size=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("build v1/models request: %w", err)
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s/v1/models: %w", hostRoot, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/v1/models returned status %d — is this upstream a Bifrost gateway?", hostRoot, resp.StatusCode)
	}

	var parsed listModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode v1/models response: %w", err)
	}

	prices := make([]priceprovider.ModelPrice, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		modelName, ok := claudeModelName(m.ID)
		if !ok {
			continue
		}

		inputPerToken, err := parsePerTokenRate(m.Pricing.Prompt)
		if err != nil {
			return nil, fmt.Errorf("%s: parse prompt price: %w", m.ID, err)
		}
		outputPerToken, err := parsePerTokenRate(m.Pricing.Completion)
		if err != nil {
			return nil, fmt.Errorf("%s: parse completion price: %w", m.ID, err)
		}
		cacheWritePerToken, err := parsePerTokenRate(m.Pricing.InputCacheWrite)
		if err != nil {
			return nil, fmt.Errorf("%s: parse input_cache_write price: %w", m.ID, err)
		}
		cacheReadPerToken, err := parsePerTokenRate(m.Pricing.InputCacheRead)
		if err != nil {
			return nil, fmt.Errorf("%s: parse input_cache_read price: %w", m.ID, err)
		}

		prices = append(prices, priceprovider.ModelPrice{
			ModelName:      modelName,
			InputPerM:      priceprovider.Round6(inputPerToken * 1_000_000),
			OutputPerM:     priceprovider.Round6(outputPerToken * 1_000_000),
			CacheWritePerM: priceprovider.Round6(cacheWritePerToken * 1_000_000),
			CacheReadPerM:  priceprovider.Round6(cacheReadPerToken * 1_000_000),
		})
	}
	return prices, nil
}

// parsePerTokenRate parses a Bifrost pricing field's decimal-string USD
// per-token rate, treating "" (a dimension the model doesn't bill for,
// e.g. no cache support) as 0 rather than an error.
func parsePerTokenRate(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// hostRootURL reduces rawURL to just its scheme and host, discarding any
// path (and any query/fragment) — Bifrost's /v1/models lives at the
// gateway's host root regardless of what inference-route prefix the
// proxy's own baseURL carries.
func hostRootURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String(), nil
}
