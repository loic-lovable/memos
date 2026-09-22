package sqlite

import (
	"context"
	"database/sql"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

// ConfigureShrimpPolicy pins trust and permanently latches writer withdrawal.
// The pilot has no reconciled writer-handoff procedure, so re-enabling a
// withdrawn writer is refused. Historical operation reads remain available.
func (d *DB) ConfigureShrimpPolicy(ctx context.Context, key string, allowWrite bool) error {
	if len(key) != 64 {
		return errors.New("invalid SHRIMP issuer fingerprint")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	var withdrawn bool
	err = tx.QueryRowContext(ctx, "SELECT issuer_key,write_withdrawn FROM shrimp_policy WHERE id=1").Scan(&previous, &withdrawn)
	action, code := "", ""
	switch {
	case errors.Is(err, sql.ErrNoRows):
		action = "pin_provisioning_policy"
		_, err = tx.ExecContext(ctx, "INSERT INTO shrimp_policy(id,issuer_key,write_withdrawn) VALUES(1,?,?)", key, !allowWrite)
	case err != nil:
		return err
	case key != previous:
		action, code = "replace_provisioning_trust", "trust_change_unsupported"
	case withdrawn && allowWrite:
		action, code = "reinstate_provisioning_writer", "writer_reconciliation_required"
	case !withdrawn && !allowWrite:
		action = "withdraw_provisioning_writer"
		_, err = tx.ExecContext(ctx, "UPDATE shrimp_policy SET write_withdrawn=1 WHERE id=1")
	}
	if err != nil {
		return err
	}
	if action != "" {
		// Policy changes have no network-supplied actor or credential material.
		event := store.ShrimpAudit{AttemptID: random.UUID(), Actor: "local-startup", Authority: "memos-local-enrollment", Action: action, Stage: "enrollment", Kind: "commit", Commit: "committed", Code: code}
		if code != "" {
			event.Kind, event.Commit = "outcome", "not_committed"
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_audit_attempt").Scan(&count); err != nil {
			return err
		}
		if count >= shrimpAuditMaxAttempts {
			return errors.New("audit_capacity")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO shrimp_audit_attempt(id) VALUES(?)", event.AttemptID); err != nil {
			return err
		}
		if err := appendShrimpAudit(ctx, tx, event); err != nil {
			return err
		}
		event.Kind = "outcome"
		if err := appendShrimpAudit(ctx, tx, event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if code != "" {
		return errors.New(code)
	}
	return nil
}

func shrimpWriterAllowed(ctx context.Context, q rowQuerier) error {
	var withdrawn bool
	err := q.QueryRowContext(ctx, "SELECT write_withdrawn FROM shrimp_policy WHERE id=1").Scan(&withdrawn)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("SHRIMP policy missing")
	}
	if err != nil {
		return err
	}
	if withdrawn {
		return store.ErrShrimpInsufficientScope
	}
	return nil
}
