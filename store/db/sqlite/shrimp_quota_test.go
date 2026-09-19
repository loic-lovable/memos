package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

func TestWindowQuotaPreservesExecutionAndRecovery(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	original, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Empty(t, original.Error)
	for range 127 {
		_, _, err := d.ShrimpWindow(ctx, intent.Principal)
		require.NoError(t, err)
	}
	_, _, err = d.ShrimpWindow(ctx, intent.Principal)
	require.ErrorIs(t, err, store.ErrShrimpWindowQuota)
	_, _, err = d.ShrimpWindow(ctx, "another-principal")
	require.NoError(t, err, "quota is per principal")

	// Existing open windows remain executable even when no new slot is available.
	next := intent
	next.ID, next.SourceReference, next.Fingerprint = random.UUID(), random.UUID(), random.UUID()
	created, err := s.ApplyShrimp(ctx, next)
	require.NoError(t, err)
	require.Empty(t, created.Error)

	// Expire one window in the disposable fixture without waiting five minutes.
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_window SET closes_at=? WHERE id=?", time.Now().Unix()-1, intent.Window)
	require.NoError(t, err)
	_, _, err = d.ShrimpWindow(ctx, intent.Principal)
	require.NoError(t, err, "expiry restores one quota slot")
	_, _, err = d.ShrimpWindow(ctx, intent.Principal)
	require.ErrorIs(t, err, store.ErrShrimpWindowQuota)

	recovered, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, original, recovered, "window expiry and collection must preserve original results")
	recovered, err = d.ShrimpResult(ctx, next.Principal, next.Window, next.ID)
	require.NoError(t, err)
	require.Equal(t, created, recovered)
}
