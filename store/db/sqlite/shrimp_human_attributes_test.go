package sqlite

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func humanIntent(t *testing.T, d *DB, action string, s *store.ShrimpSubject, raw string) store.ShrimpMutation {
	t.Helper()
	m := pilotIntent(t, d, action, s)
	p, err := humanattributes.New(humanattributes.Config{MaxEmails: 16, TZDBVersion: "2025b", Timezones: []string{"Europe/Stockholm", "US/Eastern"}})
	require.NoError(t, err)
	m.HumanProfile = p
	m.AttributeProfile = humanattributes.ID
	m.HumanAttributes = json.RawMessage(raw)
	return m
}

func TestShrimpHumanFactsPersistAndRetainEmailHistory(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	create := humanIntent(t, d, "create_subject", nil, `{"displayName":"  Maya Å  ","name":{"givenName":"Maya","familyName":""},"department":"Research","emails":[{"entry_id":"work","value":"Maya@Example.COM","primary":true,"expected_generation":null}],"locale":"sv-SE","timezone":"US/Eastern"}`)
	first, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Empty(t, first.Error)
	require.Nil(t, first.Subject.Attributes)
	require.Equal(t, humanattributes.ID, first.Subject.AttributeProfile)
	facts := first.Subject.HumanAttributes
	require.Equal(t, "US/Eastern", *facts.Timezone.Value)
	oldEmail := (*facts.Emails.Value)[0]
	native, err := s.GetUser(ctx, &store.FindUser{ID: &first.Subject.UserID})
	require.NoError(t, err)
	require.Equal(t, "  Maya Å  ", native.Nickname)
	require.Empty(t, native.Email)
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &first.Subject))
	require.NoError(t, err)
	require.Empty(t, active.Error)
	require.Equal(t, facts, active.Subject.HumanAttributes)
	update := humanIntent(t, d, "update_subject", &active.Subject, `{"set":{"emails":[]},"clear":["displayName"]}`)
	cleared, err := s.ApplyShrimp(ctx, update)
	require.NoError(t, err)
	require.Empty(t, cleared.Error)
	require.Nil(t, cleared.Subject.HumanAttributes.DisplayName.Value)
	require.NotNil(t, cleared.Subject.HumanAttributes.Emails.Value)
	require.Empty(t, *cleared.Subject.HumanAttributes.Emails.Value)
	for _, raw := range []string{
		`{"set":{"emails":[{"entry_id":"work","value":"new@example.com","primary":true,"expected_generation":null}]},"clear":[]}`,
		`{"set":{"department":"changed","emails":[{"entry_id":"work","value":"Maya@Example.COM","primary":true,"expected_generation":"` + oldEmail.Generation + `"}]},"clear":[]}`,
	} {
		rejected, err := s.ApplyShrimp(ctx, humanIntent(t, d, "update_subject", &cleared.Subject, raw))
		require.NoError(t, err)
		require.Equal(t, "revision_conflict", rejected.Error)
		observed, _, err := d.ReadShrimp(ctx, first.Subject.ID, nil)
		require.NoError(t, err)
		require.Equal(t, cleared.Subject, *observed)
	}
	// Withdrawing current support cannot erase a retained outcome.
	create.HumanProfile = nil
	create.UnsupportedProfiles = true
	retry, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Equal(t, first, retry)
	_, err = s.UpdateUser(ctx, &store.UpdateUser{ID: first.Subject.UserID, Nickname: new("forbidden")})
	require.ErrorIs(t, err, store.ErrShrimpManagedWrite)
	require.NoError(t, s.Close())
	driver, err := NewDB(d.profile)
	require.NoError(t, err)
	reopened := driver.(*DB)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	observed, _, err := reopened.ReadShrimp(ctx, first.Subject.ID, nil)
	require.NoError(t, err)
	require.Equal(t, cleared.Subject, *observed)
	page, err := reopened.EnumerateShrimp(ctx, store.ShrimpEnumeration{Principal: "hr", Scope: "resource", Authorization: "hr-authority", Epoch: "e1", Selection: "subjects", Types: []string{"subject"}, Visible: true, PageSize: 10}, enumerationFits)
	require.NoError(t, err)
	require.Equal(t, *observed, page.Records[0].Subject)
	var raw string
	require.NoError(t, reopened.db.QueryRowContext(ctx, "SELECT human_attributes FROM shrimp_subject WHERE id=?", first.Subject.ID).Scan(&raw))
	var state humanattributes.State
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	require.True(t, state.EntryIDs[oldEmail.EntryID])
	require.True(t, state.Generations[oldEmail.Generation])
	resultJSON, err := json.Marshal(retry)
	require.NoError(t, err)
	require.NotContains(t, string(resultJSON), "entry_ids")
}

func TestShrimpMigrationApprovalAndOldWriterFence(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	create := pilotIntent(t, d, "create_subject", nil)
	create.Set = map[string]string{"displayName": "Legacy", "email": "Exact@Example.COM", "department": "Research"}
	original, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Empty(t, original.Error)
	migrate := humanIntent(t, d, "migrate_human_attributes", &original.Subject, "")
	migrate.AttributeProfile = ""
	migrate.Migration = &store.ShrimpHumanMigration{EmailEntryID: new("legacy"), Authorization: "approval", ApprovalFingerprint: strings.Repeat("a", 64)}
	denied, err := s.ApplyShrimp(ctx, migrate)
	require.NoError(t, err)
	require.Equal(t, "forbidden", denied.Error)
	// A rejected operation stays rejected; approvals authorize a new exact intent.
	migrate = humanIntent(t, d, "migrate_human_attributes", &original.Subject, "")
	migrate.AttributeProfile = ""
	migrate.Migration = &store.ShrimpHumanMigration{EmailEntryID: new("legacy"), Authorization: "approval", ApprovalFingerprint: strings.Repeat("b", 64)}
	grant := store.ShrimpMigrationApproval{Authorization: "approval", Principal: migrate.Principal, Window: migrate.Window, Operation: migrate.ID, Fingerprint: migrate.Migration.ApprovalFingerprint, Owners: []string{migrate.Authority}, ExpiresAt: time.Now().Unix() + 300}
	require.NoError(t, d.AuthorizeShrimpAttributeMigration(ctx, grant))
	changed, err := s.ApplyShrimp(ctx, migrate)
	require.NoError(t, err)
	require.Empty(t, changed.Error)
	require.Equal(t, original.Subject.Attributes["displayName"].Revision, changed.Subject.HumanAttributes.DisplayName.Revision)
	require.Equal(t, "Exact@Example.COM", (*changed.Subject.HumanAttributes.Emails.Value)[0].Value)
	var consumed int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT consumed FROM shrimp_attribute_approval WHERE authorization='approval'").Scan(&consumed))
	require.Equal(t, 1, consumed)
	require.NoError(t, d.AuthorizeShrimpAttributeMigration(ctx, grant))
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT consumed FROM shrimp_attribute_approval WHERE authorization='approval'").Scan(&consumed))
	require.Equal(t, 1, consumed, "reloading a grant must not reset consumption")
	grant.ExpiresAt++
	require.Error(t, d.AuthorizeShrimpAttributeMigration(ctx, grant), "existing grants cannot be renewed")

	oldWrite := pilotIntent(t, d, "update_subject", &changed.Subject)
	oldWrite.Set = map[string]string{"email": "old@shape.test", "displayName": "Wrong"}
	oldWrite.HumanProfile = migrate.HumanProfile
	rejected, err := s.ApplyShrimp(ctx, oldWrite)
	require.NoError(t, err)
	require.Equal(t, "invalid_request", rejected.Error)
	observed, _, err := d.ReadShrimp(ctx, changed.Subject.ID, nil)
	require.NoError(t, err)
	require.Equal(t, changed.Subject, *observed)
	retry, err := s.ApplyShrimp(ctx, create)
	require.NoError(t, err)
	require.Equal(t, original, retry)
	migrate.HumanProfile = nil
	retry, err = s.ApplyShrimp(ctx, migrate)
	require.NoError(t, err)
	require.Equal(t, changed, retry)
}

func TestShrimpMigrationRollsBackApprovalAndPartialWrites(t *testing.T) {
	for _, test := range []struct {
		name, email string
		owners      []string
		fingerprint string
		want        string
	}{
		{"invalid scalar email", "", []string{"hr-authority"}, strings.Repeat("a", 64), "invalid_request"},
		{"missing owner approval", "valid@example.com", []string{"another-owner"}, strings.Repeat("a", 64), "forbidden"},
		{"different intent", "valid@example.com", []string{"hr-authority"}, strings.Repeat("b", 64), "forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, d := pilotStore(t)
			ctx := t.Context()
			create := pilotIntent(t, d, "create_subject", nil)
			create.Set = map[string]string{"displayName": "Before", "email": test.email}
			original, err := s.ApplyShrimp(ctx, create)
			require.NoError(t, err)
			m := humanIntent(t, d, "migrate_human_attributes", &original.Subject, "")
			m.AttributeProfile = ""
			m.Migration = &store.ShrimpHumanMigration{EmailEntryID: new("legacy"), Authorization: "grant", ApprovalFingerprint: strings.Repeat("a", 64)}
			require.NoError(t, d.AuthorizeShrimpAttributeMigration(ctx, store.ShrimpMigrationApproval{Authorization: "grant", Principal: m.Principal, Window: m.Window, Operation: m.ID, Fingerprint: test.fingerprint, Owners: test.owners, ExpiresAt: time.Now().Unix() + 300}))
			result, err := s.ApplyShrimp(ctx, m)
			require.NoError(t, err)
			require.Equal(t, test.want, result.Error)
			observed, _, err := d.ReadShrimp(ctx, original.Subject.ID, nil)
			require.NoError(t, err)
			require.Equal(t, original.Subject, *observed)
			var consumed int
			require.NoError(t, d.db.QueryRowContext(ctx, "SELECT consumed FROM shrimp_attribute_approval WHERE authorization='grant'").Scan(&consumed))
			require.Zero(t, consumed)
		})
	}
}

func TestShrimpCorruptHumanStateRefusesReadsAndLifecycle(t *testing.T) {
	for _, raw := range []string{
		` null `,
		`{"facts":{},"entry_ids":{},"generations":{},"unexpected":true}`,
		`{"facts":{"department":{"value":"Research","authority":"","revision":"r1"}},"entry_ids":null,"generations":null}`,
		`{"facts":{"emails":{"value":[{"entry_id":"work","value":"a@example.com","primary":true,"generation":"g1"}],"authority":"hr-authority","revision":"r1"}},"entry_ids":null,"generations":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s, d := pilotStore(t)
			ctx := t.Context()
			made, err := s.ApplyShrimp(ctx, humanIntent(t, d, "create_subject", nil, `{"displayName":"Before"}`))
			require.NoError(t, err)
			_, err = d.db.ExecContext(ctx, "UPDATE shrimp_subject SET human_attributes=? WHERE id=?", raw, made.Subject.ID)
			require.NoError(t, err)
			_, _, err = d.ReadShrimp(ctx, made.Subject.ID, nil)
			require.Error(t, err)
			_, err = d.EnumerateShrimp(ctx, store.ShrimpEnumeration{Principal: "hr", Scope: "resource", Authorization: "hr-authority", Epoch: "e1", Selection: "subjects", Types: []string{"subject"}, Visible: true, PageSize: 10}, enumerationFits)
			require.Error(t, err)
			m := pilotIntent(t, d, "activate", &made.Subject)
			_, err = s.ApplyShrimp(ctx, m)
			require.Error(t, err)
			var lifecycle, revision string
			require.NoError(t, d.db.QueryRowContext(ctx, "SELECT lifecycle,revision FROM shrimp_subject WHERE id=?", made.Subject.ID).Scan(&lifecycle, &revision))
			require.Equal(t, "disabled", lifecycle)
			require.Equal(t, made.Subject.Revision, revision)
			native, err := s.GetUser(ctx, &store.FindUser{ID: &made.Subject.UserID})
			require.NoError(t, err)
			require.Equal(t, store.Archived, native.RowStatus)
		})
	}
}

func TestShrimpMigrationApprovalMustStillBeValidAtCommit(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	made, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	m := humanIntent(t, d, "migrate_human_attributes", &made.Subject, "")
	m.AttributeProfile = ""
	m.Migration = &store.ShrimpHumanMigration{Authorization: "expires", ApprovalFingerprint: strings.Repeat("a", 64)}
	expiry := time.Now().Unix() + 2
	require.NoError(t, d.AuthorizeShrimpAttributeMigration(ctx, store.ShrimpMigrationApproval{Authorization: "expires", Principal: m.Principal, Window: m.Window, Operation: m.ID, Fingerprint: m.Migration.ApprovalFingerprint, Owners: []string{m.Authority}, ExpiresAt: expiry}))
	paused := store.WithShrimpFault(ctx, func(point string) {
		if point == "before_commit" {
			time.Sleep(time.Until(time.Unix(expiry, 0)))
		}
	})
	_, err = s.ApplyShrimp(paused, m)
	require.ErrorIs(t, err, store.ErrShrimpInsufficientScope)
	observed, _, err := d.ReadShrimp(ctx, made.Subject.ID, nil)
	require.NoError(t, err)
	require.Equal(t, made.Subject, *observed)
	var consumed, count int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT consumed FROM shrimp_attribute_approval WHERE authorization='expires'").Scan(&consumed))
	require.Zero(t, consumed)
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_operation WHERE id=?", m.ID).Scan(&count))
	require.Zero(t, count)
}
