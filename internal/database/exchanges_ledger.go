package database

import (
	"context"
	"database/sql"
)

// insertLedgerEntry inserts the cost/token bookkeeping row mirroring an
// exchange just inserted as exchangeID, inside the same transaction as that
// insert. exchanges_ledger outlives its source exchanges row: deleting the
// row later only sets exchange_id to NULL (see the schema's
// ON DELETE SET NULL), never removes the ledger entry itself.
func insertLedgerEntry(ctx context.Context, tx *sql.Tx, exchangeID int64, e Exchange, cost *float64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO exchanges_ledger
			(exchange_id, session_id, timestamp, model,
			 input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			 cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, matched_price)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exchangeID, e.SessionID, e.Timestamp, e.Model,
		e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens,
		cost, e.InputCost, e.OutputCost, e.CacheCreationCost, e.CacheReadCost, e.MatchedPrice,
	)
	return err
}
