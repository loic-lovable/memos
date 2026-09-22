package shrimp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/lovablelabs/shrimp-protocol/sdk/go/server"
	"github.com/lovablelabs/shrimp-protocol/sdk/go/server/servertest"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

func TestSDKContractAgainstMemos(t *testing.T) {
	servertest.Run(t, func(t *testing.T) servertest.Fixture {
		p := &profile.Profile{Driver: "sqlite", Data: t.TempDir(), Version: "0.31.0"}
		p.DSN = filepath.Join(p.Data, "memos.db")
		var current *store.Store
		open := func(t *testing.T) sdk.Application {
			driver, err := sqlite.NewDB(p)
			require.NoError(t, err)
			current = store.New(driver, p)
			require.NoError(t, current.Migrate(t.Context()))
			require.NoError(t, current.EnableShrimpPilot(t.Context(), "sdk-contract-fixture"))
			policy := driver.(interface {
				ConfigureShrimpPolicy(context.Context, string, bool) error
			})
			require.NoError(t, policy.ConfigureShrimpPolicy(t.Context(), strings.Repeat("a", 64), true))
			return &application{handler: &Handler{store: current, driver: driver.(store.ShrimpDriver)}}
		}
		app := open(t)
		t.Cleanup(func() { require.NoError(t, current.Close()) })
		return servertest.Fixture{Application: app, Principal: "hr", Authority: "hr-authority", AtomicSubjectUpdates: true,
			Restart: func(t *testing.T) sdk.Application { require.NoError(t, current.Close()); return open(t) }}
	})
}
