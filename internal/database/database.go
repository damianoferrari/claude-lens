// Package database wraps the SQLite-backed persistence layer shared by the
// proxy and admin servers.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/lfsc09/claude-lens/internal/notify"
	_ "modernc.org/sqlite"
)

// DB wraps a *sql.DB configured for claude-lens' schema and concurrency model.
type DB struct {
	sql *sql.DB

	// notifier drives Slack alert delivery from accrueLimiterCost. Unset
	// (nil) until SetNotifications is called, in which case alert dispatch
	// is silently skipped.
	notifier *notify.Client

	// alertMu serializes accrueLimiterCost's read-check-write of each
	// limiter's alert state, so two exchanges completing close together for
	// the same limiter can't both observe AlertSent == false and send a
	// duplicate budget-threshold alert.
	alertMu sync.Mutex
}

// SetNotifications wires up Slack delivery for limiter alerts (both the
// budget-threshold and per-request-cost kinds). Safe to skip calling
// entirely — accrueLimiterCost just won't send alerts.
func (db *DB) SetNotifications(client *notify.Client) {
	db.notifier = client
}

const schema = `
CREATE TABLE IF NOT EXISTS exchanges (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id            TEXT    NOT NULL,
    session_name          TEXT,
    path                  TEXT    NOT NULL,
    timestamp             REAL    NOT NULL,
    is_streaming          INTEGER NOT NULL DEFAULT 0,
    input_messages        TEXT,
    output_text           TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_creation_tokens INTEGER,
    cache_read_tokens     INTEGER,
    model                 TEXT,
    cost                  REAL,
    input_cost            REAL,
    output_cost           REAL,
    cache_creation_cost   REAL,
    cache_read_cost       REAL,
    matched_price         TEXT,
    raw_request           TEXT,
    raw_response          TEXT
);
CREATE INDEX IF NOT EXISTS idx_exchanges_session_id ON exchanges (session_id);
CREATE INDEX IF NOT EXISTS idx_exchanges_timestamp  ON exchanges (timestamp);

CREATE TABLE IF NOT EXISTS exchanges_ledger (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    exchange_id           INTEGER REFERENCES exchanges(id) ON DELETE SET NULL,
    session_id            TEXT    NOT NULL,
    timestamp             REAL    NOT NULL,
    model                 TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_creation_tokens INTEGER,
    cache_read_tokens     INTEGER,
    cost                  REAL,
    input_cost            REAL,
    output_cost           REAL,
    cache_creation_cost   REAL,
    cache_read_cost       REAL,
    matched_price         TEXT
);
CREATE INDEX IF NOT EXISTS idx_exchanges_ledger_session_id  ON exchanges_ledger (session_id);
CREATE INDEX IF NOT EXISTS idx_exchanges_ledger_timestamp   ON exchanges_ledger (timestamp);
CREATE INDEX IF NOT EXISTS idx_exchanges_ledger_exchange_id ON exchanges_ledger (exchange_id);

CREATE TABLE IF NOT EXISTS model_prices (
    id                           INTEGER PRIMARY KEY AUTOINCREMENT,
    model_prefix                 TEXT    NOT NULL UNIQUE,
    input_per_m                  REAL    NOT NULL,
    output_per_m                 REAL    NOT NULL,
    cache_write_per_m            REAL    NOT NULL DEFAULT 0,
    cache_read_per_m             REAL    NOT NULL DEFAULT 0,
    input_per_m_above_200k       REAL,
    output_per_m_above_200k      REAL,
    cache_write_per_m_above_200k REAL,
    cache_read_per_m_above_200k  REAL,
    created_at                   REAL    NOT NULL,
    updated_at                   REAL    NOT NULL
);

CREATE TABLE IF NOT EXISTS limiters (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id        TEXT    NOT NULL DEFAULT '',
    limit_amount      REAL    NOT NULL,
    current_cost      REAL    NOT NULL DEFAULT 0,
    refresh_value     INTEGER NOT NULL,
    refresh_unit      TEXT    NOT NULL CHECK (refresh_unit IN ('minutes','hours','days','months')),
    refresh_aligned   INTEGER NOT NULL DEFAULT 0,
    next_refresh_at   REAL    NOT NULL,
    active_start_hour INTEGER,
    active_end_hour   INTEGER,
    is_active         INTEGER NOT NULL DEFAULT 1,
    created_at        REAL    NOT NULL,
    updated_at        REAL    NOT NULL,
    slack_webhook_url    TEXT    NOT NULL DEFAULT '',
    alert_threshold_pct  INTEGER,
    alert_sent           INTEGER NOT NULL DEFAULT 0,
    alert_request_cost_usd REAL,
    CHECK ((active_start_hour IS NULL) = (active_end_hour IS NULL)),
    CHECK (alert_threshold_pct IS NULL OR (alert_threshold_pct BETWEEN 1 AND 100)),
    CHECK (alert_request_cost_usd IS NULL OR alert_request_cost_usd > 0)
);
CREATE INDEX IF NOT EXISTS idx_limiters_session_id ON limiters (session_id);

CREATE TABLE IF NOT EXISTS settings (
    id                             INTEGER PRIMARY KEY CHECK (id = 1),
    litellm_sync_interval_minutes  INTEGER NOT NULL DEFAULT 60,
    litellm_last_synced_at         REAL    NOT NULL DEFAULT 0,
    litellm_last_sync_error        TEXT    NOT NULL DEFAULT '',
    litellm_last_attempt_at        REAL    NOT NULL DEFAULT 0,
    updated_at                     REAL    NOT NULL
);
`

// newColumns lists columns added to the schema after the tables already
// shipped, keyed by table. CREATE TABLE IF NOT EXISTS above is a no-op on a
// database that already has these tables, so on an existing installation
// these columns must be added explicitly. Safe to run on every startup:
// addColumnIfMissing checks PRAGMA table_info before altering.
var newColumns = map[string][]string{
	"exchanges": {
		"cache_creation_tokens INTEGER",
		"cache_read_tokens INTEGER",
		"cache_creation_cost REAL",
		"cache_read_cost REAL",
		"matched_price TEXT",
	},
	"model_prices": {
		"cache_write_per_m REAL NOT NULL DEFAULT 0",
		"cache_read_per_m REAL NOT NULL DEFAULT 0",
	},
	"limiters": {
		"slack_webhook_url TEXT NOT NULL DEFAULT ''",
		"alert_threshold_pct INTEGER",
		"alert_sent INTEGER NOT NULL DEFAULT 0",
		"alert_request_cost_usd REAL",
	},
	"settings": {
		"litellm_last_sync_error TEXT NOT NULL DEFAULT ''",
		"litellm_last_attempt_at REAL NOT NULL DEFAULT 0",
	},
}

// migrateSchema adds any column listed in newColumns that isn't already
// present on its table. Idempotent — freshly created tables (which already
// have every column via the schema constant above) are left untouched.
func migrateSchema(ctx context.Context, sqlDB *sql.DB) error {
	for table, columns := range newColumns {
		existing, err := tableColumns(ctx, sqlDB, table)
		if err != nil {
			return fmt.Errorf("inspect columns of %s: %w", table, err)
		}
		for _, col := range columns {
			name := strings.Fields(col)[0]
			if existing[name] {
				continue
			}
			if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, col)); err != nil {
				return fmt.Errorf("add column %s.%s: %w", table, name, err)
			}
		}
	}
	return nil
}

// migrateModelPricesToRules rewrites model_prices from the old one-row-per-
// prefix shape (model_prefix as PRIMARY KEY) to the tiered shape (surrogate
// id, rule, rule_tokens, created_at) that lets a prefix own multiple rule
// rows. SQLite can't relax a PRIMARY KEY via ALTER TABLE, so this rebuilds
// the table inside a transaction instead of adding a column. Guarded by
// presence of the `id` column: a no-op on a fresh DB (already created in
// the new shape by `schema` above) or an already-migrated one. Runs after
// migrateSchema so cache_write_per_m/cache_read_per_m are guaranteed
// present on the old table even for a very old DB.
func migrateModelPricesToRules(ctx context.Context, sqlDB *sql.DB) error {
	cols, err := tableColumns(ctx, sqlDB, "model_prices")
	if err != nil {
		return fmt.Errorf("inspect columns of model_prices: %w", err)
	}
	if cols["id"] {
		return nil
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE model_prices_new (
		    id                INTEGER PRIMARY KEY AUTOINCREMENT,
		    model_prefix      TEXT    NOT NULL,
		    rule              TEXT    NOT NULL DEFAULT 'over',
		    rule_tokens       INTEGER NOT NULL DEFAULT 0,
		    input_per_m       REAL    NOT NULL,
		    output_per_m      REAL    NOT NULL,
		    cache_write_per_m REAL    NOT NULL DEFAULT 0,
		    cache_read_per_m  REAL    NOT NULL DEFAULT 0,
		    created_at        REAL    NOT NULL,
		    updated_at        REAL    NOT NULL
		)`); err != nil {
		return fmt.Errorf("create model_prices_new: %w", err)
	}
	// Pre-existing rows become unconditional "over 0" rules, since a
	// prefix's single price previously applied unconditionally. created_at
	// has no prior value to recover, so it's backfilled from updated_at.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_prices_new
		    (model_prefix, rule, rule_tokens, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m, created_at, updated_at)
		SELECT model_prefix, 'over', 0, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m, updated_at, updated_at
		FROM model_prices`); err != nil {
		return fmt.Errorf("copy model_prices rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE model_prices`); err != nil {
		return fmt.Errorf("drop old model_prices: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE model_prices_new RENAME TO model_prices`); err != nil {
		return fmt.Errorf("rename model_prices_new: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_model_prices_prefix ON model_prices (model_prefix)`); err != nil {
		return fmt.Errorf("create model_prices prefix index: %w", err)
	}

	return tx.Commit()
}

// migrateModelPricesToUniquePrefix collapses model_prices from the tiered,
// multi-row-per-prefix shape back down to a single unique row per prefix,
// now that a prefix's price config carries its own above-200k override
// columns instead of separate rule rows. Guarded by presence of the `rule`
// column: a no-op on a fresh DB (already created in the new shape by
// `schema` above) or an already-migrated one. Runs after
// migrateModelPricesToRules so it can rely on that shape existing even for
// a very old DB.
//
// For each prefix, the kept row is its unconditional "over 0" rule if one
// exists, else the row with the smallest rule_tokens; ties (including
// duplicate "over 0" rows, which the old schema never prevented) are broken
// by lowest id, so a prefix's pricing is never lost outright and exactly one
// row survives per prefix. Any other tiered rule a prefix owned is
// discarded — above-200k overrides start unset and are populated afterward
// by a LiteLLM sync or manual admin entry.
func migrateModelPricesToUniquePrefix(ctx context.Context, sqlDB *sql.DB) error {
	cols, err := tableColumns(ctx, sqlDB, "model_prices")
	if err != nil {
		return fmt.Errorf("inspect columns of model_prices: %w", err)
	}
	if !cols["rule"] {
		return nil
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE model_prices_new (
		    id                           INTEGER PRIMARY KEY AUTOINCREMENT,
		    model_prefix                 TEXT    NOT NULL UNIQUE,
		    input_per_m                  REAL    NOT NULL,
		    output_per_m                 REAL    NOT NULL,
		    cache_write_per_m            REAL    NOT NULL DEFAULT 0,
		    cache_read_per_m             REAL    NOT NULL DEFAULT 0,
		    input_per_m_above_200k       REAL,
		    output_per_m_above_200k      REAL,
		    cache_write_per_m_above_200k REAL,
		    cache_read_per_m_above_200k  REAL,
		    created_at                   REAL    NOT NULL,
		    updated_at                   REAL    NOT NULL
		)`); err != nil {
		return fmt.Errorf("create model_prices_new: %w", err)
	}
	// A prefix's kept row is picked by ordering candidates
	// (unconditional "over 0" rule first, then smallest rule_tokens, then
	// lowest id) and taking the first — a single deterministic row per
	// prefix even if a prefix owned duplicate "over 0" rows, which the old
	// schema never prevented.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_prices_new
		    (model_prefix, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m, created_at, updated_at)
		SELECT model_prefix, input_per_m, output_per_m, cache_write_per_m, cache_read_per_m, created_at, updated_at
		FROM model_prices AS p
		WHERE p.id = (
		    SELECT m.id FROM model_prices AS m
		    WHERE m.model_prefix = p.model_prefix
		    ORDER BY
		        CASE WHEN m.rule = 'over' AND m.rule_tokens = 0 THEN 0 ELSE 1 END,
		        m.rule_tokens,
		        m.id
		    LIMIT 1
		)`); err != nil {
		return fmt.Errorf("copy model_prices rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE model_prices`); err != nil {
		return fmt.Errorf("drop old model_prices: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE model_prices_new RENAME TO model_prices`); err != nil {
		return fmt.Errorf("rename model_prices_new: %w", err)
	}

	return tx.Commit()
}

// migrateExchangesLedgerBackfill copies every exchange's cost/token data
// into exchanges_ledger for rows that predate that table's existence. The
// NOT EXISTS guard makes this safe on every startup: a no-op on a fresh DB
// (exchanges is empty), a one-time full backfill on an upgrade, and a no-op
// again afterward since SaveExchange keeps both tables in sync going
// forward.
func migrateExchangesLedgerBackfill(ctx context.Context, sqlDB *sql.DB) error {
	_, err := sqlDB.ExecContext(ctx, `
		INSERT INTO exchanges_ledger
		    (exchange_id, session_id, timestamp, model,
		     input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		     cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, matched_price)
		SELECT id, session_id, timestamp, model,
		       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		       cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, matched_price
		FROM exchanges e
		WHERE NOT EXISTS (SELECT 1 FROM exchanges_ledger l WHERE l.exchange_id = e.id)`)
	if err != nil {
		return fmt.Errorf("backfill exchanges_ledger: %w", err)
	}
	return nil
}

func tableColumns(ctx context.Context, sqlDB *sql.DB, table string) (map[string]bool, error) {
	rows, err := sqlDB.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// Open opens (creating if necessary) the SQLite database at path, applies the
// schema, and seeds default model prices on first run.
//
// The connection pool is deliberately capped at a single connection: SQLite
// allows only one writer at a time, and both the proxy (writing on every
// intercepted POST) and the admin server (upserting/deleting prices) share
// this *DB from the same process. Serializing at the pool level avoids
// SQLITE_BUSY races without needing retry logic throughout the query code;
// busy_timeout gives any queued writer up to 5s to acquire the lock before
// failing outright. _foreign_keys=on turns on SQLite's foreign-key
// enforcement (off by default per connection) so exchanges_ledger's
// ON DELETE SET NULL on exchange_id actually fires. Because of this, any
// future migration that rebuilds exchanges (drop+recreate under a new
// shape, as migrateModelPricesToRules does for model_prices) must wrap the
// drop in PRAGMA foreign_keys=OFF / =ON: SQLite performs an implicit
// delete-all before dropping a table that's an FK parent, which would
// otherwise cascade-null every exchanges_ledger.exchange_id even though the
// rows are only meant to be recreated, not gone.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", path)

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.ExecContext(ctx, schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateSchema(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	if err := migrateModelPricesToRules(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate model_prices to rules: %w", err)
	}
	if err := migrateModelPricesToUniquePrefix(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate model_prices to unique prefix: %w", err)
	}
	if err := migrateExchangesLedgerBackfill(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("backfill exchanges_ledger: %w", err)
	}

	db := &DB{sql: sqlDB}

	if err := db.seedDefaultPrices(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("seed default prices: %w", err)
	}
	if err := db.seedDefaultSettings(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("seed default settings: %w", err)
	}

	return db, nil
}

// Close closes the underlying connection pool.
func (db *DB) Close() error {
	return db.sql.Close()
}
