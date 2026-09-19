package shrimp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"github.com/usememos/memos/store"
)

type proofStore struct {
	store.ShrimpDriver
	used map[string]bool
}

func (d *proofStore) ConsumeShrimpProof(_ context.Context, id string, _ int64) error {
	if d.used[id] {
		return store.ErrShrimpProofReplay
	}
	d.used[id] = true
	return nil
}

func TestAuthenticationBindsIssuerAudienceMethodURLTokenAndKey(t *testing.T) {
	issuer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	proof, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	jwk := map[string]string{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(proof.X.FillBytes(make([]byte, 32))), "y": base64.RawURLEncoding.EncodeToString(proof.Y.FillBytes(make([]byte, 32)))}
	canonical, err := json.Marshal(jwk)
	require.NoError(t, err)
	thumb := sha256.Sum256(canonical)
	resource := "https://pilot.example/shrimp/v1/tenants/acme/domains/A"
	cases := []string{"valid", "issuer", "audience", "method", "url", "access_hash", "proof_key", "expired", "private_jwk", "replay"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			h := &Handler{config: Config{Issuer: "https://issuer.example", IssuerKeyID: "issuer-1", Resource: resource, ClientID: "hr"}, origin: "https://pilot.example", key: &issuer.PublicKey, driver: &proofStore{used: map[string]bool{}}}
			now := time.Now().Unix()
			claims := jwt.MapClaims{"iss": h.config.Issuer, "aud": resource, "sub": "hr", "client_id": "hr", "iat": now, "exp": now + 60, "jti": "access-1", "scope": "shrimp.read shrimp.write", "cnf": map[string]string{"jkt": base64.RawURLEncoding.EncodeToString(thumb[:])}}
			switch name {
			case "issuer":
				claims["iss"] = "https://attacker.example"
			case "audience":
				claims["aud"] = resource + "other"
			case "expired":
				claims["exp"] = now - 1
			case "proof_key":
				claims["cnf"] = map[string]string{"jkt": "different"}
			}
			at := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
			at.Header["typ"] = "at+jwt"
			at.Header["kid"] = "issuer-1"
			access, err := at.SignedString(issuer)
			require.NoError(t, err)
			hash := sha256.Sum256([]byte(access))
			pc := jwt.MapClaims{"jti": "proof-1", "iat": now, "htm": "GET", "htu": resource + "/capabilities", "ath": base64.RawURLEncoding.EncodeToString(hash[:])}
			switch name {
			case "method":
				pc["htm"] = "POST"
			case "url":
				pc["htu"] = resource + "/mutations"
			case "access_hash":
				pc["ath"] = "other"
			}
			pt := jwt.NewWithClaims(jwt.SigningMethodES256, pc)
			pt.Header["typ"] = "dpop+jwt"
			public := map[string]string{}
			for k, v := range jwk {
				public[k] = v
			}
			if name == "private_jwk" {
				public["d"] = "private-material"
			}
			pt.Header["jwk"] = public
			signed, err := pt.SignedString(proof)
			require.NoError(t, err)
			r := httptest.NewRequest("GET", resource+"/capabilities", nil)
			r.Header.Set("Authorization", "DPoP "+access)
			r.Header.Set("DPoP", signed)
			_, err = h.authenticate(r)
			if name == "valid" || name == "replay" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if name == "replay" {
				_, err = h.authenticate(r)
				require.Error(t, err)
			}
		})
	}
}
