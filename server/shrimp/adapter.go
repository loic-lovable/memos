package shrimp

import (
	"context"
	"database/sql"
	"fmt"

	sdk "github.com/lovablelabs/shrimp-protocol/sdk/go/server"
	"github.com/pkg/errors"

	"github.com/usememos/memos/store"
)

// application keeps native storage, enrollment and admission coordination in Memos.
// Apply must go through Store, whose lock covers native writes and credential
// publication, rather than calling the SQLite driver's mutation method directly.
type application struct{ handler *Handler }

var _ sdk.Application = (*application)(nil)

func adapterError(err error) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sdk.ErrNotFound
	case errors.Is(err, store.ErrShrimpProofReplay):
		return sdk.ErrProofReplay
	case errors.Is(err, store.ErrShrimpProofStorage):
		return sdk.ErrProofStorage
	case errors.Is(err, store.ErrShrimpHistoryCapacity):
		return sdk.ErrCapacity
	case errors.Is(err, store.ErrShrimpWindowQuota):
		return sdk.ErrWindowQuota
	}
	for _, mapping := range []struct{ native, sdk error }{
		{store.ErrShrimpReplayConflict, sdk.ErrReplayConflict},
		{store.ErrShrimpOperationResultUnavailable, sdk.ErrOperationResultUnavailable},
		{store.ErrShrimpUnsupportedProfile, sdk.ErrUnsupportedProfile},
		{store.ErrShrimpInsufficientScope, sdk.ErrInsufficientScope},
		{store.ErrShrimpExecutionDeadlineExpired, sdk.ErrExecutionDeadlineExpired},
		{store.ErrShrimpInvalidDependency, sdk.ErrInvalidDependency},
	} {
		if errors.Is(err, mapping.native) {
			// Preserve native context for internal diagnostics. The SDK emits only
			// its bounded public category, never this wrapped error text.
			return fmt.Errorf("%w: %w", mapping.sdk, err)
		}
	}
	var enumeration store.ShrimpEnumerationError
	if errors.As(err, &enumeration) {
		return sdk.EnumerationError(enumeration)
	}
	return err
}

func subject(s store.ShrimpSubject) sdk.Subject {
	var attributes map[string]sdk.ScalarFact
	if s.Attributes != nil {
		attributes = map[string]sdk.ScalarFact{}
		for name, fact := range s.Attributes {
			attributes[name] = sdk.ScalarFact{Value: fact.Value, Authority: fact.Authority, Revision: fact.Revision}
		}
	}
	return sdk.Subject{ID: s.ID, SourceID: s.SourceID, SourceRevision: s.SourceRevision,
		SourceReference: s.SourceReference, Revision: s.Revision, Lifecycle: s.Lifecycle, DisplayName: s.DisplayName, Attributes: attributes}
}

func result(r *store.ShrimpResult) *sdk.Result {
	if r == nil {
		return nil
	}
	return &sdk.Result{AuditAttempt: r.AuditAttempt, Subject: subject(r.Subject), Token: r.Token, Time: r.Time,
		RetainedUntil: r.RetainedUntil, Action: r.Action, Error: r.Error, CommandID: r.CommandID}
}

func (a *application) Window(ctx context.Context, principal string) (string, int64, error) {
	id, expires, err := a.handler.driver.ShrimpWindow(ctx, principal)
	return id, expires, adapterError(err)
}

func (a *application) Apply(ctx context.Context, m sdk.Mutation) (*sdk.Result, error) {
	h := a.handler
	if h.Fault != nil {
		ctx = store.WithShrimpFault(ctx, func(point string) { h.Fault(m.ID, point) })
	}
	r, err := h.store.ApplyShrimp(ctx, store.ShrimpMutation{
		Principal: m.Principal, Window: m.Window, ID: m.ID, Fingerprint: m.Fingerprint,
		Action: m.Action, SubjectID: m.SubjectID, ExpectedRevision: m.ExpectedRevision,
		SourceReference: m.SourceReference, DisplayName: m.DisplayName, Authority: m.Authority, Set: m.Set, Clear: m.Clear, SSOProvider: h.config.SSOProvider,
		Deadline: m.Deadline, Dependencies: m.Dependencies, CommandID: m.CommandID,
		RecoverOnly: m.RecoverOnly, UnsupportedProfiles: m.UnsupportedProfiles,
	})
	return result(r), adapterError(err)
}

func (a *application) Result(ctx context.Context, principal, window, id string) (*sdk.Result, error) {
	r, err := a.handler.driver.ShrimpResult(ctx, principal, window, id)
	return result(r), adapterError(err)
}

func (a *application) Read(ctx context.Context, id string, deps []string) (*sdk.Subject, string, error) {
	s, frontier, err := a.handler.driver.ReadShrimp(ctx, id, deps)
	if err != nil {
		return nil, frontier, adapterError(err)
	}
	value := subject(*s)
	return &value, frontier, nil
}

func page(p *store.ShrimpEnumerationPage) *sdk.EnumerationPage {
	if p == nil {
		return nil
	}
	records := make([]sdk.Record, 0, len(p.Records))
	for _, r := range p.Records {
		records = append(records, sdk.Record{Type: r.Type, ID: r.ID, Subject: subject(r.Subject)})
	}
	return &sdk.EnumerationPage{Records: records, Frontier: p.Frontier, Cursor: p.Cursor, ExpiresAt: p.ExpiresAt}
}

func (a *application) Enumerate(ctx context.Context, e sdk.Enumeration, fits func(*sdk.EnumerationPage) (bool, error)) (*sdk.EnumerationPage, error) {
	p, err := a.handler.driver.EnumerateShrimp(ctx, store.ShrimpEnumeration{
		Principal: e.Principal, Scope: e.Scope, Authorization: e.Authorization, Epoch: e.Epoch,
		Selection: e.Selection, Cursor: e.Cursor, Types: e.Types, Visible: e.Visible,
		PageSize: e.PageSize, Dependencies: e.Dependencies,
	}, func(candidate *store.ShrimpEnumerationPage) (bool, error) { return fits(page(candidate)) })
	return page(p), adapterError(err)
}

func (a *application) ConsumeProof(ctx context.Context, id string, expires int64) error {
	return adapterError(a.handler.driver.ConsumeShrimpProof(ctx, id, expires))
}

func (a *application) BeginAudit(ctx context.Context, principal, authority, action string) (context.Context, error) {
	event, err := a.handler.driver.(store.ShrimpAuditDriver).BeginShrimpAudit(ctx, principal, authority, action)
	if err != nil {
		return nil, err
	}
	return store.WithShrimpAudit(ctx, event), nil
}

func (a *application) IdentifyAudit(ctx context.Context, window, operation string) error {
	event := store.ShrimpAuditFromContext(ctx)
	if event == nil {
		return errors.New("missing SHRIMP audit context")
	}
	event.Window, event.Operation = window, operation
	return a.handler.driver.(store.ShrimpAuditDriver).IdentifyShrimpAudit(ctx, *event)
}

func (a *application) FinishAudit(ctx context.Context, outcome sdk.AuditOutcome) error {
	event := store.ShrimpAuditFromContext(ctx)
	if event == nil {
		return errors.New("missing SHRIMP audit context")
	}
	event.Stage, event.Code = outcome.Stage, outcome.Code
	if outcome.Commit != "" {
		event.Commit = outcome.Commit
	}
	return a.handler.driver.(store.ShrimpAuditDriver).FinishShrimpAudit(ctx, *event)
}
