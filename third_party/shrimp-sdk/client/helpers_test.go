package client

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
)

func testKey(t *testing.T, dir, name string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testProfile(t *testing.T) Profile {
	t.Helper()
	dir := t.TempDir()
	return Profile{Resource: "https://app.example/shrimp/v1/tenants/acme/domains/A", Issuer: "https://idp.example",
		Tenant: "acme", Domain: "A", ClientID: "reader", ClientKeyID: "client-1", Version: "0.1",
		ClientKeyFile: testKey(t, dir, "client.pem"), DPoPKeyFile: testKey(t, dir, "proof.pem")}
}

func testSaver(dir string) SaveIntent {
	return func(_ context.Context, id string, raw []byte) error {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, id+".json"), raw, 0600)
	}
}
