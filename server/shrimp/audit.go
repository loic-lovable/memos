package shrimp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"time"

	"github.com/usememos/memos/store"
)

// auditResponse withholds the response until its known rejection is durable.
// Successful/replayed operation outcomes are already journaled in their transaction.
type auditResponse struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	code, stage string
}

func (w *auditResponse) Header() http.Header { return w.header }
func (w *auditResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *auditResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.body.Write(p)
}

func (h *Handler) auditedMutation(w http.ResponseWriter, r *http.Request, claims *accessClaims) {
	driver := h.driver.(store.ShrimpAuditDriver)
	attempt, err := driver.BeginShrimpAudit(r.Context(), claims.Subject, h.config.Authority, "mutation")
	if err != nil {
		h.problem(w, 503, "audit_unavailable", "acceptance")
		return
	}
	buffer := &auditResponse{header: make(http.Header)}
	h.authenticated(buffer, r.WithContext(store.WithShrimpAudit(r.Context(), attempt)), claims)
	attempt.Stage, attempt.Code = buffer.stage, buffer.code
	if buffer.status >= 400 && buffer.status < 500 {
		attempt.Commit = "not_committed"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := driver.FinishShrimpAudit(ctx, *attempt); err != nil {
		h.problem(w, 503, "audit_unavailable", "publication")
		return
	}
	for name, values := range buffer.header {
		w.Header()[name] = values
	}
	w.WriteHeader(buffer.status)
	_, _ = w.Write(buffer.body.Bytes())
}

// OperationalAuditHandler is only mounted by the loopback pilot launcher. Its
// independently generated secret permits a fixed redacted resource-wide view;
// provisioning credentials and the fault-control credential confer no access.
func (h *Handler) OperationalAuditHandler(secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.TLS == nil || len(secret) < 32 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1 {
			h.problem(w, 401, "audit_authority_required", "authorization")
			return
		}
		if r.Method != http.MethodGet {
			h.problem(w, 405, "read_only", "read")
			return
		}
		query := r.URL.Query()
		for key, values := range query {
			if len(values) != 1 || (key != "epoch" && key != "after" && key != "limit") {
				h.problem(w, 400, "invalid_cursor", "read")
				return
			}
		}
		after, limit := int64(0), 100
		var err error
		if query.Has("after") {
			after, err = strconv.ParseInt(query.Get("after"), 10, 64)
		}
		if err != nil {
			h.problem(w, 400, "invalid_cursor", "read")
			return
		}
		if query.Has("limit") {
			limit, err = strconv.Atoi(query.Get("limit"))
		}
		if err != nil {
			h.problem(w, 400, "invalid_cursor", "read")
			return
		}
		page, err := h.driver.(store.ShrimpAuditDriver).InspectShrimpAudit(r.Context(), query.Get("epoch"), after, limit)
		if err != nil {
			if err.Error() == "invalid_cursor" {
				h.problem(w, 400, "invalid_cursor", "read")
			} else {
				h.problem(w, 503, "audit_unavailable", "read")
			}
			return
		}
		send(w, 200, map[string]any{
			"scope": h.scope(), "journal": page,
			"coverage": map[string]any{
				"complete":              false,
				"surface":               "authenticated SHRIMP mutation submissions and native UpdateUser/DeleteUser calls in this pilot",
				"restrictions":          []string{"no unauthenticated traffic or ordinary reads", "no native signup, settings, binding or trust administration", "no backup restoration continuity", "no raw values, credentials, request bodies or receipts"},
				"min_retention_seconds": 604800, "collection": "none during the retained pilot deployment",
				"max_attempts": 100000, "publication": "synchronous local SQLite journal; unresolved attempts remain visible",
			},
		})
	})
}
