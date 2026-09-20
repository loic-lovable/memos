package server

import (
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

func TestEnrolledDatabaseCannotStartWithoutEnforcement(t *testing.T) {
	p := &profile.Profile{Data: t.TempDir(), Driver: "sqlite", Version: "0.31.0"}
	p.DSN = filepath.Join(p.Data, "memos.db")
	driver, err := sqlite.NewDB(p)
	require.NoError(t, err)
	st := store.New(driver, p)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.Migrate(t.Context()))
	s := &Server{Profile: p, Store: st, echoServer: echo.New()}
	require.NoError(t, s.configureShrimp(t.Context()))
	require.NoError(t, st.EnableShrimpPilot(t.Context(), "fixed-enrollment"))
	require.ErrorContains(t, s.configureShrimp(t.Context()), "requires --shrimp-config")
}
