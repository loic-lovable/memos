package server

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
)

// IssuerKeyFingerprint identifies the exact public key loaded by New, independent
// of PEM formatting. Applications can pin it in their durable enrollment so a
// changed file under the same key ID cannot silently replace the trust anchor.
func (h *Handler) IssuerKeyFingerprint() string {
	der, err := x509.MarshalPKIXPublicKey(h.key)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(der)
	return hex.EncodeToString(digest[:])
}
