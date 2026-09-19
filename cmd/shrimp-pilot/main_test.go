package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

type blockedFlush struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w *blockedFlush) Flush() { close(w.entered); <-w.release; w.ResponseRecorder.Flush() }

func TestAdmissionTransportFlushesBeforeDisableCompletes(t *testing.T) {
	p := &profile.Profile{Driver: "sqlite", Data: t.TempDir(), Version: "0.31.0"}
	p.DSN = filepath.Join(p.Data, "memos.db")
	d, err := sqlite.NewDB(p)
	require.NoError(t, err)
	s := store.New(d, p)
	defer s.Close()
	ctx := t.Context()
	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.EnableShrimpPilot(ctx, "https://pilot.example/shrimp/v1/tenants/acme/domains/A"))
	driver := d.(store.ShrimpDriver)
	mutation := func(action string, subject store.ShrimpSubject) store.ShrimpMutation {
		window, deadline, err := driver.ShrimpWindow(ctx, "hr")
		require.NoError(t, err)
		return store.ShrimpMutation{Principal: "hr", Window: window, ID: random.UUID(), Fingerprint: random.UUID(), Action: action, SourceReference: random.UUID(), DisplayName: "Pilot", SubjectID: subject.ID, ExpectedRevision: subject.Revision, Deadline: deadline - 1, CommandID: "c1"}
	}
	created, err := s.ApplyShrimp(ctx, mutation("create_subject", store.ShrimpSubject{}))
	require.NoError(t, err)
	active, err := s.ApplyShrimp(ctx, mutation("activate", created.Subject))
	require.NoError(t, err)
	ticket, err := s.AdmissionTicket(ctx, active.Subject.UserID)
	require.NoError(t, err)
	var publicationErr error
	handler := admissionTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicationErr = s.PublishAdmission(r.Context(), active.Subject.UserID, ticket, func(*store.User) error { _, err := w.Write([]byte("credential")); return err })
	}))
	writer := &blockedFlush{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, httptest.NewRequest("POST", "https://pilot.example/login", nil))
		close(finished)
	}()
	<-writer.entered
	disabled := make(chan error, 1)
	intent := mutation("disable", active.Subject)
	go func() { _, err := s.ApplyShrimp(ctx, intent); disabled <- err }()
	select {
	case <-disabled:
		t.Error("disable finished before the credential flush")
	case <-time.After(30 * time.Millisecond):
	}
	close(writer.release)
	<-finished
	require.NoError(t, publicationErr)
	select {
	case err := <-disabled:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("disable remained blocked after flush")
	}
}
