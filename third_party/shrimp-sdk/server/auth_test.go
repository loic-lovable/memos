package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

type proofStore struct {
	Application
	used    map[string]bool
	failure error
	calls   int
}

func (d *proofStore) ConsumeProof(_ context.Context, id string, _ int64) error {
	d.calls++
	if d.failure != nil {
		return d.failure
	}
	if d.used[id] {
		return ErrProofReplay
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
	cases := []string{"valid", "singleton_audience", "issuer", "audience", "multiple_audiences", "duplicate_audience", "method", "url", "access_hash", "proof_key", "expired", "private_jwk", "replay"}
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
			case "singleton_audience":
				claims["aud"] = []string{resource}
			case "multiple_audiences":
				claims["aud"] = []string{resource, resource + "other"}
			case "duplicate_audience":
				claims["aud"] = []string{resource, resource}
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
			if name == "valid" || name == "singleton_audience" || name == "replay" {
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

func TestAuthenticationChallengesPreserveFailureClassAndPrivacy(t *testing.T) {
	issuer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	proofKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	jwk := map[string]string{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(proofKey.X.FillBytes(make([]byte, 32))), "y": base64.RawURLEncoding.EncodeToString(proofKey.Y.FillBytes(make([]byte, 32)))}
	canonical, err := json.Marshal(jwk)
	require.NoError(t, err)
	thumb := sha256.Sum256(canonical)
	resource := "https://pilot.example/shrimp/v1/tenants/acme/domains/A"
	for _, tc := range []struct {
		name                   string
		status                 int
		code, challenge, scope string
	}{
		{"valid", 200, "", "", ""},
		{"scheme_case", 200, "", "", ""},
		{"missing", 401, "unauthenticated", `DPoP algs="ES256"`, ""},
		{"missing_unknown_route", 401, "unauthenticated", `DPoP algs="ES256"`, ""},
		{"empty", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"duplicate_authorization", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"bearer", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"malformed_token", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"expired_token", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"wrong_issuer", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"wrong_audience", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"wrong_token_type", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"unknown_token_key", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"token_key_binding", 401, "invalid_token", `DPoP error="invalid_token", algs="ES256"`, ""},
		{"missing_proof", 401, "invalid_dpop_proof", `DPoP error="invalid_dpop_proof", algs="ES256"`, ""},
		{"duplicate_proof", 401, "invalid_dpop_proof", `DPoP error="invalid_dpop_proof", algs="ES256"`, ""},
		{"malformed_proof", 401, "invalid_dpop_proof", `DPoP error="invalid_dpop_proof", algs="ES256"`, ""},
		{"wrong_method", 401, "invalid_dpop_proof", `DPoP error="invalid_dpop_proof", algs="ES256"`, ""},
		{"wrong_access_hash", 401, "invalid_dpop_proof", `DPoP error="invalid_dpop_proof", algs="ES256"`, ""},
		{"replay", 401, "invalid_dpop_proof", `DPoP error="invalid_dpop_proof", algs="ES256"`, ""},
		{"missing_read_scope", 403, "insufficient_scope", `DPoP error="insufficient_scope", scope="shrimp.read", algs="ES256"`, "shrimp.write"},
		{"missing_write_scope", 403, "insufficient_scope", `DPoP error="insufficient_scope", scope="shrimp.write", algs="ES256"`, "shrimp.read"},
		{"withdrawn_delegation", 403, "forbidden", "", ""},
		{"storage_unavailable", 503, "storage_unavailable", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &proofStore{used: map[string]bool{}}
			h := &Handler{config: Config{Issuer: "https://issuer.example", IssuerKeyID: "issuer-1", Resource: resource, ClientID: "hr", AllowWrite: true}, path: "/shrimp/v1/tenants/acme/domains/A", origin: "https://pilot.example", key: &issuer.PublicKey, driver: driver, discovery: map[string]any{"discovery_version": "0.2"}}
			now := time.Now().Unix()
			claims := jwt.MapClaims{"iss": h.config.Issuer, "aud": resource, "sub": "hr", "client_id": "hr", "iat": now, "exp": now + 60, "jti": "private-access-id", "scope": "shrimp.read shrimp.write", "cnf": map[string]string{"jkt": base64.RawURLEncoding.EncodeToString(thumb[:])}}
			if tc.scope != "" {
				claims["scope"] = tc.scope
			}
			switch tc.name {
			case "expired_token":
				claims["exp"] = now - 1
			case "wrong_issuer":
				claims["iss"] = "https://untrusted.example"
			case "wrong_audience":
				claims["aud"] = resource + "other"
			case "token_key_binding":
				claims["cnf"] = map[string]string{"jkt": "different-key"}
			}
			token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
			token.Header["typ"], token.Header["kid"] = "at+jwt", "issuer-1"
			if tc.name == "wrong_token_type" {
				token.Header["typ"] = "JWT"
			}
			if tc.name == "unknown_token_key" {
				token.Header["kid"] = "untrusted"
			}
			access, err := token.SignedString(issuer)
			require.NoError(t, err)
			url := resource + "/capabilities"
			if tc.name == "missing_unknown_route" {
				url = resource + "/operations/private-window/private-operation"
			}
			if tc.name == "missing_write_scope" || tc.name == "withdrawn_delegation" {
				url = resource + "/replay-window"
			}
			if tc.name == "malformed_token" {
				access = "private-invalid-token"
			}
			hash := sha256.Sum256([]byte(access))
			proof := jwt.MapClaims{"jti": "private-proof-id", "iat": now, "htm": "GET", "htu": url, "ath": base64.RawURLEncoding.EncodeToString(hash[:])}
			if tc.name == "wrong_method" {
				proof["htm"] = "POST"
			}
			if tc.name == "wrong_access_hash" {
				proof["ath"] = "different-access-token"
			}
			pt := jwt.NewWithClaims(jwt.SigningMethodES256, proof)
			pt.Header["typ"], pt.Header["jwk"] = "dpop+jwt", jwk
			signed, err := pt.SignedString(proofKey)
			require.NoError(t, err)
			body := strings.NewReader(`{"private":"untrusted-body"}`)
			r := httptest.NewRequest(http.MethodGet, url, body)
			r.Header.Set("Authorization", "DPoP "+access)
			r.Header.Set("DPoP", signed)
			r.Header.Set("SHRIMP-Version", "0.2")
			r.Header.Set("SHRIMP-Discovery-Version", "0.2")
			switch tc.name {
			case "scheme_case":
				r.Header.Set("Authorization", "dpop "+access)
			case "missing", "missing_unknown_route":
				r.Header.Del("Authorization")
			case "empty":
				r.Header.Set("Authorization", "")
			case "duplicate_authorization":
				r.Header.Add("Authorization", "DPoP private-other-token")
			case "bearer":
				r.Header.Set("Authorization", "Bearer "+access)
			case "missing_proof":
				r.Header.Del("DPoP")
			case "duplicate_proof":
				r.Header.Add("DPoP", signed)
			case "malformed_proof":
				r.Header.Set("DPoP", "private-invalid-proof")
			case "withdrawn_delegation":
				h.config.AllowWrite = false
			case "storage_unavailable":
				driver.failure = errors.New("private-storage-failure")
			case "replay":
				_, err := h.authenticate(r)
				require.NoError(t, err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code)
			require.Equal(t, tc.challenge, w.Header().Get("WWW-Authenticate"))
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			require.Empty(t, w.Header().Get("DPoP-Nonce"), "pilot does not require a resource nonce")
			require.Equal(t, len(`{"private":"untrusted-body"}`), body.Len(), "authentication must precede reading the body")
			if tc.status == 200 {
				return
			}
			var problem map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
			require.Equal(t, tc.code, problem["code"])
			require.Equal(t, float64(tc.status), problem["status"])
			require.Equal(t, "unknown", problem["commit"])
			require.Nil(t, problem["operation"])
			require.Nil(t, problem["command_id"])
			for _, secret := range []string{access, signed, "private-access-id", "private-proof-id", "private-storage-failure", "private-operation", "untrusted-body"} {
				require.NotContains(t, w.Body.String(), secret)
			}
			if tc.status == 401 && tc.name != "replay" {
				require.Zero(t, driver.calls, "invalid credentials/proofs must not reach replay storage")
			}
			if tc.status == 503 {
				require.Equal(t, "1", w.Header().Get("Retry-After"))
				recovery := problem["recovery"].(map[string]any)
				require.Equal(t, float64(1), recovery["retry_after_seconds"])
				require.Equal(t, "retry_with_fresh_proof", recovery["action"])
			} else {
				require.Empty(t, w.Header().Get("Retry-After"))
			}
		})
	}
}
