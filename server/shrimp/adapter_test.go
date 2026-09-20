package shrimp

import (
	"context"
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
