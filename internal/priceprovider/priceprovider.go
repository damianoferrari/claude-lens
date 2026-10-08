// Package priceprovider defines the shared contract that every upstream
// price-sync client (internal/litellm, internal/bifrost) implements, so
// internal/pricesync can sync from whichever one is configured without
// depending on either concretely.
package priceprovider

import (
	"context"
	"math"
)

// ModelPrice is one model's USD-per-million-token rates, converted from the
// per-token rates an upstream reports. The four *Above200k fields are nil
// unless the upstream reports that tier for this model — see
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

// Fetcher fetches per-model pricing from an upstream gateway's own API.
type Fetcher interface {
	FetchModelPrices(ctx context.Context, baseURL, authToken string) ([]ModelPrice, error)
}

// Round6 rounds to 6 decimal places, clearing the float64 noise a per-token
// rate picks up when scaled by 1,000,000 (e.g. 0.0000033*1e6 rendering as
// 3.3000000000000003 instead of 3.3).
func Round6(f float64) float64 {
	const mult = 1e6
	return math.Round(f*mult) / mult
}

// PerMPtr converts a per-token rate to a per-million-token rate, preserving
// nil (no override reported) instead of defaulting it to zero.
func PerMPtr(perToken *float64) *float64 {
	if perToken == nil {
		return nil
	}
	perM := Round6(*perToken * 1_000_000)
	return &perM
}
