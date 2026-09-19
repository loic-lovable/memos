package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func auditIntent(t *testing.T, d *DB, m store.ShrimpMutation) (context.Context, *store.ShrimpAudit) {
	t.Helper()
	event, err := d.BeginShrimpAudit(t.Context(), m.Principal, "hr-authority", "mutation")
	require.NoError(t, err)
	event.Window, event.Operation = m.Window, m.ID
	require.NoError(t, d.IdentifyShrimpAudit(t.Context(), *event))
	return store.WithShrimpAudit(t.Context(), event), event
}

func TestShrimpAuditRetryDoesNotResolveAbandonedAttemptOrInventCommit(t *testing.T) {
	s, d := pilotStore(t)
	m := pilotIntent(t, d, "create_subject", nil)
	_, abandoned := auditIntent(t, d, m)
	ctx, original := auditIntent(t, d, m)
	result, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	ctx, retry := auditIntent(t, d, m)
	again, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	require.Equal(t, result, again)
	page, err := d.InspectShrimpAudit(t.Context(), "", 0, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Unresolved)
	commits, replays := 0, 0
	for _, e := range page.Events {
		if e.Kind == "outcome" {
			require.NotEqual(t, abandoned.AttemptID, e.AttemptID)
		}
		if e.Kind == "commit" {
			commits++
			require.Equal(t, original.AttemptID, e.AttemptID)
		}
		if e.Stage == "replay" {
			replays++
			require.Equal(t, retry.AttemptID, e.AttemptID)
			require.Equal(t, original.AttemptID, e.OriginalAttempt)
		}
	}
	require.Equal(t, 1, commits)
	require.Equal(t, 1, replays)
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), m.SourceReference)
	require.NotContains(t, string(encoded), m.DisplayName)
	require.NotContains(t, string(encoded), m.Fingerprint)
}

func TestShrimpAuditFailureRollsBackBusinessAndRetainsUncertainty(t *testing.T) {
	s, d := pilotStore(t)
	m := pilotIntent(t, d, "create_subject", nil)
	ctx, attempt := auditIntent(t, d, m)
	_, err := d.db.ExecContext(ctx, `CREATE TRIGGER audit_failure BEFORE INSERT ON shrimp_audit_event
		WHEN NEW.kind='commit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`)
	require.NoError(t, err)
	_, err = s.ApplyShrimp(ctx, m)
	require.ErrorContains(t, err, "audit unavailable")
	var count int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_subject").Scan(&count))
	require.Zero(t, count)
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_operation").Scan(&count))
	require.Zero(t, count)
	require.NoError(t, d.FinishShrimpAudit(ctx, *attempt))
	page, err := d.InspectShrimpAudit(ctx, "", 0, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Unresolved)
	require.Len(t, page.Events, 2)
}

func TestShrimpAuditCapacityReservesAcceptedOutcomeAndInspection(t *testing.T) {
	s, d := pilotStore(t)
	m := pilotIntent(t, d, "create_subject", nil)
	ctx, _ := auditIntent(t, d, m)
	_, err := d.db.ExecContext(ctx, `WITH RECURSIVE slots(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM slots WHERE n < ?)
		INSERT INTO shrimp_audit_attempt(id) SELECT 'occupied-'||n FROM slots`, shrimpAuditMaxAttempts-1)
	require.NoError(t, err)
	_, err = d.BeginShrimpAudit(ctx, "hr", "authority", "mutation")
	require.ErrorContains(t, err, "audit_capacity")
	result, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	require.Empty(t, result.Error)
	recovered, err := d.ShrimpResult(ctx, m.Principal, m.Window, m.ID)
	require.NoError(t, err)
	require.Equal(t, result, recovered)
	_, err = d.InspectShrimpAudit(ctx, "", 0, 100)
	require.NoError(t, err)
}

func TestShrimpAuditPaginationRestartAndLegacyBoundary(t *testing.T) {
	s, d := pilotStore(t)
	m := pilotIntent(t, d, "create_subject", nil)
	ctx, _ := auditIntent(t, d, m)
	_, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	first, err := d.InspectShrimpAudit(ctx, "", 0, 2)
	require.NoError(t, err)
	require.True(t, first.More)
	require.False(t, first.LegacyGap)
	require.NoError(t, s.Close())
	driver, err := NewDB(d.profile)
	require.NoError(t, err)
	reopened := driver.(*DB)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.ConfigureShrimp(ctx, "https://pilot.example/shrimp/v1/tenants/acme/domains/A"))
	next, err := reopened.InspectShrimpAudit(ctx, first.Epoch, first.Next, 100)
	require.NoError(t, err)
	require.Equal(t, first.Epoch, next.Epoch)
	require.False(t, next.More)
	require.Greater(t, next.Events[0].Sequence, first.Next)
	for _, epoch := range []string{"wrong", ""} {
		_, err := reopened.InspectShrimpAudit(ctx, epoch, first.Next, 1)
		require.ErrorContains(t, err, "invalid_cursor")
	}
	// Upgrading existing operation history must not manufacture pre-audit coverage.
	_, err = reopened.db.ExecContext(ctx, "DELETE FROM shrimp_audit_state")
	require.NoError(t, err)
	require.NoError(t, reopened.configureShrimpAudit(ctx))
	legacy, err := reopened.InspectShrimpAudit(ctx, "", 0, 100)
	require.NoError(t, err)
	require.True(t, legacy.LegacyGap)
	require.NotEqual(t, first.Epoch, legacy.Epoch)
}

func TestShrimpAuditRejectedTargetAndNativeDeleteNoOp(t *testing.T) {
	s, d := pilotStore(t)
	m := pilotIntent(t, d, "create_subject", nil)
	ctx, _ := auditIntent(t, d, m)
	created, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	stale := pilotIntent(t, d, "activate", &created.Subject)
	stale.ExpectedRevision = "obsolete"
	ctx, failed := auditIntent(t, d, stale)
	result, err := s.ApplyShrimp(ctx, stale)
	require.NoError(t, err)
	require.NotEmpty(t, result.Error)
	retire := pilotIntent(t, d, "retire", &created.Subject)
	ctx, _ = auditIntent(t, d, retire)
	_, err = s.ApplyShrimp(ctx, retire)
	require.NoError(t, err)
	for range 2 {
		ctx, err := s.BeginShrimpNativeAudit(t.Context(), "memos-user:1", "delete_native_account")
		require.NoError(t, err)
		require.NoError(t, s.IdentifyShrimpNativeAudit(ctx, created.Subject.UserID))
		_, err = s.DeleteUser(ctx, &store.DeleteUser{ID: created.Subject.UserID})
		require.NoError(t, err)
		require.NoError(t, s.FinishShrimpNativeAudit(ctx, "OK", false))
	}
	page, err := d.InspectShrimpAudit(t.Context(), "", 0, 100)
	require.NoError(t, err)
	require.Zero(t, page.Unresolved)
	deletes, noOps, rejected := 0, 0, 0
	for _, event := range page.Events {
		if event.AttemptID == failed.AttemptID && event.Kind == "outcome" {
			rejected++
			require.Equal(t, created.Subject.ID, event.SubjectID)
			require.Equal(t, created.Subject.Revision, event.Revision)
		}
		if event.Action == "delete_native_account" && event.Kind == "commit" {
			deletes++
		}
		if event.Code == "already_absent" {
			noOps++
			require.Equal(t, "not_committed", event.Commit)
		}
	}
	require.Equal(t, 1, rejected)
	require.Equal(t, 1, deletes)
	require.Equal(t, 1, noOps)
}
