package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/usememos/memos/store"
)

func TestShrimpAuditRetentionPreservesUncertaintyReferencesAndSequence(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	settle := func() *store.ShrimpAudit {
		event, err := d.BeginShrimpAudit(ctx, "hr", "authority", "mutation")
		require.NoError(t, err)
		event.Commit = "not_committed"
		require.NoError(t, d.FinishShrimpAudit(ctx, *event))
		return event
	}
	expired := settle()
	boundary := settle()
	unresolved, err := d.BeginShrimpAudit(ctx, "hr", "authority", "mutation")
	require.NoError(t, err)
	m := pilotIntent(t, d, "create_subject", nil)
	audited, referenced := auditIntent(t, d, m)
	_, err = s.ApplyShrimp(audited, m)
	require.NoError(t, err)
	tail := settle()
	before, err := d.InspectShrimpAudit(ctx, "", 0, 100)
	require.NoError(t, err)
	now := time.Now().Unix() + shrimpAuditRetentionSeconds + 100
	cutoff := now - shrimpAuditRetentionSeconds
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_audit_event SET body=json_set(body,'$.recorded_at',?)", cutoff-1)
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_audit_event SET body=json_set(body,'$.recorded_at',?) WHERE attempt_id=?", cutoff, boundary.AttemptID)
	require.NoError(t, err)
	tx, err := d.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, collectShrimpAudit(ctx, tx, now))
	require.NoError(t, tx.Commit())
	exists := func(id string) bool {
		var found bool
		require.NoError(t, d.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM shrimp_audit_attempt WHERE id=?)", id).Scan(&found))
		return found
	}
	require.False(t, exists(expired.AttemptID))
	require.True(t, exists(boundary.AttemptID), "exact retention boundary must survive")
	require.True(t, exists(unresolved.AttemptID), "unknown outcomes must not disappear")
	require.True(t, exists(referenced.AttemptID), "retained receipts keep their audit evidence")
	require.True(t, exists(tail.AttemptID), "preserve sequence high-water mark")
	after, err := d.InspectShrimpAudit(ctx, "", 0, 100)
	require.NoError(t, err)
	require.Equal(t, before.Epoch, after.Epoch)
	require.Equal(t, before.Boundary, after.Boundary)
	require.Equal(t, cutoff, after.Since)
	require.EqualValues(t, 1, after.Unresolved)
	next := settle()
	latest, err := d.InspectShrimpAudit(ctx, after.Epoch, after.Boundary, 100)
	require.NoError(t, err)
	require.Greater(t, latest.Boundary, after.Boundary)
	require.Equal(t, next.AttemptID, latest.Events[0].AttemptID)
}

func TestShrimpAuditAtCapacityReclaimsOnlyExpiredSettledAttempts(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	audited, old := auditIntent(t, d, intent)
	_, err := s.ApplyShrimp(audited, intent)
	require.NoError(t, err)
	past := time.Now().Unix() - 1
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_window SET closes_at=?", past)
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_operation SET retained_until=?", past)
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_audit_event SET body=json_set(body,'$.recorded_at',?)", time.Now().Unix()-shrimpAuditRetentionSeconds-1)
	require.NoError(t, err)
	_, err = d.BeginShrimpAudit(ctx, "hr", "authority", "mutation")
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, `WITH RECURSIVE slots(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM slots WHERE n < ?)
 INSERT INTO shrimp_audit_attempt(id) SELECT 'held-'||n FROM slots`, shrimpAuditMaxAttempts-2)
	require.NoError(t, err)
	_, err = d.BeginShrimpAudit(ctx, "hr", "authority", "mutation")
	require.NoError(t, err, "settled expired evidence frees a slot without dropping uncertainty")
	var count int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_audit_attempt WHERE id=?", old.AttemptID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_audit_attempt").Scan(&count))
	require.Equal(t, shrimpAuditMaxAttempts, count)
	_, err = d.BeginShrimpAudit(ctx, "hr", "authority", "mutation")
	require.ErrorContains(t, err, "audit_capacity", "unresolved attempts cannot be evicted to admit work")
}
