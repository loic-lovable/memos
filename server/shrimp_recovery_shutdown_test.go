package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	apiv1 "github.com/usememos/memos/server/api/v1"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

func recoveryShutdownServer(t *testing.T) *Server {
	t.Helper()
	p := &profile.Profile{Driver: "sqlite", Data: t.TempDir(), ShrimpRecoveryGuard: true}
	p.DSN = filepath.Join(p.Data, "memos.db")
	driver, err := sqlite.NewDB(p)
	require.NoError(t, err)
	require.NoError(t, driver.GetDB().PingContext(t.Context()))
	st := store.New(driver, p)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return &Server{Profile: p, Store: st, apiV1Service: apiv1.NewAPIV1Service("test", p, st)}
}

func TestRecoveryShutdownRefusesHeldConnection(t *testing.T) {
	s := recoveryShutdownServer(t)
	db := s.Store.GetDriver().GetDB()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.ErrorContains(t, s.Shutdown(t.Context()), "recovery checkpoint refused")
	// Closing the pool does not close a connection already checked out of it.
	require.Equal(t, 1, db.Stats().OpenConnections)
	require.NoError(t, conn.PingContext(t.Context()))
	require.NoError(t, conn.Close())
	require.Zero(t, db.Stats().OpenConnections)
}

func TestRecoveryShutdownRefusesHeldTransaction(t *testing.T) {
	s := recoveryShutdownServer(t)
	db := s.Store.GetDriver().GetDB()
	_, err := db.ExecContext(t.Context(), "CREATE TABLE shutdown_probe (value INTEGER)")
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	require.ErrorContains(t, s.Shutdown(t.Context()), "recovery checkpoint refused")
	// This transaction can still publish a write after sql.DB.Close returned.
	// A clean checkpoint at that boundary would therefore certify moving state.
	_, err = tx.ExecContext(t.Context(), "INSERT INTO shutdown_probe VALUES (1)")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Zero(t, db.Stats().OpenConnections)
}

func TestRecoveryShutdownPropagatesFailedHTTPDrain(t *testing.T) {
	s := recoveryShutdownServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() { close(release) })
	s.httpServer = httpServer.Config
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := httpServer.Client().Get(httpServer.URL)
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the server")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.Shutdown(ctx), context.Canceled)
	require.Zero(t, s.Store.GetDriver().GetDB().Stats().OpenConnections)
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("forced HTTP closure did not release the client")
	}
}

func TestRecoveryShutdownAcceptsDrainedDatabase(t *testing.T) {
	s := recoveryShutdownServer(t)
	require.NoError(t, s.Shutdown(t.Context()))
	require.Zero(t, s.Store.GetDriver().GetDB().Stats().OpenConnections)
}
