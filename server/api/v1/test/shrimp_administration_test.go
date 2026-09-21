package test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	apiv1 "github.com/usememos/memos/server/api/v1"
	"github.com/usememos/memos/store"
)

func TestShrimpNativeAdministrationAPIRefusesTopologyChanges(t *testing.T) {
	ts := NewTestService(t)
	defer ts.Cleanup()
	ctx := t.Context()
	admin, err := ts.CreateHostUser(ctx, "admin")
	require.NoError(t, err)
	oauth := newMockOAuthServer(t, "code", "token", map[string]any{"sub": "new-external-subject"})
	defer oauth.Close()
	idpName := createTestingOAuthIdentityProvider(ctx, t, ts, oauth.URL, "existing")
	require.NoError(t, ts.Store.EnableShrimpPilot(ctx, "test-enrollment"))
	adminCtx := ts.CreateUserContext(ctx, admin.ID)
	cases := map[string]func() error{
		"create native user": func() error {
			_, err := ts.Service.CreateUser(adminCtx, &v1pb.CreateUserRequest{User: &v1pb.User{Username: "new-user", Password: "secure-password-123"}})
			return err
		},
		"create provider": func() error {
			_, err := ts.Service.CreateIdentityProvider(adminCtx, &v1pb.CreateIdentityProviderRequest{IdentityProviderId: "new-provider", IdentityProvider: &v1pb.IdentityProvider{Title: "Other"}})
			return err
		},
		"update provider": func() error {
			_, err := ts.Service.UpdateIdentityProvider(adminCtx, &v1pb.UpdateIdentityProviderRequest{IdentityProvider: &v1pb.IdentityProvider{Name: idpName, Title: "Changed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}})
			return err
		},
		"delete provider": func() error {
			_, err := ts.Service.DeleteIdentityProvider(adminCtx, &v1pb.DeleteIdentityProviderRequest{Name: idpName})
			return err
		},
		"link identity": func() error {
			_, err := ts.Service.CreateLinkedIdentity(adminCtx, &v1pb.CreateLinkedIdentityRequest{Parent: "users/admin", IdpName: idpName, Code: "code"})
			return err
		},
		"unlink identity": func() error {
			_, err := ts.Service.DeleteLinkedIdentity(adminCtx, &v1pb.DeleteLinkedIdentityRequest{Name: "users/admin/identities/existing"})
			return err
		},
		"SSO first login": func() error {
			requestCtx, release := store.WithAdmissionScope(apiv1.WithHeaderCarrier(context.WithoutCancel(ctx)))
			defer release()
			response, err := ts.Service.SignIn(requestCtx, &v1pb.SignInRequest{Credentials: &v1pb.SignInRequest_SsoCredentials{SsoCredentials: &v1pb.SignInRequest_SSOCredentials{IdpName: idpName, Code: "code", RedirectUri: "http://localhost:8080/auth/callback"}}})
			require.Nil(t, response)
			require.Empty(t, apiv1.GetHeaderCarrier(requestCtx).Get("Set-Cookie"))
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) { require.Equal(t, codes.PermissionDenied, status.Code(call())) })
	}
	users, err := ts.Store.ListUsers(ctx, &store.FindUser{})
	require.NoError(t, err)
	require.Len(t, users, 1)
	identities, err := ts.Store.ListUserIdentities(ctx, &store.FindUserIdentity{})
	require.NoError(t, err)
	require.Empty(t, identities)
	providers, err := ts.Store.ListIdentityProviders(ctx, &store.FindIdentityProvider{})
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, "existing", providers[0].Uid)
}
