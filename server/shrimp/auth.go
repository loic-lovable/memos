// Package shrimp contains the explicitly limited HTTPS pilot integration.
package shrimp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pkg/errors"

	"github.com/usememos/memos/store"
)

type accessClaims struct {
	jwt.RegisteredClaims
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
	Confirmation struct {
		Thumbprint string `json:"jkt"`
	} `json:"cnf"`
}

type proofClaims struct {
	jwt.RegisteredClaims
	Method     string `json:"htm"`
	URL        string `json:"htu"`
	AccessHash string `json:"ath"`
}

func oneHeader(r *http.Request, name string) (string, error) {
	values := r.Header.Values(name)
	if len(values) != 1 || len(values[0]) > 16384 {
		return "", errors.New("invalid header")
	}
	return values[0], nil
}

func (h *Handler) authenticate(r *http.Request) (*accessClaims, error) {
	authorization, err := oneHeader(r, "Authorization")
	if err != nil {
		return nil, err
	}
	scheme, access, ok := strings.Cut(authorization, " ")
	if !ok || scheme != "DPoP" {
		return nil, errors.New("DPoP required")
	}
	if err := strictJWT(access); err != nil {
		return nil, err
	}
	claims := &accessClaims{}
	_, err = jwt.ParseWithClaims(access, claims, func(t *jwt.Token) (any, error) {
		if t.Header["typ"] != "at+jwt" || t.Header["kid"] != h.config.IssuerKeyID {
			return nil, errors.New("untrusted key or token type")
		}
		return h.key, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(h.config.Issuer), jwt.WithAudience(h.config.Resource), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		return nil, err
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > 300*time.Second || claims.ID == "" || claims.ClientID != h.config.ClientID || claims.Subject != claims.ClientID || claims.Confirmation.Thumbprint == "" {
		return nil, errors.New("invalid access claims")
	}
	proof, err := oneHeader(r, "DPoP")
	if err != nil {
		return nil, err
	}
	if err := strictJWT(proof); err != nil {
		return nil, err
	}
	pc := &proofClaims{}
	var thumbprint string
	_, err = jwt.ParseWithClaims(proof, pc, func(t *jwt.Token) (any, error) {
		if t.Header["typ"] != "dpop+jwt" {
			return nil, errors.New("invalid proof type")
		}
		encoded, err := json.Marshal(t.Header["jwk"])
		if err != nil {
			return nil, err
		}
		var jwk struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		}
		if err := strictJSON(encoded, &jwk); err != nil {
			return nil, err
		}
		if jwk.Kty != "EC" || jwk.Crv != "P-256" {
			return nil, errors.New("unsupported proof key")
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(x) != 32 {
			return nil, errors.New("invalid x")
		}
		y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
		if err != nil || len(y) != 32 {
			return nil, errors.New("invalid y")
		}
		key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !key.Curve.IsOnCurve(key.X, key.Y) {
			return nil, errors.New("invalid curve point")
		}
		canonical, _ := json.Marshal(map[string]string{"crv": "P-256", "kty": "EC", "x": jwk.X, "y": jwk.Y})
		hash := sha256.Sum256(canonical)
		thumbprint = base64.RawURLEncoding.EncodeToString(hash[:])
		return key, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuedAt())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	hash := sha256.Sum256([]byte(access))
	if pc.IssuedAt == nil || now.Sub(pc.IssuedAt.Time) > 60*time.Second || pc.IssuedAt.After(now.Add(5*time.Second)) || pc.ID == "" || len(pc.ID) > 128 || pc.Method != r.Method || pc.URL != h.origin+r.URL.EscapedPath() || r.URL.RawQuery != "" || pc.AccessHash != base64.RawURLEncoding.EncodeToString(hash[:]) || thumbprint != claims.Confirmation.Thumbprint {
		return nil, errors.New("invalid proof binding")
	}
	replay := sha256.Sum256([]byte(thumbprint + "\x00" + pc.ID))
	if err := h.driver.ConsumeShrimpProof(r.Context(), hex.EncodeToString(replay[:]), pc.IssuedAt.Unix()+65); err != nil {
		if errors.Is(err, store.ErrShrimpProofReplay) {
			return nil, err
		}
		return nil, errors.Wrap(store.ErrShrimpProofStorage, err.Error())
	}
	return claims, nil
}

func hasScope(claims *accessClaims, scope string) bool {
	for _, value := range strings.Fields(claims.Scope) {
		if value == scope {
			return true
		}
	}
	return false
}

func strictJWT(value string) error {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return errors.New("invalid JWT")
	}
	for _, part := range parts[:2] {
		data, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return err
		}
		var object map[string]any
		if err := strictJSON(data, &object); err != nil {
			return err
		}
	}
	return nil
}
