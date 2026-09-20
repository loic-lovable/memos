package shrimp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

func TestSSOEnrollmentRequiresFixedProviderAndClosedRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, identifier                string
		registration, password, allowed bool
	}{
		{"fixed subject", "sub", true, true, true},
		{"email mapping", "email", true, true, false},
		{"open registration", "sub", false, true, false},
		{"password allowed", "sub", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &profile.Profile{Data: t.TempDir(), Driver: "sqlite", Version: "0.31.0"}
			p.DSN = filepath.Join(p.Data, "memos.db")
			driver, err := sqlite.NewDB(p)
			require.NoError(t, err)
			s := store.New(driver, p)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.NoError(t, s.Migrate(t.Context()))
			_, err = ssoEnrollment(t.Context(), s, "local-sso")
			require.Error(t, err, "database-only or missing configuration must be refused")
			directory := t.TempDir()
			provider := map[string]any{"uid": "local-sso", "name": "Local SSO", "type": "OAUTH2", "config": map[string]any{"oauth2Config": map[string]any{
				"clientId": "memos", "clientSecret": "synthetic-secret", "authUrl": "https://sso.example/authorize", "tokenUrl": "https://sso.example/token", "userInfoUrl": "https://sso.example/userinfo", "scopes": []string{"profile"}, "fieldMapping": map[string]string{"identifier": tc.identifier}}}}
			write := func(name string, value any) {
				data, err := json.Marshal(value)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0600))
			}
			write("memos-idp-local-sso.json", provider)
			write("memos-instance-setting-general.json", map[string]any{"key": "GENERAL", "generalSetting": map[string]bool{"disallowUserRegistration": tc.registration, "disallowPasswordAuth": tc.password}})
			require.NoError(t, s.LoadDeploymentConfigurationDir(t.Context(), directory))
			binding, err := ssoEnrollment(t.Context(), s, "local-sso")
			if !tc.allowed {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotContains(t, binding, "synthetic-secret")
			require.NoError(t, s.EnableShrimpPilot(t.Context(), binding))
			require.NoError(t, s.EnableShrimpPilot(t.Context(), binding))
			provider["config"].(map[string]any)["oauth2Config"].(map[string]any)["userInfoUrl"] = "https://other.example/userinfo"
			write("memos-idp-local-sso.json", provider)
			require.NoError(t, s.LoadDeploymentConfigurationDir(t.Context(), directory))
			changed, err := ssoEnrollment(t.Context(), s, "local-sso")
			require.NoError(t, err)
			require.NotEqual(t, binding, changed)
			require.Error(t, s.EnableShrimpPilot(t.Context(), changed), "restart cannot redirect existing identities to a different provider")
		})
	}
}
