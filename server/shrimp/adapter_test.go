package shrimp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sdk "github.com/lovablelabs/shrimp-protocol/sdk/go/server"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

type auditDriver struct {
	store.ShrimpDriver
	store.ShrimpAuditDriver
	event                *store.ShrimpAudit
	identified, finished bool
}

func (d *auditDriver) BeginShrimpAudit(_ context.Context, actor, authority, action string) (*store.ShrimpAudit, error) {
	d.event = &store.ShrimpAudit{ID: "attempt", Actor: actor, Authority: authority, Action: action, Commit: "unknown"}
	return d.event, nil
}
func (d *auditDriver) IdentifyShrimpAudit(ctx context.Context, event store.ShrimpAudit) error {
	d.identified = store.ShrimpAuditFromContext(ctx) == d.event && event.Window == "window" && event.Operation == "operation"
	return nil
}
func (d *auditDriver) FinishShrimpAudit(ctx context.Context, event store.ShrimpAudit) error {
	d.finished = store.ShrimpAuditFromContext(ctx) == d.event && ctx.Err() == nil && event.Commit == "unknown" && event.Stage == "commit" && event.Code == "storage_unavailable"
	return nil
}

func TestAdapterPreservesNativeAuditContextAndUnknownOutcome(t *testing.T) {
	driver := &auditDriver{}
	app := &application{handler: &Handler{driver: driver}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempt, err := app.BeginAudit(ctx, "principal", "authority", "mutation")
	require.NoError(t, err)
	require.Same(t, driver.event, store.ShrimpAuditFromContext(attempt), "native transaction must see the exact durable attempt")
	require.NoError(t, app.IdentifyAudit(attempt, "window", "operation"))
	require.True(t, driver.identified)
	cancel()
	require.NoError(t, app.FinishAudit(context.WithoutCancel(attempt), sdk.AuditOutcome{Stage: "commit", Code: "storage_unavailable"}))
	require.True(t, driver.finished, "storage failures must remain unknown, even after request cancellation")
}

func TestAdapterMapsWrappedRequestFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		native, sdk error
	}{
		{"replay", store.ErrShrimpReplayConflict, sdk.ErrReplayConflict},
		{"result unavailable", store.ErrShrimpOperationResultUnavailable, sdk.ErrOperationResultUnavailable},
		{"profile", store.ErrShrimpUnsupportedProfile, sdk.ErrUnsupportedProfile},
		{"scope", store.ErrShrimpInsufficientScope, sdk.ErrInsufficientScope},
		{"deadline", store.ErrShrimpExecutionDeadlineExpired, sdk.ErrExecutionDeadlineExpired},
		{"dependency", store.ErrShrimpInvalidDependency, sdk.ErrInvalidDependency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := fmt.Errorf("private storage detail: %w", tc.native)
			mapped := adapterError(native)
			require.ErrorIs(t, mapped, tc.sdk)
			require.ErrorIs(t, mapped, native)
		})
	}
	require.NoError(t, adapterError(nil))
	unknown := errors.New("private storage outcome is unknown")
	require.Same(t, unknown, adapterError(unknown))
	lookalike := errors.New("replay_conflict")
	require.Same(t, lookalike, adapterError(lookalike), "the native adapter must classify by identity, not text")
	require.NotErrorIs(t, adapterError(lookalike), sdk.ErrReplayConflict)
}
