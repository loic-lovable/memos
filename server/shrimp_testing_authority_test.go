//go:build shrimptest

package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOperationalAuditCannotReuseFaultAuthority(t *testing.T) {
	control := strings.Repeat("c", 32)
	audit := strings.Repeat("a", 32)
	raw, err := json.Marshal(map[string]any{"test": map[string]string{"control_token": control, "audit_token": audit}})
	require.NoError(t, err)
	var settings shrimpTestSettings
	require.NoError(t, json.Unmarshal(raw, &settings))
	require.Error(t, settings.validateAuditAuthority(control))
	require.NoError(t, settings.validateAuditAuthority(audit))
	require.NoError(t, settings.validateAuditAuthority(""))
}
