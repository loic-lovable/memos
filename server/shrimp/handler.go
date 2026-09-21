// Package shrimp adapts the experimental SHRIMP SDK to Memos storage and enrollment.
package shrimp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/lovablelabs/shrimp-protocol/sdk/go/server"
	"github.com/pkg/errors"

	"github.com/usememos/memos/store"
)

// Config is trusted local enrollment; no request can choose its issuer or authority.
type Config struct {
	Resource        string `json:"resource"`
	Issuer          string `json:"issuer"`
	IssuerKeyID     string `json:"issuer_key_id"`
	IssuerKeyFile   string `json:"issuer_key_file"`
	ClientID        string `json:"client_id"`
	Authority       string `json:"authority"`
	SchemaDirectory string `json:"schema_directory"`
	AllowWrite      bool   `json:"allow_write"`
	SSOProvider     string `json:"sso_provider,omitempty"`
}

// Handler combines the protocol SDK with Memos-owned enrollment and persistence.
type Handler struct {
	*sdk.Handler
	config Config
	store  *store.Store
	driver store.ShrimpDriver
	// Fault is set only by the explicitly tagged disposable-test build.
	Fault func(string, string)
}

// New validates the local pilot deployment before enabling its application adapter.
func New(ctx context.Context, s *store.Store, config Config) (*Handler, error) {
	u, err := url.Parse(config.Resource)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || !strings.HasSuffix(u.Path, "/tenants/acme/domains/A") {
		return nil, errors.New("pilot requires an exact HTTPS acme/A resource")
	}
	driver, ok := s.GetDriver().(store.ShrimpDriver)
	if !ok {
		return nil, errors.New("SHRIMP pilot requires SQLite")
	}
	if _, ok := driver.(store.ShrimpAuditDriver); !ok {
		return nil, errors.New("SHRIMP pilot requires durable auditing")
	}
	h := &Handler{config: config, store: s, driver: driver}
	protocol, err := sdk.New(ctx, &application{handler: h}, sdk.Config{
		Resource: config.Resource, Issuer: config.Issuer, IssuerKeyID: config.IssuerKeyID,
		IssuerKeyFile: config.IssuerKeyFile, ClientID: config.ClientID, Authority: config.Authority,
		SchemaDirectory: config.SchemaDirectory, AllowWrite: config.AllowWrite,
		Tenant: "acme", Domain: "A", HistoryEpoch: "memos-pilot-1", DiscoveryRevision: "memos-pilot-3",
		AdmissionConsumer: "memos-session-refresh-pat",
		HealthyConditions: "Disposable single-process SQLite pilot with at most 10000 unexpired retained operation results. Only listed operations, one human command per mutation, source reference plus displayName required on creation; displayName-only updates. No complete profile, public audit, sync, existing-session revocation, backup restore, or multi-process admission guarantee.",
	})
	if err != nil {
		return nil, err
	}
	identity := []string{config.Resource, config.Issuer, config.IssuerKeyID, config.ClientID, config.Authority}
	if config.SSOProvider != "" {
		binding, err := ssoEnrollment(ctx, s, config.SSOProvider)
		if err != nil {
			return nil, err
		}
		identity = append(identity, binding)
	}
	enrollment, _ := json.Marshal(identity)
	if err := s.EnableShrimpPilot(ctx, string(enrollment)); err != nil {
		return nil, err
	}
	h.Handler = protocol
	return h, nil
}

// The operational audit view is application-specific and has separate read-only
// authority; these helpers do not implement provisioning routes.
func (h *Handler) scope() map[string]any {
	return map[string]any{"resource": h.config.Resource, "tenant": "acme", "domain": "A", "schema_version": "0.2", "history_epoch": "memos-pilot-1", "authorization_context": h.config.Authority}
}
func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (h *Handler) problem(w http.ResponseWriter, status int, code, stage string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": code, "status": status, "code": code, "stage": stage, "operation": nil, "command_id": nil, "commit": "unknown", "recovery": map[string]any{"action": "inspect_or_repair", "retry_after_seconds": nil}})
}
