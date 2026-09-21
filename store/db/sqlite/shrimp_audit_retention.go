package sqlite

import (
	"context"
	"database/sql"
)

const shrimpAuditRetentionSeconds = 7 * 86400

// collectShrimpAudit frees only settled attempts whose entire evidence is older
// than the promised retention. Unknown attempts and evidence still referenced by
// retained operation results stay intact. Keeping the latest attempt preserves
// the sequence high-water mark without reusing event positions.
func collectShrimpAudit(ctx context.Context, tx *sql.Tx, now int64) error {
	cutoff := now - shrimpAuditRetentionSeconds
	rows, err := tx.QueryContext(ctx, `SELECT a.id FROM shrimp_audit_attempt a
 WHERE a.id != (SELECT attempt_id FROM shrimp_audit_event ORDER BY sequence DESC LIMIT 1)
 AND EXISTS (SELECT 1 FROM shrimp_audit_event e WHERE e.attempt_id=a.id AND e.kind='outcome')
 AND NOT EXISTS (SELECT 1 FROM shrimp_audit_event e WHERE e.attempt_id=a.id
   AND (json_extract(e.body,'$.recorded_at') IS NULL OR json_extract(e.body,'$.recorded_at') >= ?))
 AND a.id NOT IN (SELECT json_extract(result,'$.AuditAttempt') FROM shrimp_operation
   WHERE json_extract(result,'$.AuditAttempt') IS NOT NULL)
 LIMIT 1000`, cutoff)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "DELETE FROM shrimp_audit_event WHERE attempt_id=?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM shrimp_audit_attempt WHERE id=?", id); err != nil {
			return err
		}
	}
	if len(ids) > 0 {
		_, err = tx.ExecContext(ctx, "UPDATE shrimp_audit_state SET since=MAX(since,?) WHERE id=1", cutoff)
	}
	return err
}
