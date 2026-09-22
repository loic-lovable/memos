package shrimp

import (
	"crypto/subtle"
	"net/http"
	"strconv"

	"github.com/usememos/memos/store"
)

// OperationalAuditHandler is explicitly enabled on the loopback application. Its
// independently generated secret permits a fixed redacted resource-wide view;
// provisioning credentials and the fault-control credential confer no access.
func (h *Handler) OperationalAuditHandler(secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.TLS == nil || len(r.Header.Values("Authorization")) != 1 || len(secret) < 32 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1 {
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
				"surface":               "authenticated SHRIMP mutations, native UpdateUser/DeleteUser, refused authenticated signup/provider/link/authentication-setting changes, and local provisioning-policy changes",
				"restrictions":          []string{"no unauthenticated traffic or ordinary reads", "no successful native signup or provider/link/authentication-setting changes after enrollment; other settings are outside this surface", "no backup restoration continuity", "no raw values, credentials, request bodies or receipts"},
				"min_retention_seconds": 604800, "collection": "settled attempts older than seven days may be collected at capacity; unresolved and retained-result references are preserved",
				"max_attempts": 100000, "max_operation_results": 10000, "publication": "synchronous local SQLite journal; unresolved attempts remain visible",
			},
		})
	})
}
