package database

import (
	"context"
	"database/sql"
	"time"
)

// Price is a row of the model_prices table: USD per million tokens for a
// model prefix under a given token-count rule, matched against the full
// model string returned by the API. A prefix can own several rule rows —
// see internal/pricing for how the "closest rule_tokens wins" resolution
// picks among them when more than one matches a call's prompt size.
//
// CacheWritePerM/CacheReadPerM price Anthropic's prompt-caching tokens
// (cache_creation_input_tokens / cache_read_input_tokens), which are billed
// at different rates than plain input tokens — a write is pricier, a read
// much cheaper.
type Price struct {
	ID             int64   `json:"id"`
	Prefix         string  `json:"model_prefix"`
	Rule           string  `json:"rule"`        // "over" (exclusive) | "under" (inclusive)
	RuleTokens     int64   `json:"rule_tokens"` // token offset the rule is relative to
	InputPerM      float64 `json:"input_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	CacheWritePerM float64 `json:"cache_write_per_m"`
	CacheReadPerM  float64 `json:"cache_read_per_m"`
	CreatedAt      float64 `json:"created_at"`
	UpdatedAt      float64 `json:"updated_at"`
}

const priceColumns = `id, model_prefix, rule, rule_tokens, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m, created_at, updated_at`

// defaultPrices seeds model_prices on first run. Cache write/read rates
// follow Anthropic's standard multipliers (1.25x input for a 5-minute-TTL
// cache write, 0.1x input for a cache read). All seeded as "over 0" so each
// catches every call for its prefix regardless of prompt size.
var defaultPrices = []Price{
	{Prefix: "claude-opus-5", Rule: "over", RuleTokens: 0, InputPerM: 5.00, OutputPerM: 25.00, CacheWritePerM: 6.25, CacheReadPerM: 0.50},
	{Prefix: "claude-sonnet-5", Rule: "over", RuleTokens: 0, InputPerM: 3.00, OutputPerM: 15.00, CacheWritePerM: 3.75, CacheReadPerM: 0.30},
	{Prefix: "claude-haiku-4-5", Rule: "over", RuleTokens: 0, InputPerM: 1.00, OutputPerM: 5.00, CacheWritePerM: 1.25, CacheReadPerM: 0.10},
}

func (db *DB) seedDefaultPrices(ctx context.Context) error {
	var count int
	if err := db.sql.QueryRowContext(ctx, "SELECT COUNT(*) FROM model_prices").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	now := float64(time.Now().Unix())
	for _, p := range defaultPrices {
		p.CreatedAt, p.UpdatedAt = now, now
		if _, err := db.CreatePrice(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// ListPrices returns all model price rows, ordered by prefix and then
// creation order (oldest rule first) so callers can group by prefix and
// walk each group in insertion order.
func (db *DB) ListPrices(ctx context.Context) ([]Price, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+priceColumns+` FROM model_prices ORDER BY model_prefix, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prices []Price
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.ID, &p.Prefix, &p.Rule, &p.RuleTokens, &p.InputPerM, &p.OutputPerM, &p.CacheWritePerM, &p.CacheReadPerM, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		prices = append(prices, p)
	}
	return prices, rows.Err()
}

// GetPrice returns the price row with the given id, or nil if it doesn't
// exist.
func (db *DB) GetPrice(ctx context.Context, id int64) (*Price, error) {
	var p Price
	err := db.sql.QueryRowContext(ctx, `SELECT `+priceColumns+` FROM model_prices WHERE id = ?`, id).
		Scan(&p.ID, &p.Prefix, &p.Rule, &p.RuleTokens, &p.InputPerM, &p.OutputPerM, &p.CacheWritePerM, &p.CacheReadPerM, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePrice inserts a new rule row and returns its id. Prefix/rule/
// rule_tokens are only ever set here — they're immutable for the lifetime
// of the row, edited only by deleting and re-creating.
func (db *DB) CreatePrice(ctx context.Context, p Price) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO model_prices (model_prefix, rule, rule_tokens, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Prefix, p.Rule, p.RuleTokens, p.InputPerM, p.OutputPerM, p.CacheWritePerM, p.CacheReadPerM, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePrice patches an existing rule row's rates in place. Prefix/rule/
// rule_tokens are left untouched — only the four $/M columns and updated_at
// change, mirroring the admin UI split where the table's inline inputs only
// ever touch the rate fields.
func (db *DB) UpdatePrice(ctx context.Context, id int64, inputPerM, outputPerM, cacheWritePerM, cacheReadPerM, updatedAt float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE model_prices SET input_per_m = ?, output_per_m = ?, cache_write_per_m = ?, cache_read_per_m = ?, updated_at = ?
		 WHERE id = ?`,
		inputPerM, outputPerM, cacheWritePerM, cacheReadPerM, updatedAt, id,
	)
	return err
}

// DeletePrice removes a model price rule row by id. It is not an error if
// the id does not exist.
func (db *DB) DeletePrice(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM model_prices WHERE id = ?", id)
	return err
}

// UpsertPriceFromSync writes prefix's unconditional ("over 0") rule with
// the given rates, creating it if it doesn't exist yet. Used to refresh a
// model's base rate from an external source (see internal/litellm) without
// touching any tiered rules an admin has added on top of it for the same
// prefix.
func (db *DB) UpsertPriceFromSync(ctx context.Context, prefix string, inputPerM, outputPerM, cacheWritePerM, cacheReadPerM, now float64) (created bool, err error) {
	var id int64
	err = db.sql.QueryRowContext(ctx,
		`SELECT id FROM model_prices WHERE model_prefix = ? AND rule = 'over' AND rule_tokens = 0`, prefix,
	).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		_, err = db.CreatePrice(ctx, Price{
			Prefix: prefix, Rule: "over", RuleTokens: 0,
			InputPerM: inputPerM, OutputPerM: outputPerM,
			CacheWritePerM: cacheWritePerM, CacheReadPerM: cacheReadPerM,
			CreatedAt: now, UpdatedAt: now,
		})
		return true, err
	case err != nil:
		return false, err
	default:
		return false, db.UpdatePrice(ctx, id, inputPerM, outputPerM, cacheWritePerM, cacheReadPerM, now)
	}
}
