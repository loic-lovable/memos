package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewUsesExplicitApplicationDeclarationsWithoutCallingStorage(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "issuer.pem")
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}), 0600))
	// Small constructor fixtures keep the independently versioned module testable
	// without a surrounding repository. Wire schema validation remains in app tests.
	for _, name := range []string{"mutation", "read-sync", "receipt", "discovery"} {
		schema := `{"$id":"https://shrimp.example/schemas/0.2/` + name + `-v0.2.schema.json","type":"object","$defs":{"request":{"type":"object"},"read_request":{"type":"object"},"read_response":{"type":"object"},"enumeration_request":{"type":"object"},"enumeration_page":{"type":"object"}}}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+"-v0.2.schema.json"), []byte(schema), 0600))
	}
	for _, name := range []string{"human-attributes-v1.schema.json", "enterprise-attributes-v1.schema.json"} {
		schema := `{"$id":"https://shrimp.example/schemas/0.2/` + name + `","type":"object"}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(schema), 0600))
	}
	config := Config{Resource: "https://app.example/provisioning", Issuer: "https://issuer.example", IssuerKeyID: "key", IssuerKeyFile: keyFile, ClientID: "client", Authority: "hr", SchemaDirectory: dir, Tenant: "other-tenant", Domain: "other-domain", HistoryEpoch: "history-9", DiscoveryRevision: "discovery-7", AdmissionConsumer: "native-login", HealthyConditions: "Single application process under its own admission lock."}
	// Embedding a nil Application makes every unexpected storage call panic.
	h, err := New(context.Background(), &lookupApp{}, config)
	require.NoError(t, err)
	require.Equal(t, "other-tenant", h.scope()["tenant"])
	require.Equal(t, "other-domain", h.scope()["domain"])
	require.Equal(t, "history-9", h.scope()["history_epoch"])
	require.Equal(t, "discovery-7", h.discovery["discovery_revision"])
	contract := h.discovery["versions"].([]any)[0].(map[string]any)
	require.Equal(t, config.HealthyConditions, contract["healthy_conditions"])
	require.Empty(t, contract["profiles"])
	require.Contains(t, h.schemas, "human-attributes-v1.schema.json")
	require.Contains(t, h.schemas, "enterprise-attributes-v1.schema.json")
	require.Equal(t, []string{"other-domain"}, contract["dependency_issuers"].([]any)[0].(map[string]any)["domains"])
	for _, field := range []string{"tenant", "domain", "history", "revision", "consumer", "conditions"} {
		t.Run(field, func(t *testing.T) {
			missing := config
			switch field {
			case "tenant":
				missing.Tenant = ""
			case "domain":
				missing.Domain = ""
			case "history":
				missing.HistoryEpoch = ""
			case "revision":
				missing.DiscoveryRevision = ""
			case "consumer":
				missing.AdmissionConsumer = ""
			case "conditions":
				missing.HealthyConditions = ""
			}
			_, err := New(context.Background(), &lookupApp{}, missing)
			require.Error(t, err)
		})
	}
	_, err = New(context.Background(), nil, config)
	require.Error(t, err)
}
