package sqlite

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func TestShrimpWithdrawalAndTrustPinSurviveStoreRecreation(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	m := pilotIntent(t, d, "create_subject", nil)
	created, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	next := pilotIntent(t, d, "activate", &created.Subject)
	require.NoError(t, d.ConfigureShrimpPolicy(ctx, key, false))
	again, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	require.Equal(t, created, again)
	_, err = s.ApplyShrimp(ctx, next)
	require.ErrorIs(t, err, store.ErrShrimpInsufficientScope)
	_, _, err = d.ShrimpWindow(ctx, "hr")
	require.ErrorIs(t, err, store.ErrShrimpInsufficientScope)
	reopened, err := NewDB(d.profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	other := reopened.(*DB)
	require.ErrorContains(t, other.ConfigureShrimpPolicy(ctx, key, true), "writer_reconciliation_required")
	require.ErrorContains(t, other.ConfigureShrimpPolicy(ctx, strings.Repeat("a", 64), false), "trust_change_unsupported")
	require.NoError(t, other.ConfigureShrimpPolicy(ctx, key, false))
	page, err := other.InspectShrimpAudit(ctx, "", 0, 100)
	require.NoError(t, err)
	require.Zero(t, page.Unresolved)
	codes := map[string]bool{}
	for _, event := range page.Events {
		codes[event.Code] = true
	}
	require.True(t, codes["writer_reconciliation_required"])
	require.True(t, codes["trust_change_unsupported"])
}
