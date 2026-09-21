package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func TestShrimpEnrollmentFreezesNativeIdentityAdministration(t *testing.T) {
	ctx := t.Context()
	s := newDeploymentConfigurationTestStore(t)
	admin, err := s.CreateUser(ctx, &store.User{Username: "admin", Role: store.RoleAdmin})
	require.NoError(t, err)
	provider, err := s.CreateIdentityProvider(ctx, deploymentIdentityProvider("existing", "Existing", "secret"))
	require.NoError(t, err)
	identity, err := s.CreateUserIdentity(ctx, &store.UserIdentity{UserID: admin.ID, Provider: "existing", ExternUID: "admin-sub"})
	require.NoError(t, err)
	require.NoError(t, s.EnableShrimpPilot(ctx, "test-enrollment"))

	_, err = s.CreateUser(ctx, &store.User{Username: "unmanaged", Role: store.RoleUser})
	require.ErrorIs(t, err, store.ErrShrimpNativeAdministration)
	_, err = s.CreateUserWithIdentity(ctx, &store.User{Username: "jit", Role: store.RoleUser}, &store.UserIdentity{Provider: "existing", ExternUID: "jit-sub"})
	require.ErrorIs(t, err, store.ErrShrimpNativeAdministration)
	_, err = s.CreateUserIdentity(ctx, &store.UserIdentity{UserID: admin.ID, Provider: "other", ExternUID: "retired-sub"})
	require.ErrorIs(t, err, store.ErrShrimpNativeAdministration)
	require.ErrorIs(t, s.DeleteUserIdentities(ctx, &store.DeleteUserIdentity{ID: &identity.ID}), store.ErrShrimpNativeAdministration)
	_, err = s.CreateIdentityProvider(ctx, deploymentIdentityProvider("other", "Other", "secret"))
	require.ErrorIs(t, err, store.ErrShrimpNativeAdministration)
	name := "Changed"
	_, err = s.UpdateIdentityProvider(ctx, &store.UpdateIdentityProviderV1{ID: provider.Id, Name: &name})
	require.ErrorIs(t, err, store.ErrShrimpNativeAdministration)
	require.ErrorIs(t, s.DeleteIdentityProvider(ctx, &store.DeleteIdentityProvider{ID: provider.Id}), store.ErrShrimpNativeAdministration)
	require.ErrorIs(t, s.DeleteIdentityProviderSafely(ctx, &store.DeleteIdentityProvider{ID: provider.Id}), store.ErrShrimpNativeAdministration)
	users, err := s.ListUsers(ctx, &store.FindUser{})
	require.NoError(t, err)
	require.Len(t, users, 1)
	kept, err := s.GetUserIdentity(ctx, &store.FindUserIdentity{ID: &identity.ID})
	require.NoError(t, err)
	require.Equal(t, identity, kept)
	keptProvider, err := s.GetIdentityProvider(ctx, &store.FindIdentityProvider{ID: &provider.Id})
	require.NoError(t, err)
	require.Equal(t, provider, keptProvider)
}

func TestShrimpEnrollmentAllowsOnlyFirstAdminBootstrap(t *testing.T) {
	ctx := t.Context()
	s := newDeploymentConfigurationTestStore(t)
	require.NoError(t, s.EnableShrimpPilot(ctx, "test-enrollment"))
	admin, created, err := s.CreateUserIfNoUsers(ctx, &store.User{Username: "admin", Role: store.RoleAdmin})
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, admin)
	_, created, err = s.CreateUserIfNoUsers(ctx, &store.User{Username: "second", Role: store.RoleAdmin})
	require.NoError(t, err)
	require.False(t, created)
}
