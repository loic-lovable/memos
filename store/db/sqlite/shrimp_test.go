package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

func pilotStore(t *testing.T) (*store.Store, *DB) {
	t.Helper()
	p := &profile.Profile{Driver: "sqlite", Data: t.TempDir(), Version: "0.31.0"}
	p.DSN = filepath.Join(p.Data, "memos.db")
	driver, err := NewDB(p)
	require.NoError(t, err)
	s := store.New(driver, p)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Migrate(t.Context()))
	require.NoError(t, s.EnableShrimpPilot(t.Context(), "https://pilot.example/shrimp/v1/tenants/acme/domains/A"))
	return s, driver.(*DB)
}

func pilotIntent(t *testing.T, d *DB, action string, subject *store.ShrimpSubject) store.ShrimpMutation {
	t.Helper()
	window, close, err := d.ShrimpWindow(t.Context(), "hr")
	require.NoError(t, err)
	m := store.ShrimpMutation{Principal: "hr", Window: window, ID: random.UUID(), Fingerprint: random.UUID(), Action: action, Deadline: close - 1, SourceReference: random.UUID(), DisplayName: "Pilot", CommandID: "c1"}
	if subject != nil {
		m.SubjectID = subject.ID
		m.ExpectedRevision = subject.Revision
	}
	return m
}

func TestApplyShrimpAtomicCreationAndRetry(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	m := pilotIntent(t, d, "create_subject", nil)
	first, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	require.Empty(t, first.Error)
	users, err := d.ListUsers(ctx, &store.FindUser{ID: &first.Subject.UserID})
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, store.Archived, users[0].RowStatus)
	again, err := s.ApplyShrimp(ctx, m)
	require.NoError(t, err)
	require.Equal(t, first, again)
	changed := m
	changed.Fingerprint = "changed"
	_, err = s.ApplyShrimp(ctx, changed)
	require.ErrorContains(t, err, "replay_conflict")
	duplicate := pilotIntent(t, d, "create_subject", nil)
	duplicate.SourceReference = m.SourceReference
	failed, err := s.ApplyShrimp(ctx, duplicate)
	require.NoError(t, err)
	require.NotEmpty(t, failed.Error)
	var count int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user").Scan(&count))
	require.Equal(t, 1, count, "failed source correlation must roll back the native user")
	recovered, err := d.ShrimpResult(ctx, duplicate.Principal, duplicate.Window, duplicate.ID)
	require.NoError(t, err)
	require.Equal(t, failed, recovered)
}

func TestApplyShrimpCompetingRevisionsAndNativeOwnership(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	first := pilotIntent(t, d, "activate", &created.Subject)
	second := pilotIntent(t, d, "activate", &created.Subject)
	results := make(chan *store.ShrimpResult, 2)
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	for _, intent := range []store.ShrimpMutation{first, second} {
		workers.Go(func() { result, err := s.ApplyShrimp(ctx, intent); results <- result; failures <- err })
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	successes := 0
	for result := range results {
		if result.Error == "" {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	current, _, err := d.ReadShrimp(ctx, created.Subject.ID, nil)
	require.NoError(t, err)
	archived := store.Archived
	_, err = s.UpdateUser(ctx, &store.UpdateUser{ID: current.UserID, RowStatus: &archived})
	require.Error(t, err)
	user, _, err := d.ShrimpAdmission(ctx, current.UserID)
	require.NoError(t, err)
	require.Equal(t, store.Normal, user.RowStatus, "refused native lifecycle must roll back")
	password := "new native password hash"
	_, err = s.UpdateUser(ctx, &store.UpdateUser{ID: current.UserID, PasswordHash: &password})
	require.NoError(t, err)
	updated, _, err := d.ReadShrimp(ctx, current.ID, nil)
	require.NoError(t, err)
	require.NotEqual(t, current.Revision, updated.Revision)
	_, err = s.DeleteUser(ctx, &store.DeleteUser{ID: current.UserID})
	require.Error(t, err)
	_, _, err = d.ShrimpAdmission(ctx, current.UserID)
	require.NoError(t, err, "refused native deletion must preserve account")
}

func TestShrimpDisableAlreadyDisabledAndReplayAfterRestore(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	require.Empty(t, created.Error)
	intent := pilotIntent(t, d, "disable", &created.Subject)
	disabled, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Empty(t, disabled.Error)
	want := created.Subject
	want.Revision = disabled.Subject.Revision
	require.Equal(t, want, disabled.Subject, "disable preserves identity, source and attributes")
	require.NotEqual(t, created.Subject.Revision, disabled.Subject.Revision)
	_, err = s.AdmissionTicket(ctx, disabled.Subject.UserID)
	require.ErrorIs(t, err, store.ErrAdmissionDenied)

	stale, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &created.Subject))
	require.NoError(t, err)
	require.Equal(t, "mutation_rejected", stale.Error)
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &disabled.Subject))
	require.NoError(t, err)
	require.Empty(t, active.Error)
	again, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, disabled, again, "retry must recover the original result without disabling again")
	current, frontier, err := d.ReadShrimp(ctx, active.Subject.ID, nil)
	require.NoError(t, err)
	require.Equal(t, active.Subject, *current)
	require.Equal(t, active.Token, frontier, "retry must not append an event")
	_, err = s.AdmissionTicket(ctx, current.UserID)
	require.NoError(t, err)
}

func TestShrimpRetirementFencesAdmissionAndIsTerminal(t *testing.T) {
	for _, lifecycle := range []string{"active", "disabled"} {
		t.Run(lifecycle, func(t *testing.T) {
			s, d := pilotStore(t)
			ctx := t.Context()
			created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
			require.NoError(t, err)
			active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &created.Subject))
			require.NoError(t, err)
			require.Empty(t, active.Error)
			ticket, err := s.AdmissionTicket(ctx, active.Subject.UserID)
			require.NoError(t, err)
			before := active
			if lifecycle == "disabled" {
				before, err = s.ApplyShrimp(ctx, pilotIntent(t, d, "disable", &active.Subject))
				require.NoError(t, err)
				require.Empty(t, before.Error)
			}
			intent := pilotIntent(t, d, "retire", &before.Subject)
			stale := intent
			stale.ID, stale.Fingerprint, stale.ExpectedRevision = random.UUID(), random.UUID(), created.Subject.Revision
			refused, err := s.ApplyShrimp(ctx, stale)
			require.NoError(t, err)
			require.Equal(t, "mutation_rejected", refused.Error)
			retired, err := s.ApplyShrimp(ctx, intent)
			require.NoError(t, err)
			require.Empty(t, retired.Error)
			want := before.Subject
			want.Lifecycle, want.Revision = "retired", retired.Subject.Revision
			require.Equal(t, want, retired.Subject)
			require.NotEqual(t, before.Subject.Revision, retired.Subject.Revision)
			users, err := d.ListUsers(ctx, &store.FindUser{ID: &retired.Subject.UserID})
			require.NoError(t, err)
			require.Len(t, users, 1)
			require.Equal(t, store.Archived, users[0].RowStatus)
			publication, release := store.WithAdmissionScope(ctx)
			defer release()
			require.ErrorIs(t, s.PublishAdmission(publication, retired.Subject.UserID, ticket, func(*store.User) error {
				t.Fatal("pre-retirement attempt published credentials")
				return nil
			}), store.ErrAdmissionDenied)
			_, err = s.AdmissionTicket(ctx, retired.Subject.UserID)
			require.ErrorIs(t, err, store.ErrAdmissionDenied)
			for _, action := range []string{"activate", "disable", "retire", "update_subject"} {
				result, err := s.ApplyShrimp(ctx, pilotIntent(t, d, action, &retired.Subject))
				require.NoError(t, err)
				require.Equal(t, "mutation_rejected", result.Error, action)
			}
			again, err := s.ApplyShrimp(ctx, intent)
			require.NoError(t, err)
			require.Equal(t, retired, again)
			current, frontier, err := d.ReadShrimp(ctx, retired.Subject.ID, nil)
			require.NoError(t, err)
			require.Equal(t, retired.Subject, *current)
			require.Equal(t, retired.Token, frontier)
		})
	}
}

func TestShrimpActiveRetirementPreservesLastSpaceAdmin(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &created.Subject))
	require.NoError(t, err)
	require.Empty(t, active.Error)
	space, err := s.CreateSpace(ctx, &store.Space{UID: "pilot-space", Title: "Pilot space"}, active.Subject.UserID)
	require.NoError(t, err)
	intent := pilotIntent(t, d, "retire", &active.Subject)
	refused, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, "mutation_rejected", refused.Error)
	current, frontier, err := d.ReadShrimp(ctx, active.Subject.ID, nil)
	require.NoError(t, err)
	require.Equal(t, active.Subject, *current, "native guard must roll back retirement and revision")
	require.Equal(t, active.Token, frontier)
	_, err = s.AdmissionTicket(ctx, current.UserID)
	require.NoError(t, err, "rejected retirement must leave native admission intact")
	_, err = s.DeleteSpace(ctx, &store.DeleteSpace{ID: space.ID, ActorUserID: current.UserID})
	require.NoError(t, err)
	again, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, refused, again, "changed circumstances must not re-evaluate a retained rejection")
	retired, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "retire", current))
	require.NoError(t, err)
	require.Empty(t, retired.Error)
	require.Equal(t, "retired", retired.Subject.Lifecycle)
}

func TestShrimpAdmissionSurvivesStaleCacheAndRestore(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &created.Subject))
	require.NoError(t, err)
	_, err = s.GetUser(ctx, &store.FindUser{ID: &active.Subject.UserID})
	require.NoError(t, err) // Warm native cache.
	ticket, err := s.AdmissionTicket(ctx, active.Subject.UserID)
	require.NoError(t, err)
	disabled, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "disable", &active.Subject))
	require.NoError(t, err)
	_, err = s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &disabled.Subject))
	require.NoError(t, err)
	publication, release := store.WithAdmissionScope(ctx)
	defer release()
	called := false
	err = s.PublishAdmission(publication, active.Subject.UserID, ticket, func(*store.User) error { called = true; return nil })
	require.ErrorIs(t, err, store.ErrAdmissionDenied)
	require.False(t, called)
	fresh, err := s.AdmissionTicket(ctx, active.Subject.UserID)
	require.NoError(t, err)
	require.NoError(t, s.PublishAdmission(publication, active.Subject.UserID, fresh, func(*store.User) error { called = true; return nil }))
	require.True(t, called)
}

func TestShrimpDisableWaitsForTransportPublication(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", &created.Subject))
	require.NoError(t, err)
	ticket, err := s.AdmissionTicket(ctx, active.Subject.UserID)
	require.NoError(t, err)
	publication, release := store.WithAdmissionScope(ctx)
	defer release()
	require.NoError(t, s.PublishAdmission(publication, active.Subject.UserID, ticket, func(*store.User) error { return nil }))
	intent := pilotIntent(t, d, "disable", &active.Subject)
	finished := make(chan error, 1)
	go func() { _, err := s.ApplyShrimp(context.Background(), intent); finished <- err }()
	select {
	case <-finished:
		t.Fatal("disable completed before transport release")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("disable did not complete after transport release")
	}
}

func TestShrimpPasswordSnapshotAndTypedIDs(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	created, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	for _, action := range []string{"activate", "update_subject"} {
		intent := pilotIntent(t, d, action, &created.Subject)
		intent.SubjectID = created.Subject.SourceID
		result, err := s.ApplyShrimp(ctx, intent)
		require.NoError(t, err)
		require.NotEmpty(t, result.Error)
	}
	current, _, err := d.ReadShrimp(ctx, created.Subject.ID, nil)
	require.NoError(t, err)
	require.Equal(t, created.Subject, *current)
	active, err := s.ApplyShrimp(ctx, pilotIntent(t, d, "activate", current))
	require.NoError(t, err)
	user, _, err := d.ShrimpAdmission(ctx, current.UserID)
	require.NoError(t, err)
	old := "old hash"
	_, err = s.UpdateUser(ctx, &store.UpdateUser{ID: user.ID, PasswordHash: &old})
	require.NoError(t, err)
	snapshot, ticket, err := s.PasswordAdmission(ctx, user.Username)
	require.NoError(t, err)
	require.Equal(t, old, snapshot.PasswordHash)
	fresh := "new hash"
	_, err = s.UpdateUser(ctx, &store.UpdateUser{ID: user.ID, PasswordHash: &fresh})
	require.NoError(t, err)
	publication, release := store.WithAdmissionScope(ctx)
	defer release()
	require.ErrorIs(t, s.PublishAdmission(publication, active.Subject.UserID, ticket, func(*store.User) error { t.Fatal("stale password snapshot published"); return nil }), store.ErrAdmissionDenied)
	updated, newTicket, err := s.PasswordAdmission(ctx, user.Username)
	require.NoError(t, err)
	require.Equal(t, fresh, updated.PasswordHash)
	require.NotEqual(t, ticket, newTicket)
}

func TestShrimpStorageFailureDoesNotBecomeTerminalRejection(t *testing.T) {
	s, d := pilotStore(t)
	ctx := t.Context()
	intent := pilotIntent(t, d, "create_subject", nil)
	_, err := d.db.ExecContext(ctx, "DROP TABLE shrimp_event")
	require.NoError(t, err)
	_, err = s.ApplyShrimp(ctx, intent)
	require.Error(t, err)
	var accounts, results int
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user").Scan(&accounts))
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_operation").Scan(&results))
	require.Zero(t, accounts)
	require.Zero(t, results)
}
