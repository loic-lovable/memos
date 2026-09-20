package shrimp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/pkg/errors"

	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
)

// ssoEnrollment pins a deployment-owned provider and a provisioned-users-only
// policy. This is a local adapter mapping, not a portable SHRIMP binding command.
func ssoEnrollment(ctx context.Context, s *store.Store, uid string) (string, error) {
	if !s.IsIdentityProviderDeploymentConfigured(uid) || !s.IsInstanceSettingDeploymentConfigured(storepb.InstanceSettingKey_GENERAL) {
		return "", errors.New("pilot SSO requires a deployment-configured provider and general settings")
	}
	provider, err := s.GetIdentityProvider(ctx, &store.FindIdentityProvider{UID: &uid})
	if err != nil {
		return "", err
	}
	if provider == nil || provider.Type != storepb.IdentityProvider_OAUTH2 || provider.Config.GetOauth2Config().GetFieldMapping().GetIdentifier() != "sub" {
		return "", errors.New("pilot SSO requires an OAuth2 provider mapped to sub")
	}
	general, err := s.GetInstanceGeneralSetting(ctx)
	if err != nil {
		return "", err
	}
	if !general.DisallowUserRegistration || !general.DisallowPasswordAuth {
		return "", errors.New("pilot SSO requires registration and regular-user password login disabled")
	}
	encoded, err := json.Marshal(provider)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sso-sub-v1:" + hex.EncodeToString(digest[:]), nil
}
