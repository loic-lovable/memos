package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/store"
)

type observationDriver struct {
	store.Driver
	user *store.User
	err  error
}

func (d *observationDriver) ListUsers(context.Context, *store.FindUser) ([]*store.User, error) {
	if d.user == nil {
		return nil, d.err
	}
	return []*store.User{d.user}, d.err
}

func TestAccessTokenObservationDoesNotInventArchivedUser(t *testing.T) {
	secret := "test-only-key"
	token, _, err := GenerateAccessTokenV2(1, "test", "USER", "NORMAL", []byte(secret))
	require.NoError(t, err)
	for _, test := range []struct {
		name     string
		user     *store.User
		err      error
		wantUser bool
	}{
		{name: "missing user"},
		{name: "store unavailable", err: errors.New("storage unavailable")},
		{name: "active user", user: &store.User{ID: 1, RowStatus: store.Normal}, wantUser: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := store.New(&observationDriver{user: test.user, err: test.err}, &profile.Profile{})
			calls := 0
			ctx := WithArchivedAccessTokenObserver(t.Context(), func(int32) { calls++ })
			result := NewAuthenticator(s, secret).Authenticate(ctx, "Bearer "+token)
			require.Equal(t, test.wantUser, result != nil)
			require.Zero(t, calls)
		})
	}
}
