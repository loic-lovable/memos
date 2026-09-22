//go:build shrimptest

package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

func TestControlsHoldDistinctRequestsUntilTheirOwnRelease(t *testing.T) {
	p := &profile.Profile{Driver: "sqlite", Data: t.TempDir(), Version: "0.31.0"}
	p.DSN = filepath.Join(p.Data, "memos.db")
	d, err := sqlite.NewDB(p)
	require.NoError(t, err)
	s := store.New(d, p)
	defer s.Close()
	ctx := t.Context()
	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.EnableShrimpPilot(ctx, "https://pilot.example/shrimp/v1/tenants/acme/domains/A"))
	window, deadline, err := d.(store.ShrimpDriver).ShrimpWindow(ctx, "hr")
	require.NoError(t, err)
	created, err := s.ApplyShrimp(ctx, store.ShrimpMutation{Authority: "hr-authority", Principal: "hr", Window: window, ID: "create", Fingerprint: "create", Action: "create_subject", SourceReference: "control-fixture", DisplayName: "Pilot", Deadline: deadline - 1, CommandID: "c1"})
	require.NoError(t, err)
	c := newControls(s, "private")
	arm := func() string {
		result, err := c.apply(ctx, "arm", created.Subject.ID, "session", "", "", "")
		require.NoError(t, err)
		return result.(map[string]any)["handle"].(string)
	}
	// An unclaimed barrier may be canceled after a request is rejected earlier.
	canceled := arm()
	_, err = c.apply(ctx, "resume", "", "", canceled, "", "")
	require.NoError(t, err)
	first := arm()
	_, err = c.apply(ctx, "arm", created.Subject.ID, "session", "", "", "")
	require.Error(t, err, "two unclaimed barriers would make assignment ambiguous")
	hold := func(handle string) <-chan error {
		done := make(chan error, 1)
		go func() { done <- c.beforePublication(ctx, created.Subject.UserID, "session") }()
		require.Eventually(t, func() bool {
			value, err := c.apply(ctx, "status", "", "", handle, "", "")
			return err == nil && value.(map[string]any)["held"] == true
		}, time.Second, time.Millisecond)
		return done
	}
	firstDone := hold(first)
	second := arm()
	secondDone := hold(second)
	_, err = c.apply(ctx, "resume", "", "", first, "", "")
	require.NoError(t, err)
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("first request remained held")
	}
	select {
	case <-secondDone:
		t.Fatal("second request was released by the first handle")
	default:
	}
	_, err = c.apply(ctx, "resume", "", "", second, "", "")
	require.NoError(t, err)
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("second request remained held")
	}
}
