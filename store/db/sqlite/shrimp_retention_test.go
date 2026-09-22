package sqlite

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func TestShrimpRetentionBoundaryAndRestart(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	original, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Empty(t, original.Error)
	var originalEvents int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_event").Scan(&originalEvents))

	// Advance the collector, not the system clock. At the exact advertised
	// retention boundary the result must still be available after window closure.
	require.NoError(t, d.collectShrimpHistory(ctx, original.RetainedUntil))
	recovered, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, original, recovered)
	require.NoError(t, d.collectShrimpHistory(ctx, original.RetainedUntil+1))
	_, err = d.ShrimpResult(ctx, intent.Principal, intent.Window, intent.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)

	// The real clock is now earlier than collection. Reopening must not
	// reactivate the deleted window or execute the collected intent again.
	require.NoError(t, s.Close())
	driver, err := NewDB(d.profile)
	require.NoError(t, err)
	reopened := driver.(*DB)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	_, err = reopened.ApplyShrimp(ctx, intent)
	require.ErrorIs(t, err, store.ErrShrimpOperationResultUnavailable)
	for table, expected := range map[string]int{"user": 1, "shrimp_subject": 1, "shrimp_event": originalEvents} {
		var count int
		require.NoError(t, reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
		require.Equal(t, expected, count, table)
	}
}

func TestShrimpRetentionRequiresClosedWindow(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	original, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	// Defensive fixture: even an inconsistent early expiry must not allow
	// deletion of a result whose execution window is still open.
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_operation SET retained_until=?", time.Now().Unix()-1)
	require.NoError(t, err)
	require.NoError(t, d.collectShrimpHistory(ctx, time.Now().Unix()))
	recovered, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, original, recovered)
}

func TestShrimpRetentionReclaimsCapacityWithoutEvictingProtectedResults(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	original, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	// Fill capacity with retained historical results, without creating 10,000
	// business mutations or confusing operation capacity with audit capacity.
	_, err = d.db.ExecContext(ctx, `WITH RECURSIVE entries(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM entries WHERE n<9999
	) INSERT INTO shrimp_operation(principal,window_id,id,fingerprint,result,retained_until)
	SELECT 'history','closed-window',CAST(n AS TEXT),'fingerprint','{}',? FROM entries`, original.RetainedUntil)
	require.NoError(t, err)
	next := pilotIntent(t, d, "create_subject", nil)
	_, err = s.ApplyShrimp(ctx, next)
	require.ErrorIs(t, err, store.ErrShrimpHistoryCapacity)
	recovered, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, original, recovered, "retries work even at capacity")
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_operation SET retained_until=? WHERE principal='history'", time.Now().Unix()-1)
	require.NoError(t, err)
	created, err := s.ApplyShrimp(ctx, next)
	require.NoError(t, err)
	require.Empty(t, created.Error)
	var count int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_operation").Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user").Scan(&count))
	require.Equal(t, 2, count)
}

func TestShrimpWindowCollectsExpiredResults(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	_, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	past := time.Now().Unix() - 1
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_window SET closes_at=?", past)
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_operation SET retained_until=?", past)
	require.NoError(t, err)
	_, _, err = d.ShrimpWindow(ctx, intent.Principal)
	require.NoError(t, err)
	_, err = d.ShrimpResult(ctx, intent.Principal, intent.Window, intent.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = s.ApplyShrimp(ctx, intent)
	require.ErrorIs(t, err, store.ErrShrimpOperationResultUnavailable)
}

func TestShrimpRejectedReplayCommitsWindowClosure(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	_, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	past := time.Now().Unix() - 1
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_window SET closes_at=?", past)
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_operation SET retained_until=?", past)
	require.NoError(t, err)
	_, err = s.ApplyShrimp(ctx, intent)
	require.ErrorIs(t, err, store.ErrShrimpOperationResultUnavailable)
	var count int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_window").Scan(&count))
	require.Zero(t, count, "mutation rejection must not roll back permanent window closure")
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_operation").Scan(&count))
	require.Zero(t, count)
}
