package server

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

// authenticationFailure preserves the OAuth failure class without exposing JWT
// parser or storage details in responses (or in this error's public text).
type authenticationFailure struct {
	code  string
	cause error
}

func (e *authenticationFailure) Error() string { return e.code }
func (e *authenticationFailure) Unwrap() error { return e.cause }

func (h *Handler) authenticate(r *http.Request) (_ *accessClaims, err error) {
	failureCode := "invalid_token"
	defer func() {
		if err != nil && !errors.Is(err, ErrProofStorage) {
			err = &authenticationFailure{code: failureCode, cause: err}
		}
	}()
	if len(r.Header.Values("Authorization")) == 0 {
		failureCode = "unauthenticated"
		return nil, errors.New("resource credential required")
	}
	authorization, err := oneHeader(r, "Authorization")
	if err != nil {
		return nil, err
	}
	scheme, access, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "DPoP") {
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
	if len(claims.Audience) != 1 || claims.Audience[0] != h.config.Resource || claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > 300*time.Second || claims.ID == "" || claims.ClientID != h.config.ClientID || claims.Subject != claims.ClientID || claims.Confirmation.Thumbprint == "" {
		return nil, errors.New("invalid access claims")
	}
	failureCode = "invalid_dpop_proof"
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
	if pc.IssuedAt == nil || now.Sub(pc.IssuedAt.Time) > 60*time.Second || pc.IssuedAt.After(now.Add(5*time.Second)) || pc.ID == "" || len(pc.ID) > 128 || pc.Method != r.Method || pc.URL != h.origin+r.URL.EscapedPath() || r.URL.RawQuery != "" || pc.AccessHash != base64.RawURLEncoding.EncodeToString(hash[:]) {
		return nil, errors.New("invalid proof binding")
	}
	// A valid proof made with a different key fails token confirmation, not
	// proof validation (RFC 9449 section 7.1).
	if thumbprint != claims.Confirmation.Thumbprint {
		failureCode = "invalid_token"
		return nil, errors.New("token confirmation failed")
	}
	replay := sha256.Sum256([]byte(thumbprint + "\x00" + pc.ID))
	if err := h.driver.ConsumeProof(r.Context(), hex.EncodeToString(replay[:]), pc.IssuedAt.Unix()+65); err != nil {
		if errors.Is(err, ErrProofReplay) {
			return nil, err
		}
		return nil, errors.Wrap(ErrProofStorage, err.Error())
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

// authenticationProblem never correlates an unauthenticated request with stored
// operations; a refusal cannot establish the outcome of an earlier attempt.
func (h *Handler) authenticationProblem(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrProofStorage) {
		h.problemRecovery(w, http.StatusServiceUnavailable, "storage_unavailable", "authentication", "retry_with_fresh_proof", 1)
		return
	}
	var failure *authenticationFailure
	if !errors.As(err, &failure) {
		h.problemRecovery(w, http.StatusServiceUnavailable, "storage_unavailable", "authentication", "retry_with_fresh_proof", 1)
		return
	}
	challenge, recovery := `DPoP algs="ES256"`, "obtain_token"
	switch failure.code {
	case "invalid_token":
		challenge, recovery = `DPoP error="invalid_token", algs="ES256"`, "renew_authentication"
	case "invalid_dpop_proof":
		challenge, recovery = `DPoP error="invalid_dpop_proof", algs="ES256"`, "correct_proof"
	}
	w.Header().Set("WWW-Authenticate", challenge)
	h.problemRecovery(w, http.StatusUnauthorized, failure.code, "authentication", recovery, 0)
}

func (h *Handler) insufficientScope(w http.ResponseWriter, scope string) {
	// Callers supply only fixed protocol scope names, never request values.
	w.Header().Set("WWW-Authenticate", `DPoP error="insufficient_scope", scope="`+scope+`", algs="ES256"`)
	h.problemRecovery(w, http.StatusForbidden, "insufficient_scope", "authorization", "request_authorization", 0)
}
