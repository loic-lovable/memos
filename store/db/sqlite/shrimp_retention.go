package sqlite

import (
	"context"
	"database/sql"
)

// collectShrimpHistory closes execution windows permanently before reclaiming
// expired results. Missing windows cannot execute again, even after clock rollback.
// Causal events, source tombstones and audit records have separate lifetimes.
func collectShrimpHistory(ctx context.Context, tx *sql.Tx, now int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM shrimp_window WHERE closes_at <= ?", now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM shrimp_operation
		WHERE retained_until < ? AND NOT EXISTS (
			SELECT 1 FROM shrimp_window
			WHERE shrimp_window.id=shrimp_operation.window_id
			AND shrimp_window.principal=shrimp_operation.principal
		)`, now)
	return err
}

func (d *DB) collectShrimpHistory(ctx context.Context, now int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectShrimpHistory(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}
