package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

// Each admitted attempt reserves up to six journal rows. At capacity, only
// settled evidence past retention can be collected; unresolved work stays visible.
// Accepted work can always append its bounded outcome when admission is full.
const shrimpAuditMaxAttempts = 100000

func (d *DB) configureShrimpAudit(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO shrimp_audit_state(id,epoch,since,legacy_gap)
		SELECT 1,?,?,(EXISTS(SELECT 1 FROM shrimp_operation) OR EXISTS(SELECT 1 FROM shrimp_event WHERE sequence>0))`, random.UUID(), time.Now().Unix())
	return err
}

func appendShrimpAudit(ctx context.Context, tx *sql.Tx, event store.ShrimpAudit) error {
	event.ID = random.UUID()
	event.RecordedAt = time.Now().Unix()
	if event.OccurredAt == 0 {
		event.OccurredAt = event.RecordedAt
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shrimp_audit_event(sequence,id,attempt_id,kind,body)
		SELECT COALESCE(MAX(sequence),0)+1,?,?,?,? FROM shrimp_audit_event`, event.ID, event.AttemptID, event.Kind, string(encoded))
	return err
}

// BeginShrimpAudit durably reserves an attempt before its request is processed.
func (d *DB) BeginShrimpAudit(ctx context.Context, actor, authority, action string) (*store.ShrimpAudit, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_audit_attempt").Scan(&count); err != nil {
		return nil, err
	}
	if count >= shrimpAuditMaxAttempts {
		// Audit admission precedes Apply: expired results must not pin all slots
		// and prevent the very request that would otherwise collect them.
		if err := collectShrimpHistory(ctx, tx, time.Now().Unix()); err != nil {
			return nil, err
		}
		if err := collectShrimpAudit(ctx, tx, time.Now().Unix()); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_audit_attempt").Scan(&count); err != nil {
			return nil, err
		}
		if count >= shrimpAuditMaxAttempts {
			return nil, errors.New("audit_capacity")
		}
	}
	event := &store.ShrimpAudit{AttemptID: random.UUID(), Actor: actor, Authority: authority, Action: action, Kind: "attempt", Commit: "unknown", OccurredAt: time.Now().Unix()}
	if _, err := tx.ExecContext(ctx, "INSERT INTO shrimp_audit_attempt(id) VALUES(?)", event.AttemptID); err != nil {
		return nil, err
	}
	if err := appendShrimpAudit(ctx, tx, *event); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}

// IdentifyShrimpAudit appends supplied operation correlation without asserting existence.
func (d *DB) IdentifyShrimpAudit(ctx context.Context, event store.ShrimpAudit) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	event.Kind = "identified"
	if err := appendShrimpAudit(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func auditResolved(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM shrimp_audit_event WHERE attempt_id=? AND kind='outcome')", id).Scan(&exists)
	return exists, err
}

// FinishShrimpAudit appends known rejection only. Unknown infrastructure failures
// leave their durable marker unresolved; absence of a receipt is not rollback proof.
func (d *DB) FinishShrimpAudit(ctx context.Context, event store.ShrimpAudit) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	resolved, err := auditResolved(ctx, tx, event.AttemptID)
	if err != nil || resolved {
		return err
	}
	if event.Commit == "unknown" {
		return nil
	}
	event.Kind = "outcome"
	event.OccurredAt = time.Now().Unix()
	if err := appendShrimpAudit(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

// journalShrimpOutcome runs inside the same transaction as business state and the
// immutable retry result. A retry resolves only its own attempt, never an old one.
func journalShrimpOutcome(ctx context.Context, tx *sql.Tx, m store.ShrimpMutation, result *store.ShrimpResult, replay bool) error {
	attempt := store.ShrimpAuditFromContext(ctx)
	if attempt == nil {
		return nil
	} // Internal store tests/native callers have no authenticated protocol request.
	event := *attempt
	event.OccurredAt = time.Now().Unix()
	event.Action, event.Window, event.Operation = m.Action, m.Window, m.ID
	event.OperationKnown, event.Code = true, result.Error
	event.OriginalAttempt = result.AuditAttempt
	event.Commit = "not_committed"
	if result.Error == "" {
		event.Commit = "committed"
		event.SubjectID, event.SourceID, event.Revision = result.Subject.ID, result.Subject.SourceID, result.Subject.Revision
		event.CausalToken = result.Token
		if !replay {
			event.Dependencies = m.Dependencies
		}
	}
	if result.Error != "" && m.SubjectID != "" {
		subject, err := readShrimpSubject(ctx, tx, m.SubjectID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && subject.ID == m.SubjectID {
			event.SubjectID, event.SourceID, event.Revision = subject.ID, subject.SourceID, subject.Revision
		}
	}
	if !replay {
		event.Kind, event.Stage = "accepted", "acceptance"
		if err := appendShrimpAudit(ctx, tx, event); err != nil {
			return err
		}
		if result.Error == "" {
			event.Kind, event.Stage = "commit", "commit"
			if err := appendShrimpAudit(ctx, tx, event); err != nil {
				return err
			}
			if m.Action == "disable" || m.Action == "retire" {
				event.Kind, event.Stage, event.Effect = "effect", "effect", "admission_block_complete"
				if err := appendShrimpAudit(ctx, tx, event); err != nil {
					return err
				}
			}
		}
	}
	event.Kind, event.Stage = "outcome", "terminal"
	if replay {
		event.Stage = "replay"
	}
	return appendShrimpAudit(ctx, tx, event)
}

// InspectShrimpAudit reads one bounded page and coverage from a coherent snapshot.
func (d *DB) InspectShrimpAudit(ctx context.Context, epoch string, after int64, limit int) (*store.ShrimpAuditPage, error) {
	if after < 0 || limit < 1 || limit > 100 || (after != 0 && epoch == "") {
		return nil, errors.New("invalid_cursor")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	page := &store.ShrimpAuditPage{Events: []store.ShrimpAudit{}, Next: after}
	if err := tx.QueryRowContext(ctx, "SELECT epoch,since,legacy_gap FROM shrimp_audit_state WHERE id=1").Scan(&page.Epoch, &page.Since, &page.LegacyGap); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_operation").Scan(&page.RetainedResults); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_audit_attempt").Scan(&page.RetainedAttempts); err != nil {
		return nil, err
	}
	if epoch != "" && epoch != page.Epoch {
		return nil, errors.New("invalid_cursor")
	}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM shrimp_audit_event").Scan(&page.Boundary); err != nil {
		return nil, err
	}
	if after > page.Boundary {
		return nil, errors.New("invalid_cursor")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM shrimp_audit_attempt a WHERE NOT EXISTS
		(SELECT 1 FROM shrimp_audit_event e WHERE e.attempt_id=a.id AND e.kind='outcome')`).Scan(&page.Unresolved); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT sequence,body FROM shrimp_audit_event WHERE sequence>? ORDER BY sequence LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int64
		var body string
		if err := rows.Scan(&sequence, &body); err != nil {
			return nil, err
		}
		var event store.ShrimpAudit
		if err := json.Unmarshal([]byte(body), &event); err != nil {
			return nil, err
		}
		event.Sequence = sequence
		page.Events = append(page.Events, event)
		page.Next = sequence
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page.More = page.Next < page.Boundary
	return page, nil
}

func journalShrimpNative(ctx context.Context, tx *sql.Tx, userID int32) error {
	attempt := store.ShrimpAuditFromContext(ctx)
	if attempt == nil {
		return nil
	}
	event := *attempt
	event.Kind, event.Stage, event.Commit = "commit", "native_database", "committed"
	event.NativeUserID = userID
	event.OccurredAt = time.Now().Unix()
	var subjectID string
	err := tx.QueryRowContext(ctx, "SELECT id FROM shrimp_subject WHERE user_id=?", userID).Scan(&subjectID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		subject, err := readShrimpSubject(ctx, tx, subjectID)
		if err != nil {
			return err
		}
		event.SubjectID, event.SourceID, event.Revision = subject.ID, subject.SourceID, subject.Revision
	}
	if err := appendShrimpAudit(ctx, tx, event); err != nil {
		return err
	}
	event.Kind = "outcome"
	return appendShrimpAudit(ctx, tx, event)
}

// IdentifyShrimpNativeAudit only receives native IDs resolved and authorized by the API.
func (d *DB) IdentifyShrimpNativeAudit(ctx context.Context, event store.ShrimpAudit, userID int32) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	event.Kind, event.NativeUserID = "identified", userID
	var id string
	err = tx.QueryRowContext(ctx, "SELECT id FROM shrimp_subject WHERE user_id=?", userID).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		subject, err := readShrimpSubject(ctx, tx, id)
		if err != nil {
			return err
		}
		event.SubjectID, event.SourceID, event.Revision = subject.ID, subject.SourceID, subject.Revision
	}
	if err := appendShrimpAudit(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The final rejection preserves this already authorized correlation.
	if current := store.ShrimpAuditFromContext(ctx); current != nil {
		current.NativeUserID, current.SubjectID, current.SourceID, current.Revision = event.NativeUserID, event.SubjectID, event.SourceID, event.Revision
	}
	return nil
}
