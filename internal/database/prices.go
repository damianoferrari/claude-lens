package database

import (
	"context"
	"database/sql"
	"time"
)

// Price is a row of the model_prices table: USD per million tokens for a
// model prefix, matched against the full model string returned by the API.
// A prefix owns at most one row (model_prefix is UNIQUE).
//
// CacheWritePerM/CacheReadPerM price Anthropic's prompt-caching tokens
// (cache_creation_input_tokens / cache_read_input_tokens), which are billed
// at different rates than plain input tokens — a write is pricier, a read
// much cheaper.
//
// The four *Above200k fields override their base counterpart once a call's
// prompt size (input + cache-creation + cache-read tokens) exceeds 200,000
// — the documented surcharge tier some Claude long-context variants bill
// at. A nil pointer means no override is configured, not a free rate.
type Price struct {
	ID                      int64    `json:"id"`
	Prefix                  string   `json:"model_prefix"`
	InputPerM               float64  `json:"input_per_m"`
	OutputPerM              float64  `json:"output_per_m"`
	CacheWritePerM          float64  `json:"cache_write_per_m"`
	CacheReadPerM           float64  `json:"cache_read_per_m"`
	InputPerMAbove200k      *float64 `json:"input_per_m_above_200k"`
	OutputPerMAbove200k     *float64 `json:"output_per_m_above_200k"`
	CacheWritePerMAbove200k *float64 `json:"cache_write_per_m_above_200k"`
	CacheReadPerMAbove200k  *float64 `json:"cache_read_per_m_above_200k"`
	CreatedAt               float64  `json:"created_at"`
	UpdatedAt               float64  `json:"updated_at"`
}

const priceColumns = `id, model_prefix, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m,
	input_per_m_above_200k, output_per_m_above_200k, cache_write_per_m_above_200k, cache_read_per_m_above_200k,
	created_at, updated_at`

// defaultPrices seeds model_prices on first run. Cache write/read rates
// follow Anthropic's standard multipliers (1.25x input for a 5-minute-TTL
// cache write, 0.1x input for a cache read).
var defaultPrices = []Price{
	{Prefix: "claude-opus-5", InputPerM: 5.00, OutputPerM: 25.00, CacheWritePerM: 6.25, CacheReadPerM: 0.50},
	{Prefix: "claude-sonnet-5", InputPerM: 3.00, OutputPerM: 15.00, CacheWritePerM: 3.75, CacheReadPerM: 0.30},
	{Prefix: "claude-haiku-4-5", InputPerM: 1.00, OutputPerM: 5.00, CacheWritePerM: 1.25, CacheReadPerM: 0.10},
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

func scanPrice(row interface{ Scan(...any) error }) (Price, error) {
	var p Price
	err := row.Scan(
		&p.ID, &p.Prefix, &p.InputPerM, &p.OutputPerM, &p.CacheWritePerM, &p.CacheReadPerM,
		&p.InputPerMAbove200k, &p.OutputPerMAbove200k, &p.CacheWritePerMAbove200k, &p.CacheReadPerMAbove200k,
		&p.CreatedAt, &p.UpdatedAt,
	)
	return p, err
}

// ListPrices returns all model price rows, ordered by prefix.
func (db *DB) ListPrices(ctx context.Context) ([]Price, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+priceColumns+` FROM model_prices ORDER BY model_prefix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prices []Price
	for rows.Next() {
		p, err := scanPrice(rows)
		if err != nil {
			return nil, err
		}
		prices = append(prices, p)
	}
	return prices, rows.Err()
}

// GetPrice returns the price row with the given id, or nil if it doesn't
// exist.
func (db *DB) GetPrice(ctx context.Context, id int64) (*Price, error) {
	p, err := scanPrice(db.sql.QueryRowContext(ctx, `SELECT `+priceColumns+` FROM model_prices WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetPriceByPrefix returns the price row for the given prefix, or nil if it
// doesn't exist.
func (db *DB) GetPriceByPrefix(ctx context.Context, prefix string) (*Price, error) {
	p, err := scanPrice(db.sql.QueryRowContext(ctx, `SELECT `+priceColumns+` FROM model_prices WHERE model_prefix = ?`, prefix))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePrice inserts a new price row and returns its id. Prefix is only
// ever set here — it's immutable for the lifetime of the row.
func (db *DB) CreatePrice(ctx context.Context, p Price) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO model_prices
		    (model_prefix, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m,
		     input_per_m_above_200k, output_per_m_above_200k, cache_write_per_m_above_200k, cache_read_per_m_above_200k,
		     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Prefix, p.InputPerM, p.OutputPerM, p.CacheWritePerM, p.CacheReadPerM,
		p.InputPerMAbove200k, p.OutputPerMAbove200k, p.CacheWritePerMAbove200k, p.CacheReadPerMAbove200k,
		p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePrice replaces an existing row's rates and above-200k overrides in
// place. Prefix is left untouched — it's immutable once created.
func (db *DB) UpdatePrice(ctx context.Context, id int64, p Price) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE model_prices SET
		    input_per_m = ?, output_per_m = ?, cache_write_per_m = ?, cache_read_per_m = ?,
		    input_per_m_above_200k = ?, output_per_m_above_200k = ?, cache_write_per_m_above_200k = ?, cache_read_per_m_above_200k = ?,
		    updated_at = ?
		 WHERE id = ?`,
		p.InputPerM, p.OutputPerM, p.CacheWritePerM, p.CacheReadPerM,
		p.InputPerMAbove200k, p.OutputPerMAbove200k, p.CacheWritePerMAbove200k, p.CacheReadPerMAbove200k,
		p.UpdatedAt, id,
	)
	return err
}

// DeletePrice removes a model price row by id. It is not an error if the id
// does not exist.
func (db *DB) DeletePrice(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, "DELETE FROM model_prices WHERE id = ?", id)
	return err
}

// UpsertPriceFromSync writes prefix's base rates from an external source
// (see internal/litellm), creating the row if it doesn't exist yet. The
// four base rates always overwrite; an above-200k override is only
// overwritten when the sync supplies a non-nil value for it, so a manually
// configured override survives a sync that doesn't report that tier.
func (db *DB) UpsertPriceFromSync(ctx context.Context, prefix string, inputPerM, outputPerM, cacheWritePerM, cacheReadPerM float64, inputAbove200k, outputAbove200k, cacheWriteAbove200k, cacheReadAbove200k *float64, now float64) (created bool, err error) {
	existing, err := db.GetPriceByPrefix(ctx, prefix)
	if err != nil {
		return false, err
	}

	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO model_prices
		    (model_prefix, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m,
		     input_per_m_above_200k, output_per_m_above_200k, cache_write_per_m_above_200k, cache_read_per_m_above_200k,
		     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(model_prefix) DO UPDATE SET
		    input_per_m = excluded.input_per_m,
		    output_per_m = excluded.output_per_m,
		    cache_write_per_m = excluded.cache_write_per_m,
		    cache_read_per_m = excluded.cache_read_per_m,
		    input_per_m_above_200k = COALESCE(excluded.input_per_m_above_200k, model_prices.input_per_m_above_200k),
		    output_per_m_above_200k = COALESCE(excluded.output_per_m_above_200k, model_prices.output_per_m_above_200k),
		    cache_write_per_m_above_200k = COALESCE(excluded.cache_write_per_m_above_200k, model_prices.cache_write_per_m_above_200k),
		    cache_read_per_m_above_200k = COALESCE(excluded.cache_read_per_m_above_200k, model_prices.cache_read_per_m_above_200k),
		    updated_at = excluded.updated_at`,
		prefix, inputPerM, outputPerM, cacheWritePerM, cacheReadPerM,
		inputAbove200k, outputAbove200k, cacheWriteAbove200k, cacheReadAbove200k,
		now, now,
	)
	if err != nil {
		return false, err
	}
	return existing == nil, nil
}
