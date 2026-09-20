package server

import (
	"bytes"
	"context"
	"net/http"
	"time"
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
	ctx, err := h.driver.BeginAudit(r.Context(), claims.Subject, h.config.Authority, "mutation")
	if err != nil {
		h.problem(w, 503, "audit_unavailable", "acceptance")
		return
	}
	buffer := &auditResponse{header: make(http.Header)}
	h.authenticated(buffer, r.WithContext(ctx), claims)
	outcome := AuditOutcome{Stage: buffer.stage, Code: buffer.code}
	if buffer.status >= 400 && buffer.status < 500 {
		outcome.Commit = "not_committed"
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.driver.FinishAudit(finishCtx, outcome); err != nil {
		h.problem(w, 503, "audit_unavailable", "publication")
		return
	}
	for name, values := range buffer.header {
		w.Header()[name] = values
	}
	w.WriteHeader(buffer.status)
	_, _ = w.Write(buffer.body.Bytes())
}
