package sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func TestShrimpCompoundUpdatePublishesOneNativeStateAndHistoricalEffect(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "scalar", true: "typed"}[typed], func(t *testing.T) {
			s, d := pilotStore(t)
			ctx := t.Context()
			create := pilotIntent(t, d, "create_subject", nil)
			if typed {
				create = humanIntent(t, d, "create_subject", nil, `{"displayName":"Pilot"}`)
			}
			made, err := s.ApplyShrimp(ctx, create)
			require.NoError(t, err)
			require.Empty(t, made.Error)
			change := pilotIntent(t, d, "update_subject", &made.Subject)
			change.Set = map[string]string{"displayName": "Ready"}
			if typed {
				change = humanIntent(t, d, "update_subject", &made.Subject, `{"set":{"displayName":"Ready"},"clear":[]}`)
			}
			change.Lifecycle = "active"
			active, err := s.ApplyShrimp(ctx, change)
			require.NoError(t, err)
			require.Empty(t, active.Error)
			ticket, err := s.AdmissionTicket(ctx, active.Subject.UserID)
			require.NoError(t, err)
			var nativeName, nativeStatus string
			require.NoError(t, d.db.QueryRowContext(ctx, "SELECT nickname,row_status FROM user WHERE id=?", active.Subject.UserID).Scan(&nativeName, &nativeStatus))
			require.Equal(t, "Ready", nativeName)
			require.Equal(t, "NORMAL", nativeStatus)
			change = pilotIntent(t, d, "update_subject", &active.Subject)
			change.Set = map[string]string{"displayName": "Departed"}
			if typed {
				change = humanIntent(t, d, "update_subject", &active.Subject, `{"set":{"displayName":"Departed"},"clear":[]}`)
			}
			change.Lifecycle = "disabled"
			audit, err := d.BeginShrimpAudit(ctx, "hr", "hr-authority", "mutation")
			require.NoError(t, err)
			disabled, err := s.ApplyShrimp(store.WithShrimpAudit(ctx, audit), change)
			require.NoError(t, err)
			require.Empty(t, disabled.Error)
			require.Equal(t, "disabled", disabled.Lifecycle)
			require.Equal(t, "update_subject", disabled.Action)
			require.Equal(t, "Departed", disabled.Subject.DisplayName)
			require.NoError(t, d.db.QueryRowContext(ctx, "SELECT nickname,row_status FROM user WHERE id=?", active.Subject.UserID).Scan(&nativeName, &nativeStatus))
			require.Equal(t, "Departed", nativeName)
			require.Equal(t, "ARCHIVED", nativeStatus)
			restored, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &disabled.Subject))
			require.NoError(t, err)
			require.Empty(t, restored.Error)
			publication, release := store.WithAdmissionScope(ctx)
			defer release()
			require.ErrorIs(t, s.PublishAdmission(publication, active.Subject.UserID, ticket, func(*store.User) error { t.Fatal("old admission published"); return nil }), store.ErrAdmissionDenied)
			change.UnsupportedProfiles, change.RecoverOnly = true, true
			again, err := s.ApplyShrimp(ctx, change)
			require.NoError(t, err)
			require.Equal(t, disabled, again)
			page, err := d.InspectShrimpAudit(ctx, "", 0, 100)
			require.NoError(t, err)
			effects := 0
			for _, event := range page.Events {
				if event.Kind == "effect" && event.Effect == "admission_block_complete" {
					effects++
				}
			}
			require.Equal(t, 1, effects)
		})
	}
}

func TestShrimpCompoundRejectionRollsBackBothHalves(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	made, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &made.Subject))
	require.NoError(t, err)
	for _, target := range []string{"active", "invalid", "disabled"} {
		change := pilotIntent(t, d, "update_subject", &active.Subject)
		change.Lifecycle = target
		change.Set = map[string]string{"displayName": "must not persist"}
		if target == "disabled" {
			change.Set["unknown"] = "bad"
		}
		rejected, err := s.ApplyShrimp(ctx, change)
		require.NoError(t, err)
		require.NotEmpty(t, rejected.Error)
		current, _, err := d.ReadShrimp(ctx, active.Subject.ID, nil)
		require.NoError(t, err)
		require.Equal(t, active.Subject, *current)
		native, err := s.GetUser(ctx, &store.FindUser{ID: &current.UserID})
		require.NoError(t, err)
		require.Equal(t, "Pilot", native.Nickname)
		require.Equal(t, store.Normal, native.RowStatus)
	}
}
