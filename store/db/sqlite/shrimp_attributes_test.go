package sqlite

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/usememos/memos/store"
)

func TestShrimpAttributesPersistExactValuesAndIndependentRevisions(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	create := pilotIntent(t, d, "create_subject", nil)
	create.Set = map[string]string{"displayName": "  Maya Å  ", "department": "Research", "email": "Maya+HR@Example.COM"}
	made, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Empty(t, made.Error)
	require.Equal(t, "Maya+HR@Example.COM", *made.Subject.Attributes["email"].Value)
	native, err := s.GetUser(ctx, &store.FindUser{ID: &made.Subject.UserID})
	require.NoError(t, err)
	require.Equal(t, "  Maya Å  ", native.Nickname)
	require.Empty(t, native.Email, "provisioned contact must not alter native email/login identity")
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &made.Subject))
	require.NoError(t, err)
	require.Empty(t, active.Error)
	require.NotEqual(t, made.Subject.Revision, active.Subject.Revision)
	require.Equal(t, made.Subject.Attributes, active.Subject.Attributes, "activation must preserve field revisions")
	update := pilotIntent(t, d, "update_subject", &active.Subject)
	update.Set = map[string]string{"department": ""}
	update.Clear = []string{"email", "displayName"}
	changed, err := s.ApplyShrimp(ctx, update)
	require.NoError(t, err)
	require.Empty(t, changed.Error)
	require.Nil(t, changed.Subject.Attributes["email"].Value)
	require.NotNil(t, changed.Subject.Attributes["department"].Value)
	require.Empty(t, *changed.Subject.Attributes["department"].Value)
	require.Equal(t, "hr-authority", changed.Subject.Attributes["email"].Authority)
	native, err = s.GetUser(ctx, &store.FindUser{ID: &made.Subject.UserID})
	require.NoError(t, err)
	require.Empty(t, native.Nickname)
	disabled, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "disable", &changed.Subject))
	require.NoError(t, err)
	require.Empty(t, disabled.Error)
	require.Equal(t, changed.Subject.Attributes, disabled.Subject.Attributes)
	retained, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Equal(t, made, retained, "old receipt cannot be reconstructed from cleared facts")
	before := disabled.Subject
	require.NoError(t, s.Close())
	driver, err := NewDB(d.profile)
	require.NoError(t, err)
	reopened := driver.(*DB)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	observed, _, err := reopened.ReadShrimp(ctx, before.ID, nil)
	require.NoError(t, err)
	require.Equal(t, before, *observed)
	page, err := reopened.EnumerateShrimp(ctx, store.ShrimpEnumeration{Principal: "hr", Scope: "resource", Authorization: "hr-authority", Epoch: "e1", Selection: "subjects", Types: []string{"subject"}, Visible: true, PageSize: 10}, enumerationFits)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	require.Equal(t, before.Attributes, page.Records[0].Subject.Attributes)
}

func TestShrimpAttributeChangesRejectWholly(t *testing.T) {
	for _, test := range []struct {
		name    string
		set     map[string]string
		clear   []string
		stale   bool
		foreign bool
	}{
		{name: "unknown field", set: map[string]string{"displayName": "changed", "name": "unsupported"}},
		{name: "overlap", set: map[string]string{"displayName": "changed"}, clear: []string{"displayName"}},
		{name: "duplicate clear", set: map[string]string{}, clear: []string{"displayName", "displayName"}},
		{name: "stale revision", set: map[string]string{"displayName": "changed"}, stale: true},
		{name: "foreign owner", set: map[string]string{"displayName": "changed", "department": "unauthorized"}, foreign: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, d := pilotStore(t)
			ctx := t.Context()
			created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
			require.NoError(t, err)
			subject := created.Subject
			if test.foreign {
				value := "owned"
				subject.Attributes["department"] = store.ShrimpScalarFact{Value: &value, Authority: "other", Revision: "field-r1"}
				raw, err := json.Marshal(subject.Attributes)
				require.NoError(t, err)
				_, err = d.db.ExecContext(ctx, "UPDATE shrimp_subject SET attributes=? WHERE id=?", string(raw), subject.ID)
				require.NoError(t, err)
			}
			before, _, err := d.ReadShrimp(ctx, subject.ID, nil)
			require.NoError(t, err)
			update := pilotIntent(t, d, "update_subject", before)
			update.Set = test.set
			update.Clear = test.clear
			if test.stale {
				update.ExpectedRevision = "stale"
			}
			rejected, err := s.ApplyShrimp(ctx, update)
			require.NoError(t, err)
			require.Equal(t, "mutation_rejected", rejected.Error)
			after, _, err := d.ReadShrimp(ctx, subject.ID, nil)
			require.NoError(t, err)
			require.Equal(t, before, after)
			native, err := s.GetUser(ctx, &store.FindUser{ID: &subject.UserID})
			require.NoError(t, err)
			require.Equal(t, "Pilot", native.Nickname)
			retry, err := s.ApplyShrimp(ctx, update)
			require.NoError(t, err)
			require.Equal(t, rejected, retry)
		})
	}
}

func TestShrimpCreationAllowsAbsentFactsAndNativeWritesStayFenced(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	create := pilotIntent(t, d, "create_subject", nil)
	create.DisplayName = ""
	create.Set = map[string]string{}
	made, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Empty(t, made.Error)
	require.NotNil(t, made.Subject.Attributes)
	require.Empty(t, made.Subject.Attributes)
	name := "native bypass"
	_, err = s.UpdateUser(ctx, &store.UpdateUser{ID: made.Subject.UserID, Nickname: &name})
	require.ErrorIs(t, err, store.ErrShrimpManagedWrite)
}

func TestShrimpLegacyOwnerResolutionPreservesFactsAndRetainedResult(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	create := pilotIntent(t, d, "create_subject", nil)
	created, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	name := created.Subject.Attributes["displayName"]
	legacy := name
	legacy.Authority = "" // The migration's fixed-enrollment sentinel.
	raw, err := json.Marshal(map[string]store.ShrimpScalarFact{"displayName": legacy})
	require.NoError(t, err)
	_, err = d.db.ExecContext(ctx, "UPDATE shrimp_subject SET attributes=? WHERE id=?", string(raw), created.Subject.ID)
	require.NoError(t, err)
	update := pilotIntent(t, d, "update_subject", &created.Subject)
	update.Set = map[string]string{"department": "Research"}
	changed, err := s.ApplyShrimp(ctx, update)
	require.NoError(t, err)
	require.Empty(t, changed.Error)
	got := changed.Subject.Attributes["displayName"]
	require.Equal(t, name.Value, got.Value)
	require.Equal(t, name.Revision, got.Revision)
	require.Empty(t, got.Authority, "omitted legacy facts keep their stored representation")
	native, err := s.GetUser(ctx, &store.FindUser{ID: &created.Subject.UserID})
	require.NoError(t, err)
	require.Equal(t, *name.Value, native.Nickname)
	clear := pilotIntent(t, d, "update_subject", &changed.Subject)
	clear.Set = map[string]string{}
	clear.Clear = []string{"displayName"}
	cleared, err := s.ApplyShrimp(ctx, clear)
	require.NoError(t, err)
	require.Empty(t, cleared.Error)
	got = cleared.Subject.Attributes["displayName"]
	require.Nil(t, got.Value)
	require.Equal(t, name.Authority, got.Authority, "changed legacy owner resolves through the fixed enrollment")
	require.Equal(t, cleared.Subject.Revision, got.Revision)
	retained, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Equal(t, created, retained)
}
